# gitvalet

A git and forge attendant for Go applications: shared forge API and Git checkout client library for majordomo, gitboard, and other hosts.

- `pkg/forgeclient`: cross-forge API adapter around `git-pkgs/forge`
- `pkg/gitclone`: pinned checkout adapter around `git-pkgs/clone`
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

## Releases (for agents)

Default branch merges that change releasable code get patch tags via CI. Consumers pin `github.com/behaviorengineering/gitvalet@vX.Y.Z`.
