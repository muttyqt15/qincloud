# Dev-process gotchas — building controld with parallel agents

How M4 was actually built: parallel draft agents on one Go module → integrate
→ 3-lens adversarial review (with read-only box access) → per-scope fix
agents. What made it work:

## Pre-resolve go.mod before fanning out

Parallel agents each running `go get`/`go mod tidy` race on `go.mod`/`go.sum`.
Fix: before spawning, add one temp file importing **every** dependency any
agent will need, `go mod tidy`, delete the temp file — then forbid agents from
touching `go.mod`/`go.sum` entirely.

## Disjoint file scopes, shared contract

Each agent owns a disjoint set of files; the interfaces they code against
live in one contract file (`internal/deploy/contract.go`) written *first*.
Integration is then mechanical. An agent that "helpfully" edits a neighbor's
file is the failure mode — say so in the prompt.

## Ground fixtures in reality, not memory

The caddyapi tests were written against fixtures captured from a real
`caddy adapt` run — which is how the split :443/:80 topology (and the
ambiguity `pickPublicServer` must reject) was discovered at all. Rule: when
testing against an external system's config/API shape, capture the real
shape first; don't write the fixture from documentation memory.

## Review findings need an empirical check

The adversarial review lenses had read-only ssh to the box; several
"findings" dissolved on contact with the running system (e.g. the :80 `308`
redirect — correct auto-HTTPS behavior, not a bug). A finding that can be
checked against the live system, must be, before it becomes a fix.
