---
title: Fail Loud at the Boundaries
slug: fail-loud-at-boundaries
type: concept
status: stable
difficulty: 2
tags: [qincloud, principle, reliability, golang]
created: 2026-07-06
updated: 2026-07-06
related: ["[[make-invalid-states-unrepresentable]]", "[[root-cause-over-patch]]", "[[idempotent-self-verifying-operations]]"]
sources: ["controld/internal/deploy/contract.go:32", "controld/cmd/controld/main.go:237", "controld/internal/dockerx/dockerx.go:291", "scripts/backup.sh:69", "stack/observability/compose.yml:5", "stack/controld/compose.yml:30"]
---

# Fail Loud at the Boundaries

> **The principle in one line:** validate at every trust boundary and stop at the *cause* — a component that silently substitutes a default for bad input isn't degraded, it's broken and lying about it.

## What it means (plain English)

Think of a factory line. If a bad part arrives at the loading dock, you reject it *at the dock* — with a note saying exactly what's wrong — not three stations later when it jams a machine and you're left guessing which of a hundred parts caused it. A "boundary" is any place your system takes in something it didn't produce: an environment variable, a user's deploy request, a reply from the Docker daemon, a file on disk. The rule: check it the instant it crosses in, and if it's wrong, halt loudly and name the reason.

The opposite — the tempting shortcut — is to paper over bad input with a default: an empty password becomes `""`, a missing config becomes `"localhost"`. Now the system runs, but wrong, and the real fault surfaces far away as a confusing symptom.

## Why it matters

A silent default turns a five-second config error into a two-hour debugging session, because the crash happens nowhere near its cause. Worse, some defaults are dangerous: an empty bind address doesn't fail, it falls back to `0.0.0.0` — every network interface — quietly exposing an admin UI to the whole internet.

## Where it showed up in QinCloud

- **`AppSpec.Validate()` runs before any Docker call** ([[m4-controld-deploy-engine]], `contract.go:32`). The comment says it outright: "Fail here, loud, not three layers down in the Docker API." A bad app name is rejected against `^[a-z0-9][a-z0-9-]{0,31}$` at the boundary, not discovered when `docker run` chokes on it.
- **`mustEnv` refuses to start on missing config** ([[m4-controld-deploy-engine]], `main.go:237`): "a control plane with a silently-defaulted DSN is broken, not degraded." It `log.Fatalf`s rather than run against a phantom database.
- **`${VAR:?}` in every compose file** ([[m0-host-baseline]], [[m3-observability-and-alerting]], [[m8-disaster-recovery]]). Admin UIs bind `"${TS_IP:?}:3000:3000"`; the `:?` aborts compose if `TS_IP` is empty, because the silent fallback binds `0.0.0.0` and exposes Grafana publicly. The guard even fires on `compose down`.
- **The backup upload verifies a positive byte count** ([[m2-data-and-backups]], `backup.sh:69`): it re-reads the uploaded object's size via `rclone lsl` and `die`s if it isn't `> 0` — "an empty offsite backup is a silent disaster."
- **External responses are parsed, and sentinels are treated as errors** ([[m4-controld-deploy-engine]], `dockerx.go:291`). The Docker daemon answers `200` with an all-zero stats body for a dead container; controld treats the zero `Read` timestamp as the error it really is, instead of reporting "0% / up=1" for a corpse.

## How to apply it

Parse, don't trust, at each boundary: required env → fatal if absent; external JSON → decode into a typed shape and reject sentinels; user input → validate against an explicit schema before acting. Put the check where the data *enters*, and make the error name the offending value.

## Signs you're violating it

`?? ""`, `|| "default"`, `os.Getenv` used directly without a presence check, an `as`-cast on a network response, or a crash whose stack trace points nowhere near the actual mistake. If your first debugging step is "which layer produced this nil?", the boundary check is missing.

---
**Related:** [[make-invalid-states-unrepresentable]] · [[root-cause-over-patch]] · [[idempotent-self-verifying-operations]]
