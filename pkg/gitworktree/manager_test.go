package gitworktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/behaviorengineering/gitvalet/pkg/pullrequest"
)

// fakePrograms is the running programs a test sets.
type fakePrograms []Program

func (f *fakePrograms) Programs(context.Context) ([]Program, error) { return *f, nil }

// fakePRs answers NewestPR from a map of branch to pull request.
type fakePRs map[string]pullrequest.PR

func (f fakePRs) NewestPR(_ context.Context, branch string) (pullrequest.PR, bool, error) {
	pr, ok := f[branch]
	return pr, ok, nil
}

// app is the seeds' base folder, the way a Node project often sits below a repository's top.
const app = "app"

// tag is where new branches start in these tests, fetched from origin as a CI tag would be.
const tag = "last-green"

type fixture struct {
	t        *testing.T
	origin   string
	repo     string
	programs fakePrograms
	prepared []string
	mgr      *Manager
	cfg      Config
}

// newFixture makes an origin with a tag, and a main checkout whose app has packages and plugins installed.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, origin: filepath.Join(root, "origin"), repo: filepath.Join(root, "clone")}
	f.git(root, "init", "-q", "-b", "main", f.origin)
	f.write(f.origin, app+"/yarn.lock", "lock 1\n")
	f.write(f.origin, app+"/package.json", `{"plugins": {"a": "https://example.com/a-1.vsix"}}`)
	f.write(f.origin, app+"/applications/electron/package.json", "{}\n")
	f.write(f.origin, app+"/patches/x.patch", "patch 1\n")
	f.write(f.origin, ".gitignore", "node_modules/\n.browser_modules/\nplugins/\ntmp/\n")
	f.git(f.origin, "add", "-A")
	f.git(f.origin, "commit", "-q", "-m", "first")
	f.git(f.origin, "tag", tag)
	f.git(root, "clone", "-q", f.origin, f.repo)
	f.write(f.repo, app+"/node_modules/left-pad/index.js", "main's package\n")
	f.write(f.repo, app+"/.browser_modules/keytar/index.js", "main's browser module\n")
	f.write(f.repo, app+"/applications/electron/node_modules/.bin/tool", "main's tool\n")
	f.write(f.repo, app+"/plugins/a/package.json", "main's plugin\n")

	f.cfg = Config{
		Repo:      f.repo,
		Root:      "tmp",
		Prefix:    "wt-",
		BaseRef:   tag,
		BaseFetch: "+refs/tags/" + tag + ":refs/tags/" + tag,
		Seeds:     []Seed{f.packagesSeed(), f.pluginsSeed()},
		Programs:  &f.programs,
	}
	f.mgr = f.create(f.cfg)
	return f
}

// packagesSeed installs over a clone, as yarn install does.
func (f *fixture) packagesSeed() Seed {
	return Seed{
		Name:             "packages",
		Base:             app,
		Dirs:             []string{"node_modules", ".browser_modules", "applications/*/node_modules"},
		Inputs:           []string{"yarn.lock", "package.json", "applications/*/package.json", "patches/**"},
		Prepare:          f.prepare("packages", "node_modules"),
		PrepareOverClone: true,
	}
}

// pluginsSeed matches on one field of package.json, and downloads into an empty folder when it differs.
func (f *fixture) pluginsSeed() Seed {
	return Seed{
		Name: "plugins",
		Base: app,
		Dirs: []string{"plugins"},
		Fingerprint: func(base string) (string, error) {
			data, err := os.ReadFile(filepath.Join(base, "package.json")) // #nosec G304 -- a test fixture.
			if err != nil {
				return "", err
			}
			var pkg struct {
				Plugins json.RawMessage `json:"plugins"`
			}
			err = json.Unmarshal(data, &pkg)
			return string(pkg.Plugins), err
		},
		Prepare: f.prepare("plugins", "plugins"),
	}
}

// prepare stands in for an install: it records the call and writes a marker into dir.
func (f *fixture) prepare(name, dir string) func(context.Context, string) error {
	return func(_ context.Context, worktree string) error {
		f.prepared = append(f.prepared, name)
		target := filepath.Join(worktree, app, dir)
		if err := os.MkdirAll(target, 0o750); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, "prepared-here"), []byte("yes"), 0o600)
	}
}

func (f *fixture) create(cfg Config) *Manager {
	f.t.Helper()
	mgr, err := cfg.CreateManager()
	if err != nil {
		f.t.Fatal(err)
	}
	return mgr
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed git calls in a test repo.
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(dir, rel, text string) {
	f.t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a test file.
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// add makes a worktree, failing the test when it errs.
func (f *fixture) add(branch string) Outcome {
	f.t.Helper()
	out, err := f.mgr.Add(ctx(f.t), AddOptions{Branch: branch})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// worktreeOf makes a worktree with git alone, as an agent did before this package.
func (f *fixture) worktreeOf(name, branch string) string {
	f.t.Helper()
	path := filepath.Join(f.repo, "tmp", name)
	f.git(f.repo, "worktree", "add", "-q", "-b", branch, path, tag)
	return path
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return c
}

// needsClones skips a test that clones folders where the system cannot.
func needsClones(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("cloning folders needs APFS on macOS")
	}
}

func TestAddClonesEverySeedWhenInputsMatch(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	out := f.add("feat/tidy-panel")

	want := filepath.Join(f.repo, "tmp", "wt-tidy-panel")
	if out.Path != want {
		t.Fatalf("path = %s, want %s", out.Path, want)
	}
	for _, rel := range []string{
		"node_modules/left-pad/index.js", ".browser_modules/keytar/index.js",
		"applications/electron/node_modules/.bin/tool", "plugins/a/package.json",
	} {
		if got := f.read(filepath.Join(want, app, rel)); !strings.HasPrefix(got, "main's") {
			t.Errorf("%s = %q, want the main checkout's copy", rel, got)
		}
	}
	if len(f.prepared) != 0 {
		t.Errorf("prepared %v, want nothing: the inputs match", f.prepared)
	}
	if got := f.git(want, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat/tidy-panel" {
		t.Errorf("branch = %s", got)
	}
	if got, base := f.git(want, "rev-parse", "HEAD"), f.git(f.repo, "rev-parse", tag); got != base {
		t.Errorf("new branch starts at %s, want %s %s", got, tag, base)
	}
	wantSteps := []string{
		stepFetch + f.cfg.BaseFetch, stepCreate + " on a new branch from " + tag,
		"clone the main checkout's packages", "clone the main checkout's plugins",
	}
	if !slices.Equal(out.Steps, wantSteps) {
		t.Errorf("steps = %q, want %q", out.Steps, wantSteps)
	}
}

func TestAddPreparesOverTheCloneWhenTheLockfileDiffers(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	f.git(f.repo, "checkout", "-q", "-b", "feat/new-dep")
	f.write(f.repo, app+"/yarn.lock", "lock 2\n")
	f.git(f.repo, "commit", "-q", "-am", "new dep")
	f.git(f.repo, "checkout", "-q", "main")
	// The main checkout is back on lock 1, the lockfile its packages came from.

	out := f.add("feat/new-dep")
	if !slices.Equal(f.prepared, []string{"packages"}) {
		t.Fatalf("prepared %v, want the packages alone", f.prepared)
	}
	modules := filepath.Join(out.Path, app, "node_modules")
	if f.read(filepath.Join(modules, "left-pad/index.js")) != "main's package\n" {
		t.Error("the install should run over a clone of the main checkout's packages")
	}
	if f.read(filepath.Join(modules, "prepared-here")) != "yes" {
		t.Error("the install did not run in the new worktree")
	}
}

func TestAddPreparesAnEmptyFolderWhenTheSeedDoesNotInstallOverAClone(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	f.git(f.repo, "checkout", "-q", "-b", "feat/new-plugin")
	f.write(f.repo, app+"/package.json", `{"plugins": {"a": "https://example.com/a-2.vsix"}}`)
	f.git(f.repo, "commit", "-q", "-am", "new plugin")
	f.git(f.repo, "checkout", "-q", "main")

	out := f.add("feat/new-plugin")
	// package.json is a packages input too, so both seeds prepare.
	if !slices.Equal(f.prepared, []string{"packages", "plugins"}) {
		t.Fatalf("prepared %v", f.prepared)
	}
	if _, err := os.Stat(filepath.Join(out.Path, app, "plugins/a")); !os.IsNotExist(err) {
		t.Error("the plugins seed should start from an empty folder, not the main checkout's plugins")
	}
}

func TestAddRefusesAnExistingFolder(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	f.add("feat/one")
	_, err := f.mgr.Add(ctx(t), AddOptions{Branch: "feat/other", Name: "one"})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
}

func TestAddDryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	out, err := f.mgr.Add(ctx(t), AddOptions{Branch: "feat/plan", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done || len(out.Steps) != 4 {
		t.Fatalf("outcome = %+v, want the fetch, the create and one line per seed", out)
	}
	if _, err := os.Stat(out.Path); !os.IsNotExist(err) {
		t.Error("a dry run must not make the folder")
	}
	if _, code := f.mgr.cfg.Git.AllowFail(ctx(t), f.repo, "rev-parse", "--verify", "refs/heads/feat/plan"); code == 0 {
		t.Error("a dry run must not make the branch")
	}
}

func TestShareSwapsOwnFoldersForClones(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.worktreeOf("wt-own", "feat/own")
	f.write(path, app+"/node_modules/left-pad/index.js", "its own install\n")
	f.write(path, app+"/node_modules/only-here/index.js", "stale\n")

	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || !outs[0].Done {
		t.Fatalf("outcomes = %+v", outs)
	}
	modules := filepath.Join(path, app, "node_modules")
	if got := f.read(filepath.Join(modules, "left-pad/index.js")); got != "main's package\n" {
		t.Errorf("left-pad = %q, want the main checkout's", got)
	}
	if _, err := os.Stat(filepath.Join(modules, "only-here")); !os.IsNotExist(err) {
		t.Error("the worktree's own packages should be gone")
	}
	if _, err := os.Stat(modules + oldSuffix); !os.IsNotExist(err) {
		t.Error("the folder moved aside should be removed")
	}
	if !strings.Contains(outs[0].Reason, "plugins: it has none of its own") {
		t.Errorf("reason = %q, want the plugins seed left alone", outs[0].Reason)
	}
}

func TestShareDecidesPerSeed(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.worktreeOf("wt-mixed", "feat/mixed")
	f.write(path, app+"/yarn.lock", "lock 2\n")
	f.write(path, app+"/node_modules/left-pad/index.js", "its own install\n")
	f.write(path, app+"/plugins/a/package.json", "its own plugin\n")

	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(outs[0].Steps, []string{"clone the main checkout's plugins"}) {
		t.Fatalf("steps = %q, want the plugins alone", outs[0].Steps)
	}
	if f.read(filepath.Join(path, app, "node_modules/left-pad/index.js")) != "its own install\n" {
		t.Error("packages with a different lockfile must stay the worktree's own")
	}
	if f.read(filepath.Join(path, app, "plugins/a/package.json")) != "main's plugin\n" {
		t.Error("plugins with the same list should be the main checkout's clone")
	}
}

func TestShareLeavesABusyWorktree(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	busy := f.add("feat/busy").Path
	f.programs = fakePrograms{{PID: 4242, Command: "node " + busy + "/app/node_modules/.bin/mocha"}}

	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Done || outs[0].Reason != reasonBusy || outs[0].Users[0].PID != 4242 {
		t.Fatalf("outcome = %+v, want it kept for pid 4242", outs[0])
	}
}

func TestShareSeesAProgramWorkingInside(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	path := f.add("feat/shell").Path
	f.programs = fakePrograms{{PID: 77, Command: "-zsh", Dir: path + "/app"}}
	outs, err := f.mgr.Share(ctx(t), ShareOptions{Names: []string{"shell"}})
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Done || len(outs[0].Users) != 1 || outs[0].Users[0].PID != 77 {
		t.Fatalf("outcome = %+v, want it kept for pid 77", outs[0])
	}
}

func TestShareLeavesAWorktreeWithNoFoldersOfItsOwn(t *testing.T) {
	f := newFixture(t)
	path := f.worktreeOf("wt-bare", "feat/bare")
	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Done {
		t.Fatalf("outcome = %+v, want nothing done", outs[0])
	}
	if _, err := os.Stat(filepath.Join(path, app, "node_modules")); !os.IsNotExist(err) {
		t.Error("share must not add folders to a worktree that had none")
	}
}

func TestRemoveKeepsTheBranch(t *testing.T) {
	f := newFixture(t)
	f.worktreeOf("wt-done", "feat/done")
	if _, err := f.mgr.Remove(ctx(t), RemoveOptions{Name: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "tmp", "wt-done")); !os.IsNotExist(err) {
		t.Error("the worktree folder should be gone")
	}
	f.git(f.repo, "rev-parse", "--verify", "refs/heads/feat/done")
}

func TestRemoveRefusesUncommittedChanges(t *testing.T) {
	f := newFixture(t)
	path := f.worktreeOf("wt-dirty", "feat/dirty")
	f.write(path, app+"/yarn.lock", "edited\n")
	out, err := f.mgr.Remove(ctx(t), RemoveOptions{Name: "feat/dirty"})
	if !errors.Is(err, ErrKept) {
		t.Fatalf("err = %v, want ErrKept", err)
	}
	if !strings.Contains(out.Reason, "uncommitted") {
		t.Errorf("reason = %q", out.Reason)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the worktree should still be there")
	}
}

func TestRemoveFindsNothingByAWrongName(t *testing.T) {
	f := newFixture(t)
	if _, err := f.mgr.Remove(ctx(t), RemoveOptions{Name: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPruneRemovesClosedPullRequestsOnly(t *testing.T) {
	f := newFixture(t)
	head := func(path string) string { return f.git(path, "rev-parse", "HEAD") }
	merged := f.worktreeOf("wt-merged", "feat/merged")
	closed := f.worktreeOf("wt-closed", "feat/closed")
	moved := f.worktreeOf("wt-moved-on", "feat/moved-on")
	f.worktreeOf("wt-open", "feat/open")
	f.worktreeOf("wt-no-pr", "feat/no-pr")
	busy := f.worktreeOf("wt-busy-merged", "feat/busy-merged")
	f.programs = fakePrograms{{PID: 99, Command: "node " + busy + "/x.js"}}

	f.cfg.PRs = fakePRs{
		"feat/merged":       {Number: 1, State: "merged", Merged: true, HeadSHA: head(merged)},
		"feat/closed":       {Number: 2, State: "closed", HeadSHA: head(closed)},
		"feat/moved-on":     {Number: 3, State: "merged", Merged: true, HeadSHA: "a-commit-before-the-last"},
		"feat/open":         {Number: 4, State: "open"},
		"feat/busy-merged":  {Number: 5, State: "merged", Merged: true, HeadSHA: head(busy)},
		"feat/not-a-branch": {Number: 6, State: "closed"},
	}
	outs, err := f.create(f.cfg).Prune(ctx(t), PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, o := range outs {
		if o.Done {
			removed = append(removed, o.Name)
		}
	}
	slices.Sort(removed)
	if want := []string{"wt-closed", "wt-merged", "wt-moved-on"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	branches := strings.Split(f.git(f.repo, "branch", "--format=%(refname:short)"), "\n")
	if slices.Contains(branches, "feat/merged") {
		t.Error("the merged branch should be deleted")
	}
	for _, keep := range []string{"feat/closed", "feat/moved-on", "feat/open", "feat/busy-merged"} {
		if !slices.Contains(branches, keep) {
			t.Errorf("branch %s should stay", keep)
		}
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Error("the moved-on worktree should be removed")
	}
	if _, err := os.Stat(busy); err != nil {
		t.Error("a worktree in use should stay")
	}
}

func TestPruneDryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	merged := f.worktreeOf("wt-merged", "feat/merged")
	f.cfg.PRs = fakePRs{"feat/merged": {Number: 1, State: "merged", Merged: true, HeadSHA: f.git(merged, "rev-parse", "HEAD")}}
	outs, err := f.create(f.cfg).Prune(ctx(t), PruneOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !outs[0].Done || len(outs[0].Steps) != 2 {
		t.Fatalf("outcome = %+v, want it marked for removal and its branch for deletion", outs[0])
	}
	if _, err := os.Stat(merged); err != nil {
		t.Error("a dry run must not remove the worktree")
	}
}

func TestPruneNeedsAPRFinderAndADeadline(t *testing.T) {
	f := newFixture(t)
	if _, err := f.mgr.Prune(ctx(t), PruneOptions{}); !errors.Is(err, ErrNoPRFinder) {
		t.Errorf("err = %v, want ErrNoPRFinder", err)
	}
	f.cfg.PRs = fakePRs{}
	if _, err := f.create(f.cfg).Prune(context.Background(), PruneOptions{}); !errors.Is(err, ErrMissingDeadline) {
		t.Errorf("err = %v, want ErrMissingDeadline", err)
	}
}

func TestCreateManagerRejectsBadConfig(t *testing.T) {
	repo := t.TempDir()
	good := Seed{Name: "packages", Dirs: []string{"node_modules"}, Inputs: []string{"package-lock.json"}}
	cases := map[string]Config{
		"no repo":                        {Root: "tmp"},
		"no root":                        {Repo: repo},
		"root outside":                   {Repo: repo, Root: "../elsewhere"},
		"absolute root":                  {Repo: repo, Root: "/tmp"},
		"prefix with slash":              {Repo: repo, Root: "tmp", Prefix: "wt/"},
		"seed without name":              {Repo: repo, Root: "tmp", Seeds: []Seed{{Dirs: good.Dirs, Inputs: good.Inputs}}},
		"seed without dirs":              {Repo: repo, Root: "tmp", Seeds: []Seed{{Name: "x", Inputs: good.Inputs}}},
		"seed with no way to tell stale": {Repo: repo, Root: "tmp", Seeds: []Seed{{Name: "x", Dirs: good.Dirs}}},
		"seed glob outside":              {Repo: repo, Root: "tmp", Seeds: []Seed{{Name: "x", Dirs: []string{"../y"}, Inputs: good.Inputs}}},
	}
	for name, cfg := range cases {
		if _, err := cfg.CreateManager(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := (Config{Repo: repo, Root: "tmp", Seeds: []Seed{good}}).CreateManager(); err != nil {
		t.Errorf("a good config: %v", err)
	}
}

func TestInputFilesWalksDoubleStarFolders(t *testing.T) {
	f := &fixture{t: t}
	dir := t.TempDir()
	f.write(dir, "patches/a.patch", "a")
	f.write(dir, "patches/deep/b.patch", "b")
	f.write(dir, "package.json", "{}")
	s := Seed{Name: "x", Dirs: []string{"node_modules"}, Inputs: []string{"package.json", "patches/**", "missing/**", "none.lock"}}
	files, err := s.inputFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"package.json", "patches/a.patch", "patches/deep/b.patch"}; !slices.Equal(files, want) {
		t.Fatalf("files = %q, want %q", files, want)
	}
}

func TestMentionsMatchesWholePaths(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{"node /r/tmp/wt-a/x.js", true},
		{"node /r/tmp/wt-a", true},
		{"node /r/tmp/wt-ab/x.js", false},
		{"node /r/tmp/wt-ab/x.js /r/tmp/wt-a/y.js", true},
		{"--user-data-dir='/r/tmp/wt-a'", true},
	}
	for _, c := range cases {
		if got := mentions(c.command, "/r/tmp/wt-a"); got != c.want {
			t.Errorf("mentions(%q) = %t, want %t", c.command, got, c.want)
		}
	}
}

func TestSystemProgramsFindsThisTest(t *testing.T) {
	list, err := SystemPrograms{}.Programs(ctx(t))
	if err != nil {
		t.Skipf("ps or lsof is not available here: %v", err)
	}
	for _, p := range list {
		if p.PID == os.Getpid() {
			return
		}
	}
	t.Fatalf("ps listed %d programs, none of them this test's pid %d", len(list), os.Getpid())
}

// withoutClones makes the test act as a system whose file system cannot clone folders, such as Linux on ext4.
func (f *fixture) withoutClones() {
	f.mgr.clone = func(string, string) error { return ErrUnsupported }
	f.mgr.canClone = func(string, string) bool { return false }
}

func TestAddPreparesEverySeedWhereFoldersCannotBeCloned(t *testing.T) {
	f := newFixture(t)
	f.withoutClones()
	out := f.add("feat/linux")
	if !slices.Equal(f.prepared, []string{"packages", "plugins"}) {
		t.Fatalf("prepared %v, want both seeds", f.prepared)
	}
	if f.read(filepath.Join(out.Path, app, "node_modules/prepared-here")) != "yes" {
		t.Error("the packages seed should have prepared its own folder")
	}
	if _, err := os.Stat(filepath.Join(out.Path, app, "node_modules/left-pad")); !os.IsNotExist(err) {
		t.Error("nothing of the main checkout's should have been copied")
	}
}

func TestShareLeavesFoldersWhereTheyCannotBeCloned(t *testing.T) {
	f := newFixture(t)
	path := f.worktreeOf("wt-own", "feat/own")
	f.write(path, app+"/node_modules/left-pad/index.js", "its own install\n")
	f.withoutClones()
	outs, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Done || !strings.Contains(outs[0].Reason, "cannot be cloned") {
		t.Fatalf("outcome = %+v, want it left with a reason", outs[0])
	}
	if f.read(filepath.Join(path, app, "node_modules/left-pad/index.js")) != "its own install\n" {
		t.Error("the worktree's own folder must be put back when the clone fails")
	}
}

func TestShareDryRunMatchesTheRealRunWhereFoldersCannotBeCloned(t *testing.T) {
	f := newFixture(t)
	path := f.worktreeOf("wt-own", "feat/own")
	f.write(path, app+"/node_modules/left-pad/index.js", "its own install\n")
	f.withoutClones()
	plan, err := f.mgr.Share(ctx(t), ShareOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	real, err := f.mgr.Share(ctx(t), ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Done != real[0].Done || !slices.Equal(plan[0].Steps, real[0].Steps) || plan[0].Reason != real[0].Reason {
		t.Fatalf("dry run %+v, real run %+v; want the same outcome", plan[0], real[0])
	}
}

func TestRemoveRefusesAnEmptyNameAndKeepsADetachedWorktree(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.repo, "tmp", "wt-detached")
	f.git(f.repo, "worktree", "add", "-q", "--detach", path, tag)
	for _, name := range []string{"", "  "} {
		if _, err := f.mgr.Remove(ctx(t), RemoveOptions{Name: name}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Remove(%q): err = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the detached worktree must stay")
	}
}

func TestSeedCheckRejectsDirsThatNameBaseItself(t *testing.T) {
	for _, d := range []string{".", "./", "x/.."} {
		s := Seed{Name: "x", Dirs: []string{d}, Inputs: []string{"a"}}
		if err := s.check(); err == nil {
			t.Errorf("Dirs %q: want an error", d)
		}
	}
}

func TestSeedDirsSkipGitAndTheWorktreeRoot(t *testing.T) {
	f := newFixture(t)
	f.worktreeOf("wt-x", "feat/x")
	s := Seed{Name: "everything", Dirs: []string{"*"}, Inputs: []string{".gitignore"}}
	dirs, err := s.dirs(f.repo, f.mgr.root)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dirs, ".git") || slices.Contains(dirs, "tmp") || !slices.Contains(dirs, app) {
		t.Fatalf("dirs = %q, want app without .git or tmp", dirs)
	}
}

func TestAddPreparesWhenTheMainCheckoutsStampIsStale(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	if err := f.mgr.Stamp(ctx(t), "packages"); err != nil {
		t.Fatal(err)
	}
	// The main checkout's lockfile changes without a reinstall: its packages still match the old stamp.
	f.write(f.repo, app+"/yarn.lock", "lock 2\n")
	f.git(f.repo, "commit", "-q", "-am", "new lock, not installed")

	out, err := f.mgr.Add(ctx(t), AddOptions{Branch: "feat/after-lock", From: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.prepared, "packages") {
		t.Fatalf("prepared %v, want packages: the clone came from the old lockfile", f.prepared)
	}
	stamp := f.read(filepath.Join(out.Path, app, "node_modules", StampFile))
	if stamp == f.read(filepath.Join(f.repo, app, "node_modules", StampFile)) {
		t.Error("the worktree's stamp should be its own install's, not the main checkout's")
	}
}

func TestAddSaysWhenTheMainCheckoutIsUnstamped(t *testing.T) {
	needsClones(t)
	f := newFixture(t)
	out := f.add("feat/unstamped")
	if !strings.Contains(out.Reason, "unstamped") {
		t.Fatalf("reason = %q, want the unstamped note", out.Reason)
	}
	if err := f.mgr.Stamp(ctx(t), "packages"); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.Stamp(ctx(t), "plugins"); err != nil {
		t.Fatal(err)
	}
	if out = f.add("feat/stamped"); out.Reason != "" {
		t.Errorf("reason = %q, want none once the main checkout is stamped", out.Reason)
	}
	if err := f.mgr.Stamp(ctx(t), "nope"); err == nil {
		t.Error("want an error for a seed that does not exist")
	}
}

func TestSystemProgramsFindsAProgramByItsWorkingFolder(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	list, err := SystemPrograms{}.Programs(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range programs(list).using(dir) {
		if p.PID == cmd.Process.Pid {
			return
		}
	}
	t.Fatalf("pid %d works in %s, but the busy check did not see it", cmd.Process.Pid, dir)
}
