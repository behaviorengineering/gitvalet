# gitvalet ai-copilots

Operator and developer agent pack for the gitvalet module (forge API + git plumbing).

Canonical source lives here. Wiring is done by the AI copilot when you ask it
to execute [BOOTSTRAP.md](BOOTSTRAP.md).

**Minimal prompt:**

> Wire gitvalet ai-copilots using BOOTSTRAP.md

## Package map

| Package | Role |
|---------|------|
| `forgeclient` | Cross-forge API (`git-pkgs/forge`) |
| `gitclone` | Pinned checkout (`git-pkgs/clone`) |
| `gitexec` | Injectable CLI with deadlines, retries, `MaxConcurrent` cap and OpenTelemetry spans |
| `gitrun` | Authenticated git subprocess helper |
| `gitinspect` | Read-only repo inspection |
| `gitfetch` | Fetch TTL cache |
| `gitmutate` | Explicit mutating git operations |
| `auth` | Token to `http.extraHeader` mapping |

## CLI

Always invoke an explicit subcommand. Bare `gitvalet` prints the agent guide only.

Mutating `sync` requires `--yes` or `--dry-run`.
