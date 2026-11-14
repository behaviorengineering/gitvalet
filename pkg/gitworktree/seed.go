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
	// Dirs are globs of the folders to clone, such as "node_modules" or "packages/*/node_modules". They must name
	// folders inside Base, never Base itself; matches named .git, and the folder that holds the worktrees, are
	// skipped.
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

// Files the package keeps inside a seed's folders, which are git-ignored by definition.
const (
	// A folder moved aside while its clone takes its place ends in oldSuffix.
	oldSuffix = ".gitworktree-old"
	// StampFile, inside a seed's first folder, holds the fingerprint of the inputs the folders were made from.
	StampFile = ".gitworktree-stamp"
)

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
	for _, g := range s.Dirs {
		if filepath.Clean(g) == "." {
			return fmt.Errorf("%s: Dirs %q names Base itself, not a folder inside it", s.Name, g)
		}
	}
	for _, g := range slices.Concat(s.Dirs, s.Inputs) {
		if _, err := filepath.Match(strings.TrimSuffix(g, "/**"), ""); err != nil || !filepath.IsLocal(g) {
			return fmt.Errorf("%s: %q is not a glob inside Base", s.Name, g)
		}
	}
	return nil
}

func (s Seed) base(checkout string) string { return filepath.Join(checkout, s.Base) }

// dirs lists the seed's folders that are in the checkout, relative to its base. It skips a match named .git, the
// base itself, and any folder that is, or holds, keep: the folder that holds the worktrees.
func (s Seed) dirs(checkout, keep string) ([]string, error) {
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
			if rel == "." || filepath.Base(rel) == ".git" || inside(keep, abs) || slices.Contains(found, rel) {
				continue
			}
			found = append(found, rel)
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

// stampPath is where the seed's stamp sits in a checkout whose seed folders are dirs: inside the first of them.
func (s Seed) stampPath(checkout string, dirs []string) string {
	return filepath.Join(s.base(checkout), dirs[0], StampFile)
}

// writeStamp records in the checkout's first seed folder the fingerprint of the inputs its folders were made from.
func (s Seed) writeStamp(checkout, keep string) error {
	dirs, err := s.dirs(checkout, keep)
	if err != nil {
		return err
	}
	if len(dirs) == 0 {
		return fmt.Errorf("gitworktree: seed %s has no folders in %s to stamp", s.Name, checkout)
	}
	fp, err := s.fingerprint(checkout)
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.stampPath(checkout, dirs), []byte(fp+"\n"), 0o600); err != nil {
		return fmt.Errorf("gitworktree: seed %s: write the stamp: %w", s.Name, err)
	}
	return nil
}

// match is what a seed knows about the main checkout's folders and one worktree.
type match struct {
	// same is true when the main checkout's folders were made from the inputs the worktree has.
	same bool
	// src are the main checkout's seed folders, relative to the seed's base.
	src []string
	// note says the main checkout's folders have no stamp, so its current inputs stood in for it.
	note string
}

// matches compares the worktree's inputs with what the main checkout's folders were made from: the stamp in them
// when there is one, and the main checkout's current inputs when there is not.
func (s Seed) matches(repo, worktree, keep string) (match, error) {
	src, err := s.dirs(repo, keep)
	if err != nil || len(src) == 0 {
		return match{}, err
	}
	m := match{src: src}
	stamp, err := os.ReadFile(s.stampPath(repo, src)) // #nosec G304 -- the stamp the package writes.
	var want string
	switch {
	case err == nil:
		want = strings.TrimSpace(string(stamp))
	case errors.Is(err, fs.ErrNotExist):
		m.note = s.Name + ": the main checkout's folders are unstamped, so its current inputs were trusted"
		if want, err = s.fingerprint(repo); err != nil {
			return match{}, err
		}
	default:
		return match{}, fmt.Errorf("gitworktree: seed %s: read the main checkout's stamp: %w", s.Name, err)
	}
	have, err := s.fingerprint(worktree)
	if err != nil {
		return match{}, err
	}
	m.same = want == have
	return m, nil
}

func (s Seed) stepClone() string   { return "clone the main checkout's " + s.Name }
func (s Seed) stepPrepare() string { return "prepare " + s.Name }

func (s Seed) reasonDiffers() string {
	return s.Name + ": its inputs differ from what the main checkout's were made from, or the main checkout has none"
}

// noneOfItsOwn is why Share leaves a seed alone in a worktree with none of its folders, or no reason when err says
// the folders could not be listed.
func noneOfItsOwn(s Seed, err error) string {
	if err != nil {
		return ""
	}
	return s.Name + ": it has none of its own"
}

func (s Seed) reasonNoClones() string { return s.Name + ": folders cannot be cloned here" }

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

// seedRun says what seed does with one seed in one worktree.
type seedRun struct {
	worktree string
	// add is true for a new worktree, which gets Prepare when its inputs differ; Share leaves such a seed alone.
	add bool
	// dryRun returns the steps without taking them.
	dryRun bool
}

// seed gives the worktree the seed's folders, or with dryRun says what it would do. On add, a worktree whose inputs
// differ gets the seed's Prepare step, over a clone when the seed asks for one. On share, it is left as it was, and so
// is a worktree with none of the seed's folders of its own. It returns the steps and why it took none, if it did not.
func (m *Manager) seed(ctx context.Context, s Seed, run seedRun) ([]string, string, error) {
	if !run.add {
		own, err := s.dirs(run.worktree, m.root)
		if err != nil || len(own) == 0 {
			return nil, noneOfItsOwn(s, err), err
		}
	}
	found, err := s.matches(m.cfg.Repo, run.worktree, m.root)
	if err != nil {
		return nil, "", err
	}
	if !found.same && (!run.add || s.Prepare == nil) {
		return nil, joinReason(found.note, s.reasonDiffers()), nil
	}
	var steps []string
	prepare := !found.same
	if (found.same || s.PrepareOverClone) && len(found.src) > 0 {
		cloned, reason, err := m.cloneSeed(ctx, s, found.src, run)
		switch {
		case err != nil:
			return nil, "", err
		case !cloned && run.add && s.Prepare != nil:
			// Where folders cannot be cloned, a new worktree prepares its own instead.
			prepare = true
		case !cloned:
			return nil, joinReason(found.note, reason), nil
		default:
			steps = append(steps, s.stepClone())
		}
	}
	if prepare {
		if err := m.prepare(ctx, s, run); err != nil {
			return steps, found.note, err
		}
		steps = append(steps, s.stepPrepare())
	}
	return steps, found.note, nil
}

// cloneSeed clones the main checkout's seed folders into the worktree, or with dryRun checks that it could. It
// reports false with a reason, and no error, where folders cannot be cloned.
func (m *Manager) cloneSeed(ctx context.Context, s Seed, src []string, run seedRun) (bool, string, error) {
	if run.dryRun {
		if !m.canClone(s.base(m.cfg.Repo), run.worktree) {
			return false, s.reasonNoClones(), nil
		}
		return true, "", nil
	}
	m.cfg.Logger.Info("cloning", "seed", s.Name, "from", s.base(m.cfg.Repo), "to", s.base(run.worktree))
	err := m.cloneAll(ctx, s, src, run.worktree)
	if errors.Is(err, ErrUnsupported) {
		return false, s.reasonNoClones(), nil
	}
	return err == nil, "", err
}

// prepare runs the seed's Prepare step in the worktree, then stamps the folders it made. A dry run does nothing.
func (m *Manager) prepare(ctx context.Context, s Seed, run seedRun) error {
	if run.dryRun {
		return nil
	}
	m.cfg.Logger.Info("preparing", "seed", s.Name, "worktree", run.worktree)
	if err := s.Prepare(ctx, run.worktree); err != nil {
		return fmt.Errorf("gitworktree: seed %s: prepare %s: %w", s.Name, run.worktree, err)
	}
	return s.writeStamp(run.worktree, m.root)
}

// cloneAll replaces each of the seed's folders in the worktree with a clone of the main checkout's.
func (m *Manager) cloneAll(ctx context.Context, s Seed, rels []string, worktree string) error {
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := replaceWithClone(filepath.Join(s.base(m.cfg.Repo), rel), filepath.Join(s.base(worktree), rel), m.clone)
		if err != nil {
			return fmt.Errorf("gitworktree: seed %s: %w", s.Name, err)
		}
	}
	return nil
}

// replaceWithClone puts a clone of src at dst, made by clone. The clone shares src's disk blocks until either copy
// changes a file. A folder already at dst is moved aside first, put back when the clone fails, and removed once it
// worked.
func replaceWithClone(src, dst string, clone func(src, dst string) error) error {
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
	if err := clone(src, dst); err != nil {
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
