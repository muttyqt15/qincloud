---
title: M6 — Onboarding the First Real App
slug: m6-first-app-umami
type: milestone
milestone: M6
status: stable
difficulty: 3
tags: [qincloud, infra, databases, networking, security]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m4-controld-deploy-engine]]", "[[m2-data-and-backups]]", "[[x2-per-app-observability]]", "[[make-invalid-states-unrepresentable]]", "[[fail-loud-at-boundaries]]", "[[layered-trust-defense-in-depth]]", "[[single-source-of-truth]]", "[[idempotent-self-verifying-operations]]", "[[the-box-is-disposable]]"]
sources: ["runbooks/apps/umami.md", "controld/internal/deploy/contract.go", "stack/data/compose.yml", "stack/data/initdb/01-controld.sh"]
---

# M6 — Onboarding the First Real App

> **In one sentence:** we moved from "the platform can deploy a toy container" to "the platform runs a real, stateful, database-backed product" — Umami web analytics at `analytics.sparboard.com` — and doing that forced the deploy contract to grow the two things every real app needs: **secrets** and **a database**.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Up to now the platform had only ever deployed `whoami` — a container that does nothing but echo back "hi, I'm container qc-whoami-3." Useful for proving the plumbing works, useless as a product. It has no password, no data, nothing to lose.

Think of the platform so far as a brand-new apartment building where we've only ever moved in a cardboard cutout of a tenant. The elevators run, the front door buzzes, the mailboxes are labelled — but nobody has actually *lived* there. The first real tenant is the moment you discover the oven doesn't have a gas line and the bathroom has no lock, because the cutout never needed either.

**Umami** is that first real tenant. It's a privacy-friendly Google-Analytics alternative: you drop a `<script>` tag on a website and Umami counts your visitors. Crucially it needs two things whoami never did: a **database** to store the visit counts, and **secrets** — a database password and a signing key it must not leak. Onboarding it is the test of whether QinCloud is a *platform* or just a demo.

## 2. The plan (initial approach)

The deploy engine from [[m4-controld-deploy-engine]] already knew how to pull an image, start a container named `qc-<app>-<deployID>`, and program a Caddy route to it. The plan for M6 was small on paper: point that same engine at the Umami image, give it a hostname, and go.

The data layer from [[m2-data-and-backups]] already ran a shared Postgres and Redis on a private `data_net`, backed up nightly to R2. So "give Umami a database" sounded like: make a database, hand Umami the connection string, done.

## 3. Where it deviated

Two gaps opened the moment a *real* app showed up.

**Gap 1 — the deploy contract had nowhere to put secrets.** The `AppSpec` (the struct that describes one app to deploy) had `Name`, `Image`, `ContainerPort`, `Host` — and no way to pass `DATABASE_URL` or `APP_SECRET`. whoami needed no environment at all, so the field had never existed. You cannot run Umami without handing it a database URL, and that URL *contains a password*.

**Gap 2 — "just reach Postgres" is a security decision, not a convenience.** The naive move is to drop Umami onto `data_net`, where Postgres lives. But `data_net` is also where **Redis** lives — and Redis on this box has one shared password that every service on that network can use. Put a tenant app on `data_net` and it can reach Redis, meaning a compromised Umami (or the next, less-trusted app) could `FLUSHALL` Redis or read another service's keys. The convenient network is the wrong network.

And a third, smaller trap surfaced while provisioning the database by hand — see §5, the `psql -c` gotcha, which silently created a database with the *literal string* `:'pw'` as a password on the first attempt.

## 4. The fix — and how I found it

**AppSpec grew exactly two fields, and no more** (`controld/internal/deploy/contract.go:16`):

```go
Env   map[string]string // container environment; values may be secrets — render KEYS only
UseDB bool              // attach tenant_db_net (shared Postgres reachable; redis is NOT on it)
```

`Env` carries the secrets. The rule written into the comment and enforced everywhere they're displayed: **render KEYS only, never values.** The dashboard shows `DATABASE_URL` and `APP_SECRET` as names; the values live in the `apps.env` column of the controld database and are never echoed to a screen or a log. That is [[make-invalid-states-unrepresentable]] applied to secrets — there is no code path that turns a secret value into output, so it *can't* leak that way.

`UseDB` is a boolean, not a network name, because the app must not get to *choose* its network. Set it and the deployer attaches the container to **`tenant_db_net`** — a second network Postgres is a member of, that Redis deliberately is not. That's the fix for Gap 2, and it's [[layered-trust-defense-in-depth]] at the network layer: reachability (which network you're on) and authorization (your per-role Postgres password) are two independent walls.

For the `psql` gotcha, the fix was to stop using `psql -c` for parameterised SQL and pipe the statements over stdin instead, where `-v` variables *do* interpolate.

## 5. Going deep (systems level)

**The network invariant, verified from inside the container.** `stack/data/compose.yml:24` puts Postgres on both `data_net` and `tenant_db_net`; Redis (line 63) is on `data_net` only. So an app on `tenant_db_net` can dial Postgres and *nothing else* — not Redis, not the exporters, not controld. This is invariant #3 ("tenant apps reach Postgres and only Postgres"), and it isn't taken on faith. From inside the running Umami container:

```sh
docker exec qc-umami-<id> sh -c 'nc -z qincloud-postgres 5432; echo pg=$?'   # → pg=0 (reachable)
docker exec qc-umami-<id> sh -c 'nc -z qincloud-redis   6379; echo redis=$?' # → redis=1 (no route)
```

Proving the negative from the tenant's own vantage point is [[idempotent-self-verifying-operations]] — the check costs nothing and can rerun after any topology change.

**Provisioning a per-app database** (`runbooks/apps/umami.md`, and the same pattern as `stack/data/initdb/01-controld.sh`):

```sh
UPW=$(openssl rand -hex 24)
printf "CREATE ROLE umami LOGIN PASSWORD :'pw';\nCREATE DATABASE umami OWNER umami;\n" \
  | docker exec -i qincloud-postgres psql -U qincloud -d postgres -v ON_ERROR_STOP=1 -v pw="$UPW"
```

Each app gets its own role and its own database. The role's password is the authorization wall; `tenant_db_net` is only the reachability wall.

**The `psql -c` gotcha — why the SQL goes over stdin.** `psql`'s `-v pw=…` sets a variable, and `:'pw'` in SQL expands to a safely-quoted literal — **but only for SQL read from a file or stdin.** `psql -c "…:'pw'…"` does **not** run variable interpolation on the `-c` string; you get the literal `:'pw'` as the password and a role you can never log into. Hence the `printf … | docker exec -i … psql` shape above (and `ON_ERROR_STOP=1` so a failed statement is a non-zero exit, not a silent partial apply — [[fail-loud-at-boundaries]]).

**Where the secrets live, and the one convenience copy.** `DATABASE_URL` and `APP_SECRET` are stored in `apps.env` in the controld database. That is the [[single-source-of-truth]]: they survive redeploys, they back up to R2 nightly with everything else, and they restore with the app spec. A convenience copy of the DB password sits at `/root/.umami-db-pw` (mode `0600`) so a password rotation doesn't require a round-trip through `psql` — but the runbook is explicit that the spec is the source of truth and the file is just a shortcut.

**The `Validate()` gate** (`contract.go:32`) rejects a broken spec loudly and early — bad app name, empty image, out-of-range port, or an env **key** that isn't a valid shell identifier (`^[A-Za-z_][A-Za-z0-9_]*$`, line 27). It validates keys, never values, staying consistent with "keys are structure, values are secret."

**First-boot readiness.** The Umami image ships no `HEALTHCHECK`, so `WaitReady` falls back to a grace period (`contract.go:78`). Umami runs Prisma migrations on first boot and the Caddy route can land before they finish, so the first ~30s can return 502; subsequent deploys reuse the migrated schema and are quick. Known, documented, not a bug.

## 6. How this compares to best practice

A managed platform (Render, Fly, Heroku) would inject secrets from a dedicated secret store, network-isolate every tenant into its own VPC/namespace, and give each app a *dedicated* Postgres instance rather than a shared cluster with per-role separation.

QinCloud matches the important half: secrets are keyed and never rendered, and tenants are network-isolated from everything except the one datastore they're entitled to. Where it cuts a corner: it's **one shared Postgres with per-role/per-database separation** rather than one instance per tenant, and secrets live in the controld DB rather than a Vault. On a single box that's the right tradeoff — a per-tenant Postgres would triple the memory footprint for isolation that per-role passwords + `tenant_db_net` already buy. It would need revisiting the day a genuinely untrusted third party gets to deploy, where role separation inside one cluster is no longer a strong enough boundary.

## 7. The underlying why (the transferable lesson)

**The first real user is the spec's real test.** whoami could exist for a whole milestone without exposing that `AppSpec` had no room for secrets or a database — because a stand-in never exercises the hard requirements. The contract only grew the *right* two fields (`Env`, `UseDB`) because a real app pulled on it; had we speculated those fields earlier we'd likely have guessed wrong (an app-chosen network name instead of a boolean, secret *values* in the display path instead of keys-only).

And the second lesson underneath it: **convenience and security point in opposite directions, and the platform must choose security by construction.** The easy network (`data_net`) was the insecure one. Making `UseDB` a boolean that the engine translates into `tenant_db_net` means the app *cannot* opt into reaching Redis — the safe choice isn't a discipline the operator must remember, it's the only representable choice.

---
**Teaches:** [[make-invalid-states-unrepresentable]] · [[layered-trust-defense-in-depth]] · [[fail-loud-at-boundaries]] · [[single-source-of-truth]] · [[idempotent-self-verifying-operations]] · [[the-box-is-disposable]]
**Sources:** `runbooks/apps/umami.md`, `controld/internal/deploy/contract.go`, `stack/data/compose.yml`, `stack/data/initdb/01-controld.sh`
