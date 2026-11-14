// Package gitworktree makes, slims, measures and removes the git worktrees that agents work in, all under one folder
// of a main checkout.
//
// A new worktree gets copy-on-write clones of the main checkout's ignored folders, such as node_modules, instead of a
// fresh install, when the files those folders come from match. Each such group of folders is a Seed. Removing a
// worktree keeps its branch and refuses while a program uses it, and pruning removes the worktrees whose pull request
// has closed.
//
// Clones and Usage need APFS on macOS. On other systems a seed falls back to its Prepare step, and Usage returns
// ErrUnsupported.
package gitworktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/behaviorengineering/olly/pkg/olly"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/behaviorengineering/gitvalet/pkg/auth"
	"github.com/behaviorengineering/gitvalet/pkg/gitexec"
	"github.com/behaviorengineering/gitvalet/pkg/gitrun"
	"github.com/behaviorengineering/gitvalet/pkg/pullrequest"
)

// TracerName is the instrumentation scope of the spans this package starts.
const TracerName = "github.com/behaviorengineering/gitvalet/pkg/gitworktree"

// Errors a caller can test for with errors.Is.
var (
	// ErrNotFound is returned when no worktree under the root matches a name.
	ErrNotFound = errors.New("gitworktree: no worktree by that name")
	// ErrExists is returned when Add finds its folder already there.
	ErrExists = errors.New("gitworktree: the folder already exists")
	// ErrKept is returned when Remove refuses, because a program uses the worktree or it has changes git would lose.
	ErrKept = errors.New("gitworktree: kept the worktree")
	// ErrUnsupported is returned where folders cannot be cloned or their shared blocks cannot be read: a file system
	// without APFS clones, or a main checkout and worktree on different volumes.
	ErrUnsupported = errors.New("gitworktree: cannot clone folders here")
	// ErrNoPRFinder is returned by Prune when the Config has no PRFinder.
	ErrNoPRFinder = errors.New("gitworktree: pruning needs a PRFinder")
	// ErrMissingDeadline is returned when a step that reaches the network gets a context with no deadline.
	ErrMissingDeadline = errors.New("gitworktree: missing deadline")
)

// PRFinder finds the newest pull request a branch opened. A pullrequest.Finder answers it for any forge that
// git-pkgs/forge supports.
type PRFinder interface {
	NewestPR(ctx context.Context, branch string) (pullrequest.PR, bool, error)
}

var _ PRFinder = (*pullrequest.Finder)(nil)

// Config holds what a Manager needs.
type Config struct {
	// Repo is the main checkout. Its worktrees live under Root, and its seed folders are what they clone.
	Repo string
	// Root is the folder inside Repo that holds the worktrees, such as "tmp". It SHOULD be ignored by git.
	Root string
	// Prefix starts each worktree's folder name, such as "wt-". It may be empty.
	Prefix string
	// BaseRef is where a new branch starts when AddOptions.From is empty. Empty means the main checkout's HEAD.
	BaseRef string
	// BaseFetch is a refspec fetched from origin before a new branch starts at BaseRef, such as
	// "+refs/tags/last-green:refs/tags/last-green" or "main". Empty fetches nothing.
	BaseFetch string
	// Seeds are the groups of ignored folders a worktree gets from the main checkout, in order.
	Seeds []Seed
	// Git runs local git commands, with its Cred for the fetch too.
	Git gitrun.Runner
	// Exec runs the fetch from origin, with retries, a circuit breaker and spans. Nil uses gitexec.New().
	Exec *gitexec.Runner
	// Programs lists the running programs for the busy check. Nil uses SystemPrograms.
	Programs ProgramLister
	// PRs finds a branch's pull request for Prune. Nil turns Prune off.
	PRs PRFinder
	// Logger reports each slow step as it starts. Nil discards.
	Logger *slog.Logger
}

// Manager makes, slims, measures and removes the worktrees under one main checkout's Root.
type Manager struct {
	cfg    Config
	root   string
	tracer trace.Tracer
	// clone and canClone are cloneTree and canCloneInto, swapped in tests to act as a system that cannot clone.
	clone    func(src, dst string) error
	canClone func(src, dst string) bool
}

// validName is a folder name with no path in it.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CreateManager checks cfg and returns its Manager.
func (cfg Config) CreateManager() (*Manager, error) {
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, errors.New("gitworktree: Config.Repo is required")
	}
	if cfg.Root == "" || filepath.IsAbs(cfg.Root) || !filepath.IsLocal(cfg.Root) {
		return nil, fmt.Errorf("gitworktree: Config.Root %q must be a folder inside Repo", cfg.Root)
	}
	if cfg.Prefix != "" && !validName.MatchString(cfg.Prefix) {
		return nil, fmt.Errorf("gitworktree: Config.Prefix %q may hold only letters, digits, '.', '_' and '-'", cfg.Prefix)
	}
	for i, s := range cfg.Seeds {
		if err := s.check(); err != nil {
			return nil, fmt.Errorf("gitworktree: seed %d: %w", i, err)
		}
	}
	// git lists worktrees by their real paths, so the checkout's path is made real too, or /var and /private/var
	// would name one folder twice.
	resolved, err := filepath.EvalSymlinks(cfg.Repo)
	if err != nil {
		return nil, fmt.Errorf("gitworktree: find the main checkout %s: %w", cfg.Repo, err)
	}
	cfg.Repo = resolved
	if cfg.Programs == nil {
		cfg.Programs = SystemPrograms{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Exec == nil {
		cfg.Exec = gitexec.New()
	}
	return &Manager{
		cfg:      cfg,
		root:     filepath.Join(resolved, cfg.Root),
		tracer:   olly.Tracer(TracerName),
		clone:    cloneTree,
		canClone: canCloneInto,
	}, nil
}

// Worktree is one worktree under the root.
type Worktree struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Branch is empty for a worktree on a detached HEAD.
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head"`
}

// Outcome says what a method did, or would do in a dry run, to one worktree.
type Outcome struct {
	Worktree
	// Done is true when the worktree was changed, or would be in a dry run.
	Done bool `json:"done"`
	// Steps are what was done, in order.
	Steps []string `json:"steps,omitempty"`
	// Reason says why the worktree, or some of its seeds, were left as they were.
	Reason string `json:"reason,omitempty"`
	// Users are the programs that kept it, when they did.
	Users []Program `json:"users,omitempty"`
}

// Git words used more than once.
const (
	gitWorktree = "worktree"
	gitQuiet    = "--quiet"
)

// Steps and reasons an Outcome reports.
const (
	stepFetch        = "fetch "
	stepCreate       = "create the worktree"
	stepRemove       = "remove the worktree; its branch stays"
	stepRemoveOnly   = "remove the worktree"
	stepDeleteBranch = "delete the merged branch"
	reasonBusy       = "a program is using it"
)

// List returns the worktrees under the root.
func (m *Manager) List(ctx context.Context) ([]Worktree, error) {
	if ctx == nil {
		return nil, errors.New("gitworktree: nil context")
	}
	out, err := m.cfg.Git.RunTrim(ctx, m.cfg.Repo, gitWorktree, "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("gitworktree: list worktrees: %w", err)
	}
	var all []Worktree
	var cur *Worktree
	for line := range strings.SplitSeq(out, "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case gitWorktree:
			all = append(all, Worktree{Path: value, Name: filepath.Base(value)})
			cur = &all[len(all)-1]
		case "HEAD":
			if cur != nil {
				cur.Head = value
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		}
	}
	var under []Worktree
	for _, w := range all {
		if inside(w.Path, m.root) && w.Path != m.root {
			under = append(under, w)
		}
	}
	return under, nil
}

// Find returns the worktree a name stands for: its folder name with or without the prefix, its branch, or its path.
func (m *Manager) Find(ctx context.Context, name string) (Worktree, error) {
	all, err := m.List(ctx)
	if err != nil {
		return Worktree{}, err
	}
	if strings.TrimSpace(name) == "" {
		return Worktree{}, fmt.Errorf("%w: a name is required", ErrNotFound)
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return Worktree{}, fmt.Errorf("gitworktree: resolve %q: %w", name, err)
	}
	for _, w := range all {
		// A worktree on a detached HEAD has no branch, so its empty Branch must never match.
		if w.Name == name || w.Name == m.cfg.Prefix+name || (w.Branch != "" && w.Branch == name) || w.Path == abs {
			return w, nil
		}
	}
	return Worktree{}, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// AddOptions say which worktree to make.
type AddOptions struct {
	// Branch is checked out when it exists, and made from From when it does not.
	Branch string
	// Name is the folder under the root; it defaults to the prefix and the branch's last segment.
	Name string
	// From is where a new branch starts; it defaults to Config.BaseRef, fetched first with Config.BaseFetch.
	From   string
	DryRun bool
}

// Add makes a worktree under the root and gives it each seed's folders: clones of the main checkout's when the seed's
// inputs match, and the seed's Prepare step when they differ.
func (m *Manager) Add(ctx context.Context, opts AddOptions) (out Outcome, err error) {
	if ctx == nil {
		return Outcome{}, errors.New("gitworktree: nil context")
	}
	ctx, span := m.tracer.Start(ctx, "gitworktree.add")
	defer func() { endSpan(span, err) }()

	if strings.TrimSpace(opts.Branch) == "" {
		return Outcome{}, errors.New("gitworktree: a branch is required")
	}
	name, err := m.folderName(opts)
	if err != nil {
		return Outcome{}, err
	}
	path := filepath.Join(m.root, name)
	span.SetAttributes(attribute.String("gitworktree.branch", opts.Branch), attribute.String("gitworktree.path", path))
	out = Outcome{Worktree: Worktree{Name: name, Path: path, Branch: opts.Branch}, Done: true}
	if exists(path) {
		return out, fmt.Errorf("%w: %s", ErrExists, path)
	}
	_, code := m.cfg.Git.AllowFail(ctx, m.cfg.Repo, "rev-parse", "--verify", gitQuiet, "refs/heads/"+opts.Branch)
	branchExists := code == 0

	from := firstOf(opts.From, m.cfg.BaseRef, "HEAD")
	if !branchExists && opts.From == "" && m.cfg.BaseFetch != "" {
		out.Steps = append(out.Steps, stepFetch+m.cfg.BaseFetch)
		if !opts.DryRun {
			if err = m.fetchBase(ctx); err != nil {
				return out, err
			}
		}
	}
	if branchExists {
		out.Steps = append(out.Steps, stepCreate+" on the existing branch")
	} else {
		out.Steps = append(out.Steps, stepCreate+" on a new branch from "+from)
	}
	if opts.DryRun {
		for _, s := range m.cfg.Seeds {
			out.Steps = append(out.Steps, s.plannedOnAdd())
		}
		return out, nil
	}
	if out.Head, err = m.create(ctx, path, worktreeAddArgs(path, opts.Branch, from, branchExists)); err != nil {
		return out, err
	}
	for _, s := range m.cfg.Seeds {
		var steps []string
		var reason string
		steps, reason, err = m.seed(ctx, s, seedRun{worktree: path, add: true})
		out.Steps = append(out.Steps, steps...)
		out.Reason = joinReason(out.Reason, reason)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// worktreeAddArgs is the git worktree add that makes path on branch when it exists, and on a new branch from from
// when it does not.
func worktreeAddArgs(path, branch, from string, branchExists bool) []string {
	if branchExists {
		return []string{gitWorktree, "add", gitQuiet, path, branch}
	}
	return []string{gitWorktree, "add", gitQuiet, "-b", branch, path, from}
}

// create runs git with args, a git worktree add that makes the worktree at path, and returns its HEAD.
func (m *Manager) create(ctx context.Context, path string, args []string) (string, error) {
	if _, err := m.cfg.Git.Run(ctx, m.cfg.Repo, args...); err != nil {
		return "", fmt.Errorf("gitworktree: create the worktree: %w", err)
	}
	head, err := m.cfg.Git.RunTrim(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("gitworktree: read the new worktree's HEAD: %w", err)
	}
	return head, nil
}

// folderName is the folder under the root that AddOptions ask for.
func (m *Manager) folderName(opts AddOptions) (string, error) {
	name := opts.Name
	if name == "" {
		parts := strings.Split(opts.Branch, "/")
		name = parts[len(parts)-1]
	}
	if !strings.HasPrefix(name, m.cfg.Prefix) {
		name = m.cfg.Prefix + name
	}
	if !validName.MatchString(name) {
		return "", fmt.Errorf("gitworktree: the folder name %q may hold only letters, digits, '.', '_' and '-'", name)
	}
	return name, nil
}

// fetchBase fetches Config.BaseFetch from origin through Config.Exec, which retries transient failures. It reaches
// the network, so ctx MUST carry a deadline.
func (m *Manager) fetchBase(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		return ErrMissingDeadline
	}
	args := append(auth.GitConfigArgs(m.cfg.Git.Cred), "-C", m.cfg.Repo, "fetch", gitQuiet, "origin", m.cfg.BaseFetch)
	if _, err := m.cfg.Exec.Run(ctx, "git", args...); err != nil {
		return fmt.Errorf("gitworktree: fetch %s: %w", m.cfg.BaseFetch, err)
	}
	return nil
}

// ShareOptions say which worktrees to slim.
type ShareOptions struct {
	// Names picks worktrees by folder name, branch or path; empty means every worktree under the root.
	Names  []string
	DryRun bool
}

// Share swaps the seed folders of existing worktrees for clones of the main checkout's, seed by seed, where the
// seed's inputs match. It leaves a worktree a program is using, a seed the worktree has no folders for, and a seed
// whose inputs differ, since those folders are not the main checkout's.
func (m *Manager) Share(ctx context.Context, opts ShareOptions) (outs []Outcome, err error) {
	if ctx == nil {
		return nil, errors.New("gitworktree: nil context")
	}
	ctx, span := m.tracer.Start(ctx, "gitworktree.share")
	defer func() { endSpan(span, err) }()

	targets, err := m.pick(ctx, opts.Names)
	if err != nil {
		return nil, err
	}
	ps, err := m.programs(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range targets {
		out := Outcome{Worktree: w}
		if users := ps.using(w.Path); len(users) > 0 {
			out.Reason, out.Users = reasonBusy, users
			outs = append(outs, out)
			continue
		}
		for _, s := range m.cfg.Seeds {
			var steps []string
			var reason string
			steps, reason, err = m.seed(ctx, s, seedRun{worktree: w.Path, dryRun: opts.DryRun})
			out.Steps = append(out.Steps, steps...)
			out.Reason = joinReason(out.Reason, reason)
			if err != nil {
				return append(outs, out), err
			}
		}
		out.Done = len(out.Steps) > 0
		outs = append(outs, out)
	}
	return outs, nil
}

// RemoveOptions say which worktree to remove.
type RemoveOptions struct {
	// Name is the worktree's folder name, with or without the prefix, its branch, or its path.
	Name   string
	DryRun bool
}

// Remove removes one worktree and keeps its branch. It refuses, with ErrKept, while a program uses the worktree or
// while it has uncommitted changes or untracked files.
func (m *Manager) Remove(ctx context.Context, opts RemoveOptions) (out Outcome, err error) {
	if ctx == nil {
		return Outcome{}, errors.New("gitworktree: nil context")
	}
	ctx, span := m.tracer.Start(ctx, "gitworktree.remove")
	defer func() { endSpan(span, err) }()

	w, err := m.Find(ctx, opts.Name)
	if err != nil {
		return Outcome{}, err
	}
	ps, err := m.programs(ctx)
	if err != nil {
		return Outcome{}, err
	}
	out = Outcome{Worktree: w}
	if out.Reason, out.Users, err = m.keep(ctx, w, ps); err != nil {
		return out, err
	}
	if out.Reason != "" {
		return out, fmt.Errorf("%w %s: %s", ErrKept, w.Path, out.Reason)
	}
	out.Done, out.Steps = true, []string{stepRemove}
	if opts.DryRun {
		return out, nil
	}
	if _, err = m.cfg.Git.Run(ctx, m.cfg.Repo, gitWorktree, "remove", w.Path); err != nil {
		return out, fmt.Errorf("gitworktree: remove %s: %w", w.Path, err)
	}
	return out, nil
}

// keep returns why a worktree must stay: a program is using it, or it has changes git would lose.
func (m *Manager) keep(ctx context.Context, w Worktree, ps programs) (string, []Program, error) {
	if users := ps.using(w.Path); len(users) > 0 {
		return reasonBusy, users, nil
	}
	changed, err := m.cfg.Git.RunTrim(ctx, w.Path, "status", "--porcelain")
	if err != nil {
		return "", nil, fmt.Errorf("gitworktree: read the status of %s: %w", w.Path, err)
	}
	if changed != "" {
		return fmt.Sprintf("it has %d uncommitted or untracked files", strings.Count(changed, "\n")+1), nil, nil
	}
	return "", nil, nil
}

// PruneOptions change how Prune runs.
type PruneOptions struct {
	DryRun bool
}

// Prune removes each worktree whose branch's newest pull request has closed, and deletes the local branch too when
// the pull request merged at exactly the branch's head. It keeps a worktree on a detached HEAD, one with no pull
// request or an open one, one a program is using, and one with uncommitted changes. The context MUST carry a
// deadline, because Prune asks the forge.
func (m *Manager) Prune(ctx context.Context, opts PruneOptions) (outs []Outcome, err error) {
	if ctx == nil {
		return nil, errors.New("gitworktree: nil context")
	}
	if m.cfg.PRs == nil {
		return nil, ErrNoPRFinder
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrMissingDeadline
	}
	ctx, span := m.tracer.Start(ctx, "gitworktree.prune")
	defer func() { endSpan(span, err) }()

	all, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := m.programs(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range all {
		var out Outcome
		out, err = m.pruneOne(ctx, w, ps, opts.DryRun)
		outs = append(outs, out)
		if err != nil {
			return outs, err
		}
	}
	return outs, nil
}

func (m *Manager) pruneOne(ctx context.Context, w Worktree, ps programs, dryRun bool) (Outcome, error) {
	out := Outcome{Worktree: w}
	if w.Branch == "" {
		out.Reason = "it is on a detached HEAD, not a branch"
		return out, nil
	}
	pr, found, err := m.cfg.PRs.NewestPR(ctx, w.Branch)
	if err != nil {
		return out, fmt.Errorf("gitworktree: find the pull request for %s: %w", w.Branch, err)
	}
	switch {
	case !found:
		out.Reason = "its branch has no pull request"
		return out, nil
	case pr.Open():
		out.Reason = fmt.Sprintf("pull request #%d is open", pr.Number)
		return out, nil
	}
	if out.Reason, out.Users, err = m.keep(ctx, w, ps); err != nil || out.Reason != "" {
		return out, err
	}
	deleteBranch := pr.Merged && pr.HeadSHA == w.Head
	remove := stepRemove
	if deleteBranch {
		remove = stepRemoveOnly
	}
	out.Done, out.Steps = true, []string{fmt.Sprintf("%s (pull request #%d %s)", remove, pr.Number, closedAs(pr))}
	if deleteBranch {
		out.Steps = append(out.Steps, stepDeleteBranch)
	}
	if dryRun {
		return out, nil
	}
	if _, err := m.cfg.Git.Run(ctx, m.cfg.Repo, gitWorktree, "remove", w.Path); err != nil {
		return out, fmt.Errorf("gitworktree: remove %s: %w", w.Path, err)
	}
	if deleteBranch {
		if _, err := m.cfg.Git.Run(ctx, m.cfg.Repo, "branch", "-D", w.Branch); err != nil {
			return out, fmt.Errorf("gitworktree: delete the merged branch %s: %w", w.Branch, err)
		}
	}
	return out, nil
}

func closedAs(pr pullrequest.PR) string {
	if pr.Merged {
		return "merged"
	}
	return "closed without merging"
}

// Stamp records, in the main checkout's folders for the named seed, the fingerprint of the inputs they hold now. Call
// it straight after installing in the main checkout, so new worktrees compare their inputs with what was installed
// rather than with whatever the main checkout's input files say later.
func (m *Manager) Stamp(ctx context.Context, seed string) error {
	if ctx == nil {
		return errors.New("gitworktree: nil context")
	}
	for _, s := range m.cfg.Seeds {
		if s.Name == seed {
			return s.writeStamp(m.cfg.Repo, m.root)
		}
	}
	return fmt.Errorf("gitworktree: no seed named %q", seed)
}

// pick returns the worktrees names stand for, or all of them when names is empty.
func (m *Manager) pick(ctx context.Context, names []string) ([]Worktree, error) {
	if len(names) == 0 {
		return m.List(ctx)
	}
	picked := make([]Worktree, 0, len(names))
	for _, n := range names {
		w, err := m.Find(ctx, n)
		if err != nil {
			return nil, err
		}
		picked = append(picked, w)
	}
	return picked, nil
}

// programs reads the running programs once for a method.
func (m *Manager) programs(ctx context.Context) (programs, error) {
	list, err := m.cfg.Programs.Programs(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitworktree: list running programs: %w", err)
	}
	return programs(list), nil
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitworktree step failed")
	}
	span.End()
}

func joinReason(have, more string) string {
	switch {
	case more == "":
		return have
	case have == "":
		return more
	default:
		return have + "; " + more
	}
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
