# Agents

This module is a Go library and CLI. Humans read [README.md](README.md).

**Load these skills before you operate or extend this module:**

1. [ai-copilots/skills/README.md](ai-copilots/skills/README.md) (index)
2. [ai-copilots/skills/gitvalet-operator/SKILL.md](ai-copilots/skills/gitvalet-operator/SKILL.md)
3. [ai-copilots/skills/gitvalet-developer/SKILL.md](ai-copilots/skills/gitvalet-developer/SKILL.md) (when changing packages or tests)

## Wire host discovery

Skills ship under `ai-copilots/`. They are not bound to one agent product.
Execute [ai-copilots/BOOTSTRAP.md](ai-copilots/BOOTSTRAP.md) in **wire mode**
to symlink into `.cursor/`, `.github/`, `.claude/`, or `.codex/`.

Resolve the module root when this library is only a Go dependency:

`go list -m -f '{{.Dir}}' github.com/behaviorengineering/gitvalet`

MUST keep host links pointing at this module's `ai-copilots/` tree.
MUST NOT copy skill bodies into the host unless links fail and the user approves.

## Quality gates

From module root: `go tool task --list`. Typical flow: `go tool task hooks-install` (once per clone), `go tool task tidy`, `go tool task format`, `go tool task lint`, `go tool task vet`, `go tool task test`, `go tool task build`, `go tool task smoke`.

Pre-commit runs `gofumpt` on staged `.go` files after `go tool task hooks-install`.
