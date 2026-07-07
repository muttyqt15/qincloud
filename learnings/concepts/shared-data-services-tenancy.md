---
title: Shared Data-Services Tenancy (One Instance, Many Principals)
slug: shared-data-services-tenancy
type: concept
status: stable
difficulty: 3
tags: [qincloud, postgres, redis, security, multi-tenancy]
created: 2026-07-07
updated: 2026-07-07
related: ["[[adversarial-review]]", "[[verify-the-artifact-under-test]]", "[[layered-trust-defense-in-depth]]", "[[fail-loud-at-boundaries]]", "[[single-source-of-truth]]"]
sources: ["controld/internal/provision/provision.go", "stack/data/compose.yml", "runbooks/data-services.md", "runbooks/gotchas/data-services.md"]
---

# Shared Data-Services Tenancy (One Instance, Many Principals)

> **The principle in one line:** apps share one Postgres and one Redis, but each authenticates as its own principal that can only touch its own data — the network grants *reachability*, credentials grant *authorization*, and one command (`controld provision`) is the single door to both.

## What it means (plain English)

A one-box PaaS cannot afford a database server per app, so everyone shares.
Sharing safely means splitting two questions the network alone conflates:

- **Can the app reach the server?** → `tenant_db_net`, joined via `-db` at
  deploy. Carries only postgres + redis + opted-in apps.
- **What may it do there?** → a per-app principal named after the app:
  a Postgres role owning database `<app>` (PUBLIC's default CONNECT revoked,
  so no cross-database hops), and a Redis ACL user fenced to the `<app>:*`
  key prefix with admin/dangerous commands removed.

The password is generated at provision time, printed **once**, and stored
only in the app's own spec — the platform never keeps a copy.

## The war story (why this note exists)

The rollout survived a 26-agent adversarial review that confirmed 13 real
findings — including that `-@dangerous` blocks `INFO`, which BullMQ sends at
connect (verified by running BullMQ against the exact pinned image). And the
review still missed the worst bug: with `--aclfile` configured, Redis
**replaces its entire user table from the file at boot** — an *empty*
aclfile resets the default user to `nopass`, and `requirepass` is silently
ignored. For a few minutes the platform Redis had **no authentication**.

The post-rollout verification (`redis-cli PING` unauthenticated: expected
NOAUTH, got PONG) caught it in the first minute. The fix was structural, not
a patch: a one-shot init container now rewrites the `user default on
#<sha256>` line from `.env` on every `up`, making `.env` the single owner of
the platform password on every future box, and the dead `--requirepass` knob
was deleted rather than left as a decoy — see [[single-source-of-truth]] and
[[adversarial-review]] (its "check each finding against the running system"
clause is the whole lesson: reviews argue, drills *know*).

## Honest limits

Data isolation, not resource isolation: a runaway tenant query still hurts
its neighbors, Redis `SCAN` still leaks key *names* (never values), and
there is no per-app disk quota. When an app outgrows the shared tier, it
gets its own container — the same judgment call as any vertical split.

## Where to see it

- `controld/internal/provision/provision.go` — the whole mechanism, with the
  exec-env/hashed-password discipline (secrets never in argv).
- `stack/data/compose.yml` — the `redis-acl-init` one-shot.
- `runbooks/data-services.md` — operator guide (provision, migrate, rotate,
  deprovision, DR).
- `runbooks/gotchas/data-services.md` — the sharp edges, including the
  aclfile-resets-everything trap.
