---
name: gitvalet-developer
description: >-
  Extend gitvalet packages and CLI: forgeclient, gitclone, gitexec, gitrun.
  Use when adding packages, changing subprocess or forge wiring, or writing
  offline tests for gitvalet.
---

# gitvalet developer

## Constraints

**CONSTRAINT:** MUST keep forge API wiring in `pkg/forgeclient` (upstream `git-pkgs/forge`).
- Enforcement: `rg 'git-pkgs/forge' pkg/forgeclient`
- Violation: STOP, move forge calls into forgeclient

**CONSTRAINT:** MUST keep checkout mechanics in `pkg/gitclone` (upstream `git-pkgs/clone`).
- Enforcement: Clone options and EnsureCheckout live in gitclone
- Violation: STOP, consolidate in gitclone

**CONSTRAINT:** MUST route Git subprocesses through `pkg/gitexec` or `pkg/gitrun`; MUST NOT call `exec.Command("git", ...)` from other public packages.
- Enforcement: `rg 'exec.Command.*git' pkg/` outside gitexec/gitrun
- Violation: STOP, wrap in gitexec or gitrun

**CONSTRAINT:** MUST keep tests offline with fakes; live forge tests MUST be opt-in via environment variables.
- Enforcement: `go test ./...` passes without network; live tests gated by env
- Violation: STOP, add fakes or env skip

**CONSTRAINT:** MUST release with annotated `v*` tags; consumers MUST pin exact module versions.
- Enforcement: Tag on main; GoReleaser release for CLI artifacts
- Violation: STOP, cut tag before telling consumers to bump

CORRECT:
```go
run := gitrun.Runner{Cred: cred}
out, err := run.Run(ctx, dir, "rev-parse", "HEAD")
```

PROHIBITED:
```go
cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
```

## Quality gates

Run from module root: `go tool task hooks-install` (once per clone), `go tool task --list`, `go tool task tidy`, `go tool task format`, `go tool task lint`, `go tool task vet`, `go tool task test`, `go tool task build`, `go tool task smoke`.

**CONSTRAINT:** MUST run `go tool task hooks-install` before the first commit in a new clone so `.githooks/pre-commit` formats staged Go files.
- Enforcement: `git config --get core.hooksPath` is `.githooks`
- Violation: STOP, run `go tool task hooks-install`, then commit

## Pre-completion verification

- [ ] **Layout:** New code sits under `pkg/<domain>/` or `internal/cli`
      Method: List new files
      Pass: No grab-bag `util` packages
      Fail: STOP, move to domain package
- [ ] **CLI surface:** Bare `gitvalet` prints agent guide; `version` needs no config
      Method: `go tool task smoke`
      Pass: smoke target exits 0
      Fail: STOP, fix internal/cli before merge
