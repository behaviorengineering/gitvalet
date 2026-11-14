# gitvalet

A git and forge attendant for Go applications: shared forge API and Git checkout client library for majordomo, gitboard, and other hosts.

- `pkg/forgeclient`: cross-forge API adapter around `git-pkgs/forge`, including the newest pull request a branch opened
- `pkg/gitclone`: pinned checkout adapter around `git-pkgs/clone`
- `pkg/gitworktree`: worktrees for agents, which get copy-on-write clones of the main checkout's installed packages instead of a fresh install
- `pkg/gitexec`: deadline-gated CLI execution with retries, a concurrency cap and OpenTelemetry spans
- `pkg/auth`: HTTPS token header policy for Git smart HTTP
- `cmd/gitvalet`: CI-friendly CLI (`inspect`, `resolve`, `checkout`, `sync`)

See `AGENTS.md` for agent operating rules.

## gitexec runner options

`gitexec.New()` returns a `*gitexec.Runner`. Set its fields before the first `Run`:

| Field | Default | Effect |
|-------|---------|--------|
| `Timeout` | `gitexec.DefaultTimeout` (45s) | Bounds each process attempt. Zero or negative uses the default. |
| `MaxConcurrent` | `0` (unlimited) | Caps how many processes the runner runs at once. Extra calls queue on the caller's ctx before the process starts; a ctx that ends while queued returns `ctx.Err()` wrapped and the process never runs. |

```go
r := gitexec.New()
r.MaxConcurrent = 16
out, err := r.Run(ctx, "gh", "pr", "list", "--json", "number") // ctx MUST carry a deadline
```

Every call starts spans on the global OpenTelemetry tracer provider under the
scope `gitexec.TracerName`. The host owns the provider (for example `olly.Init`);
gitvalet never installs one, so spans are no-ops until the host does.

- One internal span per logical call, named after the executable and its
  leading subcommand words (`git fetch`, `gh pr list`).
- One client span per process attempt (`git fetch attempt`), a child of the
  call span, with `gitexec.attempt` and `gitexec.queue.wait_ms`.
- Attributes: `process.executable.name`, `gitexec.subcommand`,
  `process.working_directory` (git `-C`, else the current directory),
  `process.exit.code`, `error.type` and, on the call span, `gitexec.attempts`.
- Non-zero exit, timeout, cancellation, breaker-open and queue expiry set the
  span status to error and record a rebuilt error that names only the command
  and the failure class.

Spans never carry the full argv, environment, stdin or output: arguments can
hold credentials such as `Authorization` headers or `user:password` URLs.

Errors returned from `Run` and `RunJSON` do name the command, its arguments and
stderr, but with credentials masked as `***`: URL credentials, `Authorization`
and token or password header values, values after `--token`, `--password`,
`--user`, `-u` and `--pat`, and GitHub and GitLab token shapes. Treat the
masking as a safety net, not a licence to pass secrets in argv.

## gitworktree: worktrees for agents

Agents that work in parallel each need a **git worktree** (a second working folder of the same repository, on its
own branch). A worktree has none of the main checkout's ignored folders, so each one runs its own install, and ten
worktrees of a Node project hold ten copies of `node_modules`. On the Mac that builds Totem, ten worktrees held
22.7 GiB of near-identical copies.

`pkg/gitworktree` gives a new worktree **clones** of those folders instead: copies that share their disk blocks with
the main checkout's until either side changes a file. Each group of folders is a `Seed`, with the files that decide
whether the main checkout's copy is still right for the worktree:

```go
cfg := gitworktree.Config{
	Repo:   repo,   // the main checkout
	Root:   "tmp",  // worktrees go in tmp/wt-<name>; keep it ignored by git
	Prefix: "wt-",
	Seeds: []gitworktree.Seed{{
		Name:             "packages",
		Dirs:             []string{"node_modules"},
		Inputs:           []string{"package-lock.json", "package.json"},
		Prepare:          npmCI, // runs in the worktree when the inputs differ
		PrepareOverClone: true,  // clone first, so the install only rewrites what changed
	}},
	Git: gitrun.Runner{Cred: cred},
	PRs: forgeclient.NewBranchPRs(forgeClient, repoRef), // nil turns Prune off
}
mgr, err := cfg.CreateManager()
```

| Method | What it does |
|--------|--------------|
| `Add` | Makes the worktree on a branch, creating the branch from `BaseRef` (fetched with `BaseFetch`) when it is new, then clones each seed whose inputs match and runs `Prepare` for each that differs. |
| `Share` | Swaps an existing worktree's own seed folders for clones, seed by seed, where the inputs match. |
| `Remove` | Runs `git worktree remove` and keeps the branch. It returns `ErrKept` while a program uses the worktree or it has uncommitted or untracked files. |
| `Prune` | Removes each worktree whose branch's newest pull request merged or closed, and deletes the local branch when it merged at exactly the local head. |
| `Usage` | Reports, per checkout, what `du` would count, what the checkout alone holds, and what it shares with clones elsewhere. |

Every method that changes a worktree takes `DryRun`. A program counts as using a worktree when its command line
names a path inside it, or its working folder is inside it (from `ps` and `lsof`, or a `ProgramLister` you pass).

Clones and `Usage` need APFS on macOS. Elsewhere, `Add` runs each seed's `Prepare` instead of cloning, `Share`
leaves the worktree as it is, and `Usage` returns `ErrUnsupported`. `Usage` reads each folder with
`getattrlistbulk(2)`, which has no libSystem wrapper, so the package calls it with `unix.Syscall6`.

## Releases (for agents)

Default branch merges that change releasable code get patch tags via CI. Consumers pin `github.com/behaviorengineering/gitvalet@vX.Y.Z`.
