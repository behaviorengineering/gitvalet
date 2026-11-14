package gitworktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Seed is one group of ignored folders a worktree can clone from the main checkout, such as a Node project's
// node_modules. The clone is usable when the files the folders come from, the seed's inputs, match the main
// checkout's.
type Seed struct {
	// Name names the seed in steps and reasons, such as "packages".
	Name string
	// Base is the folder in a checkout that Dirs and Inputs are relative to. Empty means the checkout's top.
	Base string
	// Dirs are globs of the folders to clone, such as "node_modules" or "packages/*/node_modules".
	Dirs []string
	// Inputs are globs of the files whose bytes must match for the clone to be usable, such as "package-lock.json".
	// A glob ending in "/**" matches every file under that folder.
	Inputs []string
	// Fingerprint, when set, replaces Inputs, for an input that is only part of a file, such as one field of
	// package.json. It gets the seed's base folder in one checkout and returns a value to compare.
	Fingerprint func(base string) (string, error)
	// Prepare, when set, makes the folders itself when the inputs differ, such as by running an install. It gets the
	// worktree's top folder. Without it, a worktree whose inputs differ gets no folders for this seed.
	Prepare func(ctx context.Context, worktree string) error
	// PrepareOverClone clones the main checkout's folders before Prepare runs, for an installer that only rewrites
	// what changed, such as yarn or npm. Without it, Prepare starts from no folders.
	PrepareOverClone bool
}

// oldSuffix marks a folder moved aside while its clone takes its place.
const oldSuffix = ".gitworktree-old"

// cloneFolder is cloneTree, swapped in tests to act as a system that cannot clone.
var cloneFolder = cloneTree

func (s Seed) check() error {
	switch {
	case s.Name == "":
		return errors.New("a seed needs a Name")
	case len(s.Dirs) == 0:
		return fmt.Errorf("%s: Dirs is required", s.Name)
	case len(s.Inputs) == 0 && s.Fingerprint == nil:
		return fmt.Errorf("%s: Inputs or Fingerprint is required, or a clone could never be told stale", s.Name)
	case s.Base != "" && !filepath.IsLocal(s.Base):
		return fmt.Errorf("%s: Base %q must be a folder inside the checkout", s.Name, s.Base)
	}
	for _, g := range slices.Concat(s.Dirs, s.Inputs) {
		if _, err := filepath.Match(strings.TrimSuffix(g, "/**"), ""); err != nil || !filepath.IsLocal(g) {
			return fmt.Errorf("%s: %q is not a glob inside Base", s.Name, g)
		}
	}
	return nil
}

func (s Seed) base(checkout string) string { return filepath.Join(checkout, s.Base) }

// dirs lists the seed's folders that are in the checkout, relative to its base.
func (s Seed) dirs(checkout string) ([]string, error) {
	base := s.base(checkout)
	var found []string
	for _, g := range s.Dirs {
		matches, err := filepath.Glob(filepath.Join(base, g))
		if err != nil {
			return nil, fmt.Errorf("gitworktree: seed %s: match %s: %w", s.Name, g, err)
		}
		for _, abs := range matches {
			rel, err := filepath.Rel(base, abs)
			if err != nil {
				return nil, fmt.Errorf("gitworktree: seed %s: name %s: %w", s.Name, abs, err)
			}
			if !slices.Contains(found, rel) {
				found = append(found, rel)
			}
		}
	}
	return found, nil
}

// fingerprint is what decides whether the seed's folders in one checkout can stand in for another's.
func (s Seed) fingerprint(checkout string) (string, error) {
	if s.Fingerprint != nil {
		v, err := s.Fingerprint(s.base(checkout))
		if err != nil {
			return "", fmt.Errorf("gitworktree: seed %s: fingerprint %s: %w", s.Name, checkout, err)
		}
		return v, nil
	}
	files, err := s.inputFiles(checkout)
	if err != nil {
		return "", err
	}
	base := s.base(checkout)
	sum := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(base, rel)) // #nosec G304 -- an input file the seed names, inside the checkout.
		if err != nil {
			return "", fmt.Errorf("gitworktree: seed %s: read %s: %w", s.Name, rel, err)
		}
		sum.Write([]byte(rel))
		sum.Write([]byte{0})
		sum.Write(data)
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// inputFiles lists the files the seed's Inputs match in the checkout, relative to its base and sorted.
func (s Seed) inputFiles(checkout string) ([]string, error) {
	base := s.base(checkout)
	var files []string
	for _, g := range s.Inputs {
		found, err := filesMatching(base, g)
		if err != nil {
			return nil, fmt.Errorf("gitworktree: seed %s: match %s: %w", s.Name, g, err)
		}
		files = append(files, found...)
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// filesMatching lists the regular files under base that glob matches, relative to base. A glob ending in "/**"
// matches every file under that folder, and a folder that is not there matches nothing.
func filesMatching(base, glob string) ([]string, error) {
	var files []string
	if under, ok := strings.CutSuffix(glob, "/**"); ok {
		err := filepath.WalkDir(filepath.Join(base, under), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			files = append(files, path[len(base)+1:])
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return files, err
	}
	matches, err := filepath.Glob(filepath.Join(base, glob))
	if err != nil {
		return nil, err
	}
	for _, abs := range matches {
		if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() {
			files = append(files, abs[len(base)+1:])
		}
	}
	return files, nil
}

// matches reports whether the main checkout has the seed's folders and the worktree's inputs match its own, so the
// main checkout's folders can stand in for the worktree's.
func (s Seed) matches(repo, worktree string) (bool, []string, error) {
	src, err := s.dirs(repo)
	if err != nil || len(src) == 0 {
		return false, nil, err
	}
	want, err := s.fingerprint(repo)
	if err != nil {
		return false, nil, err
	}
	have, err := s.fingerprint(worktree)
	if err != nil {
		return false, nil, err
	}
	return want == have, src, nil
}

func (s Seed) stepClone() string   { return "clone the main checkout's " + s.Name }
func (s Seed) stepPrepare() string { return "prepare " + s.Name }

func (s Seed) reasonDiffers() string {
	return s.Name + ": its inputs differ from the main checkout's, or the main checkout has none"
}

func (s Seed) plannedOnAdd() string {
	switch {
	case s.Prepare != nil && s.PrepareOverClone:
		return s.stepClone() + ", then " + s.stepPrepare() + " over the clone when the inputs differ"
	case s.Prepare != nil:
		return s.stepClone() + ", or " + s.stepPrepare() + " when the inputs differ"
	default:
		return s.stepClone() + " when the inputs match"
	}
}

// planShare says what Share would do with this seed in the worktree, changing nothing.
func (s Seed) planShare(repo, worktree string) ([]string, string, error) {
	own, err := s.dirs(worktree)
	if err != nil {
		return nil, "", err
	}
	if len(own) == 0 {
		return nil, s.Name + ": it has none of its own", nil
	}
	same, _, err := s.matches(repo, worktree)
	if err != nil || !same {
		return nil, s.reasonDiffers(), err
	}
	return []string{s.stepClone()}, "", nil
}

// seed gives the worktree the seed's folders. On add, a worktree whose inputs differ gets the seed's Prepare step,
// over a clone when the seed asks for one. On share, it is left as it was, and so is a worktree with none of the
// seed's folders of its own. It returns the steps it took and why it took none, if it did not.
func (m *Manager) seed(ctx context.Context, s Seed, worktree string, add bool) ([]string, string, error) {
	if !add {
		own, err := s.dirs(worktree)
		if err != nil || len(own) == 0 {
			return nil, s.Name + ": it has none of its own", err
		}
	}
	same, src, err := s.matches(m.cfg.Repo, worktree)
	if err != nil {
		return nil, "", err
	}
	if !same && (!add || s.Prepare == nil) {
		return nil, s.reasonDiffers(), nil
	}
	var steps []string
	prepare := !same
	if (same || s.PrepareOverClone) && len(src) > 0 {
		m.cfg.Logger.Info("cloning", "seed", s.Name, "from", s.base(m.cfg.Repo), "to", s.base(worktree))
		err := m.cloneAll(ctx, s, src, worktree)
		switch {
		case errors.Is(err, ErrUnsupported) && add && s.Prepare != nil:
			// Where folders cannot be cloned, a new worktree prepares its own instead.
			prepare = true
		case errors.Is(err, ErrUnsupported):
			return nil, s.Name + ": this file system cannot clone folders", nil
		case err != nil:
			return nil, "", err
		default:
			steps = append(steps, s.stepClone())
		}
	}
	if prepare {
		m.cfg.Logger.Info("preparing", "seed", s.Name, "worktree", worktree)
		if err := s.Prepare(ctx, worktree); err != nil {
			return steps, "", fmt.Errorf("gitworktree: seed %s: prepare %s: %w", s.Name, worktree, err)
		}
		steps = append(steps, s.stepPrepare())
	}
	return steps, "", nil
}

// cloneAll replaces each of the seed's folders in the worktree with a clone of the main checkout's.
func (m *Manager) cloneAll(ctx context.Context, s Seed, rels []string, worktree string) error {
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := replaceWithClone(filepath.Join(s.base(m.cfg.Repo), rel), filepath.Join(s.base(worktree), rel)); err != nil {
			return fmt.Errorf("gitworktree: seed %s: %w", s.Name, err)
		}
	}
	return nil
}

// replaceWithClone puts a clone of src at dst. The clone shares src's disk blocks until either copy changes a file. A
// folder already at dst is moved aside first, put back when the clone fails, and removed once it worked.
func replaceWithClone(src, dst string) error {
	old := dst + oldSuffix
	// A folder left aside by a run that died part way is ours to remove.
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("remove %s, left aside by an earlier run: %w", old, err)
	}
	movedAside := false
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return fmt.Errorf("move %s aside: %w", dst, err)
		}
		movedAside = true
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("create the parent of %s: %w", dst, err)
	}
	if err := cloneFolder(src, dst); err != nil {
		if movedAside {
			if rerr := os.Rename(old, dst); rerr != nil {
				return fmt.Errorf("clone %s, and put back %s: %w", src, old, errors.Join(err, rerr))
			}
		}
		return fmt.Errorf("clone %s to %s: %w", src, dst, err)
	}
	if movedAside {
		if err := os.RemoveAll(old); err != nil {
			return fmt.Errorf("remove the old %s: %w", old, err)
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
