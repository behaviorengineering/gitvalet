package gitworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sync/errgroup"
)

// MainName names the main checkout in a usage report.
const MainName = "main checkout"

// scansAtOnce is how many checkouts Usage reads at the same time. More than four saved under a second on 13
// checkouts of a 2.6 GiB node_modules.
const scansAtOnce = 4

// Usage is the disk one checkout's seed folders take.
type Usage struct {
	Worktree
	Files int `json:"files"`
	// Size adds up each file's blocks on its own, as du does, so a clone counts in full wherever it is.
	Size int64 `json:"size_bytes"`
	// Own is what removing the checkout's seed folders would free: blocks no file outside them holds.
	Own int64 `json:"own_bytes"`
	// Shared is what a file outside the checkout also holds, such as a clone in another checkout, counted once.
	Shared int64 `json:"shared_bytes"`
}

// fileBlocks is what APFS reports about one file's blocks.
type fileBlocks struct {
	// Alloc is the bytes the file takes on disk, counted on its own.
	Alloc int64
	// Private is the bytes no clone or snapshot shares: what deleting this file alone would free.
	Private int64
	// CloneID is the same for files that are full clones of each other, and Clones counts those files, this one
	// included. Both are 0 when the volume does not track clones.
	CloneID uint64
	Clones  uint32
}

// cloneSet is the files in one checkout that are full clones of each other.
type cloneSet struct {
	size  int64
	seen  uint32
	total uint32
}

// UsageOptions say which checkouts to measure.
type UsageOptions struct {
	// Names picks worktrees by folder name, branch or path; empty means the main checkout and every worktree.
	Names []string
}

// Usage measures, for the main checkout and each worktree, how much of its seed folders' disk is its own and how
// much it shares with files outside it. It reads each file's blocks from APFS, so the numbers hold however busy the
// disk is. On a system without APFS it returns ErrUnsupported.
func (m *Manager) Usage(ctx context.Context, opts UsageOptions) (usages []Usage, err error) {
	if ctx == nil {
		return nil, errors.New("gitworktree: nil context")
	}
	ctx, span := m.tracer.Start(ctx, "gitworktree.usage")
	defer func() { endSpan(span, err) }()

	targets, err := m.pick(ctx, opts.Names)
	if err != nil {
		return nil, err
	}
	if len(opts.Names) == 0 {
		branch, err := m.cfg.Git.RunTrim(ctx, m.cfg.Repo, "branch", "--show-current")
		if err != nil {
			return nil, fmt.Errorf("gitworktree: read the main checkout's branch: %w", err)
		}
		targets = append([]Worktree{{Name: MainName, Path: m.cfg.Repo, Branch: branch}}, targets...)
	}
	usages = make([]Usage, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(scansAtOnce)
	for i, w := range targets {
		g.Go(func() error {
			u, err := m.usageOf(gctx, w)
			usages[i] = u
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return usages, nil
}

// usageOf measures one checkout's seed folders.
func (m *Manager) usageOf(ctx context.Context, w Worktree) (Usage, error) {
	u := Usage{Worktree: w}
	var dirs []string
	for _, s := range m.cfg.Seeds {
		rels, err := s.dirs(w.Path)
		if err != nil {
			return u, err
		}
		for _, rel := range rels {
			if dir := filepath.Join(s.base(w.Path), rel); !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	sets := map[uint64]*cloneSet{}
	for _, dir := range dirs {
		// A seed folder that is a symbolic link holds no files of this checkout's, and following it would count the
		// target's twice.
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			continue
		}
		if err := scanTree(ctx, dir, func(f fileBlocks) { u.add(f, sets) }); err != nil {
			return u, fmt.Errorf("gitworktree: measure %s: %w", dir, err)
		}
	}
	// A set of clones that all sit in this checkout, such as an app bundle and its renamed clone, is freed with it.
	for _, set := range sets {
		if set.seen >= set.total {
			u.Own += set.size
		} else {
			u.Shared += set.size
		}
	}
	return u, nil
}

// add counts one file. A file that is a full clone joins its set, which is counted once when the scan ends.
func (u *Usage) add(f fileBlocks, sets map[uint64]*cloneSet) {
	u.Files++
	u.Size += f.Alloc
	if f.Clones > 1 && f.Private == 0 {
		set := sets[f.CloneID]
		if set == nil {
			set = &cloneSet{size: f.Alloc, total: f.Clones}
			sets[f.CloneID] = set
		}
		set.seen++
		return
	}
	u.Own += f.Private
	if f.Alloc > f.Private {
		u.Shared += f.Alloc - f.Private
	}
}
