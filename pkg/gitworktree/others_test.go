package gitworktree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// otherOf makes a worktree outside the main checkout with git alone, as a build tool keeps one, with its own install,
// and names it in a new manager's Config.Others.
func (f *fixture) otherOf(name string) string {
	f.t.Helper()
	path := filepath.Join(filepath.Dir(f.repo), name, "work")
	f.git(f.repo, "worktree", "add", "-q", "--detach", path, tag)
	f.write(path, app+"/node_modules/left-pad/index.js", "its own install\n")
	f.cfg.Others = append(f.cfg.Others, Checkout{Name: name, Path: path})
	f.mgr = f.create(f.cfg)
	return path
}

func TestShareSwapsAnOtherCheckoutsOwnFoldersForClones(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.otherOf("runner")

	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Name != "runner" || outs[0].Path != path || !outs[0].Done {
		t.Fatalf("outcomes = %+v, want the runner's packages cloned", outs)
	}
	if outs[0].Head == "" {
		t.Error("the other checkout should carry the HEAD git lists for it")
	}
	if got := f.read(filepath.Join(path, app, "node_modules/left-pad/index.js")); got != "main's package\n" {
		t.Errorf("left-pad = %q, want the main checkout's", got)
	}
}

func TestShareFindsAnOtherCheckoutByNameAndPath(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.otherOf("everyday")
	f.worktreeOf("wt-own", "feat/own")

	for _, name := range []string{"everyday", path} {
		outs, err := f.mgr.Share(ctx(t), ShareOptions{Names: []string{name}, DryRun: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(outs) != 1 || outs[0].Name != "everyday" {
			t.Errorf("%s: outcomes = %+v, want the everyday checkout alone", name, outs)
		}
	}
}

func TestShareLeavesABusyOtherCheckout(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.otherOf("everyday")
	f.programs = fakePrograms{{PID: 99, Command: path + "/app/node_modules/electron/dist/Electron"}}

	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Done || outs[0].Reason != reasonBusy {
		t.Fatalf("outcome = %+v, want it kept for pid 99", outs[0])
	}
	if f.read(filepath.Join(path, app, "node_modules/left-pad/index.js")) != "its own install\n" {
		t.Error("a busy checkout's folders must stay as they were")
	}
}

func TestOtherCheckoutsAreSkippedUntilTheyExist(t *testing.T) {
	f := newFixture(t)
	f.cfg.Others = []Checkout{{Name: "preview", Path: filepath.Join(filepath.Dir(f.repo), "preview", "work")}}
	f.mgr = f.create(f.cfg)

	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil || len(outs) != 0 {
		t.Fatalf("outcomes = %+v, err = %v; want nothing for a checkout not made yet", outs, err)
	}
	if _, err := f.mgr.Share(ctx(t), ShareOptions{Names: []string{"preview"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a checkout not made yet", err)
	}
}

func TestUsageMeasuresOtherCheckouts(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.otherOf("runner")
	f.writeBig(filepath.Join(path, app, "node_modules/big/data.bin"))

	us := f.usage()
	if _, ok := us[MainName]; !ok {
		t.Fatal("usage should still lead with the main checkout")
	}
	if own := us["runner"].Own; own < 1<<20 {
		t.Errorf("the runner owns %d bytes, want at least its own 1 MiB install", own)
	}
}

func TestListFindRemoveAndPruneNeverSeeOtherCheckouts(t *testing.T) {
	f := newFixture(t)
	path := f.otherOf("runner")

	ws, err := f.mgr.List(ctx(t))
	if err != nil || len(ws) != 0 {
		t.Fatalf("List = %+v, err = %v; want no worktrees", ws, err)
	}
	for _, name := range []string{"runner", path} {
		if _, err := f.mgr.Remove(ctx(t), RemoveOptions{Name: name}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Remove(%s) err = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the other checkout must still be there: %v", err)
	}
}

func TestCreateManagerRejectsBadOthers(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(repo), "runner-work")
	cases := map[string][]Checkout{
		"relative path":    {{Name: "runner", Path: "runner/work"}},
		"inside the repo":  {{Name: "runner", Path: filepath.Join(repo, "build")}},
		"holds the repo":   {{Name: "runner", Path: filepath.Dir(repo)}},
		"name with slash":  {{Name: "a/b", Path: outside}},
		"main's name":      {{Name: MainName, Path: outside}},
		"same name twice":  {{Name: "runner", Path: outside}, {Name: "runner", Path: outside + "-2"}},
		"no name":          {{Path: outside}},
		"the repo itself":  {{Name: "runner", Path: repo}},
		"inside the root":  {{Name: "runner", Path: filepath.Join(repo, "tmp", "x")}},
		"empty everything": {{}},
	}
	for name, others := range cases {
		if _, err := (Config{Repo: repo, Root: "tmp", Others: others}).CreateManager(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := (Config{Repo: repo, Root: "tmp", Others: []Checkout{{Name: "runner", Path: outside}}}).CreateManager(); err != nil {
		t.Errorf("a good other checkout: %v", err)
	}
}
