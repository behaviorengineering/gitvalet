package gitworktree

import (
	"os"
	"path/filepath"
	"testing"
)

// writeBig writes a 1 MiB file that is not all zeros, so APFS gives it real blocks.
func (f *fixture) writeBig(path string) {
	f.t.Helper()
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) usage() map[string]Usage {
	f.t.Helper()
	us, err := f.mgr.Usage(ctx(f.t), UsageOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	byName := map[string]Usage{}
	for _, u := range us {
		byName[u.Name] = u
	}
	return byName
}

func TestUsageShowsClonesShareEveryBlock(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	big := app + "/node_modules/big/data.bin"
	f.writeBig(filepath.Join(f.repo, big))
	if own := f.usage()[MainName].Own; own < 1<<20 {
		t.Fatalf("before any clone, the main clone owns %d bytes, want at least 1 MiB", own)
	}

	path := f.add("feat/cloned").Path
	us := f.usage()
	main, wt := us[MainName], us["wt-cloned"]
	if wt.Files != main.Files || wt.Size < 1<<20 {
		t.Fatalf("the worktree has %d files and %d bytes, want the main clone's %d files", wt.Files, wt.Size, main.Files)
	}
	t.Logf("after cloning: size %d, own %d (main) and %d (worktree), shared %d", wt.Size, main.Own, wt.Own, wt.Shared)
	if main.Own != 0 || wt.Own != 0 {
		t.Fatalf("after cloning, own bytes: main %d, worktree %d; want 0 for both, every block shared", main.Own, wt.Own)
	}
	if wt.Shared != wt.Size {
		t.Errorf("shared = %d, want the whole size %d", wt.Shared, wt.Size)
	}

	// Rewriting the file, as an install does, gives the worktree its own copy of it, and only of it.
	f.writeBig(filepath.Join(path, big))
	us = f.usage()
	if own := us["wt-cloned"].Own; own < 1<<20 || own > 1<<20+64<<10 {
		t.Errorf("after rewriting one 1 MiB file, the worktree owns %d bytes, want about 1 MiB", own)
	}
	if own := us[MainName].Own; own < 1<<20 {
		t.Errorf("the main clone owns %d bytes, want its 1 MiB back as its own", own)
	}
}

func TestCloneCopiesOnlyTheBlockAWriteTouches(t *testing.T) {
	needsClones(t)
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	f := &fixture{t: t}
	f.writeBig(filepath.Join(src, "data.bin"))
	if err := cloneTree(src, dst); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dst, "data.bin"), os.O_WRONLY, 0) // #nosec G304 -- a test fixture.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("x"), 512<<10); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var own int64
	if err := scanTree(ctx(t), dst, func(b fileBlocks) { own += b.Private }); err != nil {
		t.Fatal(err)
	}
	t.Logf("a 1-byte write into a 1 MiB clone: the clone owns %d bytes", own)
	// One byte written in the middle of a 1 MiB clone takes one new block, not a new copy of the file.
	if own == 0 || own > 64<<10 {
		t.Errorf("the clone owns %d bytes after a 1-byte write, want one block", own)
	}
}

func TestUsageCountsClonesInsideOneCheckoutAsItsOwn(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	big := app + "/node_modules/big"
	f.writeBig(filepath.Join(f.repo, big, "data.bin"))
	path := f.add("feat/branded").Path
	// The worktree gets its own copy of the file, then clones it beside itself, as a build that clones an app bundle
	// under a new name does.
	f.writeBig(filepath.Join(path, big, "data.bin"))
	if err := cloneTree(filepath.Join(path, big), filepath.Join(path, big+"-twin")); err != nil {
		t.Fatal(err)
	}
	wt := f.usage()["wt-branded"]
	t.Logf("a 1 MiB file and its clone in one worktree: size %d, own %d, shared %d", wt.Size, wt.Own, wt.Shared)
	if wt.Size < 2<<20 {
		t.Errorf("size = %d, want both copies counted, as du counts them", wt.Size)
	}
	// Removing the worktree frees the pair's blocks, once.
	if wt.Own < 1<<20 || wt.Own > 1<<20+64<<10 {
		t.Errorf("own = %d, want about 1 MiB: the pair's blocks, counted once", wt.Own)
	}
}
