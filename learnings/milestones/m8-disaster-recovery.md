---
title: "M8 — Disaster Recovery: Wipe the Box"
slug: m8-disaster-recovery
type: milestone
milestone: M8
status: stable
difficulty: 3
tags: [qincloud, reliability, databases, devops, infra]
created: 2026-07-06
updated: 2026-07-06
related: ["[[the-box-is-disposable]]", "[[idempotent-self-verifying-operations]]", "[[single-source-of-truth]]", "[[root-cause-over-patch]]", "[[fail-loud-at-boundaries]]", "[[observe-what-matters]]", "[[verify-the-artifact-under-test]]", "[[m2-data-and-backups]]", "[[m4-controld-deploy-engine]]", "[[m5-dashboard]]", "[[m6-first-app-umami]]"]
sources: ["runbooks/drills/2026-07-06-m8-box-rebuild-drill.md", "README.md"]
---

# M8 — Disaster Recovery: Wipe the Box

> **In one sentence:** we deliberately destroyed the entire running server — every container, volume, network, image, and `/opt/qincloud` — then rebuilt it from a bootstrap script, the git repo, and offsite backups in about twelve minutes, to prove the claim we'd been making all along was actually true.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Imagine you keep every important thing you own in one house: your furniture, your files, your family photos. Now imagine the house burns down. Two very different feelings follow. Either you panic — because those photos existed nowhere else — or you shrug, because the deeds are at the bank, the photos are backed up in the cloud, and the furniture is just furniture you can buy again. QinCloud is meant to be the second kind of house. The server it runs on is treated as **disposable furniture**: if it vanishes, nothing irreplaceable goes with it.

That is a nice thing to *say*. Since the very first milestone, the README's fourth invariant has read: *"The box is disposable. `bootstrap.sh` + `git clone` + restore-from-R2 must rebuild it from zero."* But a promise you have never tested is just a hope with good posture. M8 was the fire drill that turned the promise into a measured fact. We set the house on fire on purpose — with a fresh backup in hand — and timed how long it took to move back in.

Two numbers decide whether a recovery plan is real. **RTO** (Recovery Time Objective) is *how long you are down* — the time from disaster to "serving again." **RPO** (Recovery Point Objective) is *how much recent data you lose* — the gap between your last backup and the moment things died. A plan that restores perfectly but takes three days has a bad RTO; one that comes back in a minute but loses a week of data has a bad RPO. M8 measured both.

## 2. The plan (initial approach)

The procedure already existed on paper — the README's "Rebuild from zero" section, ten ordered steps, each written to fail loudly if the step before it hadn't finished. The plan for the drill was simply to execute it against the *real* box, not a fantasy of it:

1. Take a fresh backup and verify it landed in R2 (so RPO would be seconds, isolating RTO as the thing under test).
2. **Scorch the box**: remove all containers, all volumes (including the Postgres data directory and Caddy's TLS certs), both Docker bridges, and every image; delete `/opt/qincloud` and the backup systemd units.
3. Rebuild using *only* the permitted inputs: `bootstrap.sh`, the repo, the R2 backups, and the operator's secret vault (standing in for a password manager).
4. Bring stacks up in dependency order — edge, data, restore, observability, controld — then redeploy each app.
5. Stop the clock when the public site answered `200` over freshly-issued TLS and Prometheus showed 10/10 scrape targets healthy.

The bet: the repo plus the backups plus a handful of secrets is a *complete* description of the system, and rebuilding is mechanical.

## 3. Where it deviated

The box came back. But the drive there was not the clean replay the README promised, and the interesting part of M8 is *why*.

**The repo had drifted ahead of the box.** Between when this box was last touched by hand and drill day, the data stack in git had evolved: it now expected a dedicated `controld` Postgres role, a new `CONTROLD_DB_PASSWORD` secret, and a fresh-init shell script — while the *running* box still used the older single-superuser layout. So the rebuild wasn't a restore at all; it was a **migration**. The backups had been taken under the old ownership, and they were being restored onto a box whose fresh init already spoke the new dialect.

That drift lit up three smaller surprises:

- **The teardown itself refused to run.** The compose file guards its variables with `${CONTROLD_DB_PASSWORD:?}` — fail-loud interpolation. That guard fires on `docker compose down`, not just `up`, so the data stack wouldn't even *stop* without the new variable set.
- **The README's own restore command was wrong.** Step 6 said `pg_restore --create`. Against a fresh `initdb` that had *already* created the `controld` and `qincloud` databases, `--create` collides. The documented happy path had never been run against a truly-empty box.
- **Nobody paged.** Alertmanager — the thing that yells when the site is down — lives *on the box*. When the whole box is gone, the pager is gone with it. The outage was, by construction, invisible.

## 4. The fix — and how I found it

Each deviation had a root cause worth more than its patch.

**The migration.** The fix wasn't to force the old layout back; it was to make the restore *perform* the migration in the same pass. `pg_restore --no-owner --role=controld` strips the dump's original ownership and re-grants everything to the dedicated role — so the same command that restores also reconciles old-owner dumps onto the new identity. The README step 6 now reads:

```
pg_restore --clean --if-exists --no-owner --role=<owning-role> -d <db>
```

The deeper lesson: **the rebuild path always follows the repo, so the running box must be reconciled to the repo in the same sitting the contract changes** — never let a live box lag its own source of truth, or DR silently becomes migration.

**The `--create` bug** was a documentation-vs-reality gap, fixed by correcting the README to the shape that actually works against a fresh init. It only surfaced because we ran the real thing; a rehearsal into a pre-seeded container would have hidden it.

**The blind spot** has no patch inside the box — that's the point. The remedy is *external*: an off-box uptime check (healthchecks.io / UptimeRobot) watching the public site and backup freshness, so a full-box outage is seen by something that isn't the box. Filed as the next hardening step.

## 5. Going deep (systems level)

The measured drill (all times UTC, from `runbooks/drills/2026-07-06-m8-box-rebuild-drill.md`):

| Metric | Value |
| --- | --- |
| **RTO** — teardown start → app serving + 10/10 targets | **≈ 12 min** |
| **RPO** — fresh pre-drill backup | ≈ 90 s (nightly schedule ⇒ ≤ 24 h in a real loss) |
| Destroyed | 14 containers, 10 volumes (incl. `pgdata` + Caddy certs), 2 bridges, all images (2.96 GB), `/opt/qincloud`, backup units |
| Survived | OS packages, Tailscale auth, `/root` vault (password-manager stand-in) |

Timeline: **T0 13:22:54** teardown begins → ~13:24 box scorched (0 containers, site DOWN) → ~13:26 `bootstrap.sh` re-run clean (idempotent), repo rsynced, `.env` + webhook secret restored from vault → ~13:28 edge + data up, fresh `initdb` creates the `controld` role, Let's Encrypt certs re-issue on their own → ~13:30 R2 restore of globals + `controld` + `qincloud` DBs (the `apps` table shows whoami + 5 deploys of history) → ~13:32 observability up (9 services), backup timer re-enabled → **~13:34 controld image rebuilt, whoami redeployed through the M5 dashboard** (`starting…` → `live`, site `200` over fresh TLS) → **13:35:04** the rebuilt box takes its *own* backup to R2 and re-arms the `BackupStale` metric.

Two mechanisms carried the recovery and are worth internalising:

- **The restored control plane drove its own recovery.** After the DB restore, `controld list` read the restored `apps` table and knew exactly what should exist. One dashboard redeploy per app recreated the container, the Caddy route, *and* the cert from the stored spec — env included, never re-typed by hand ([[m4-controld-deploy-engine]] / [[m5-dashboard]]). Deploy-ID sequences resumed correctly (next deploy = 6 after 5 restored rows) with no container-name collisions, because teardown had removed every `qc-*` container exactly as the initdb DR note prescribes.
- **`restore-drill.sh` never touches the real cluster.** It rehearses a restore into a throwaway container only. The *real* restore in step 6 is deliberately manual — a human `psql` of the globals then `pg_restore` per database — because an automated restore pointed at the live data directory is exactly the tool that turns a small incident into a total one.

The dependency order in the README's ten steps is load-bearing: edge first (creates the `caddy_admin` volume controld mounts) → data (wait for healthcheck) → restore → observability + backup timer → controld `--build` → redeploy every app → close public SSH. Each step fails loud if its predecessor didn't land.

## 6. How this compares to best practice

A mature platform team would recognise all of this: a **game day** (the deliberate destruction), measured **RTO/RPO**, and **immutable infrastructure** (rebuild, never repair). Where a managed cloud gives you multi-AZ failover and a standby replica so RTO is seconds and RPO is near-zero, QinCloud is honestly single-box: a full loss means ≈12 min of rebuild and up to 24 h of data at risk against the nightly cadence — an accepted tradeoff for a one-VPS learning platform, revisitable by tightening backup frequency (WAL archiving would drop RPO to seconds) and adding the external uptime check. The genuinely first-class move here is that **recovery is data-driven**: the control plane's own restored state is the recovery script, not a human reading a wiki. What a real team would still add — and QinCloud now has queued — is off-box alerting so the monitoring isn't a casualty of the outage it's meant to catch.

## 7. The underlying why (the transferable lesson)

A backup you have never restored is a **rumour**, not a recovery plan. The value of M8 wasn't the twelve-minute number; it was the three bugs the drill flushed out — a repo that had drifted ahead of its own box, a documented command that had never met a truly-empty machine, and a pager that dies with the thing it watches. None of those were visible from reading the code. They were only visible from running the real teardown against the real box. Confidence in a recovery plan is earned exactly once per drill, and it decays the moment the system changes underneath it. So test the whole artifact, on the real substrate, on a schedule — and treat every deviation not as a nuisance to patch past but as the drill doing its job.

---
**Teaches:** [[the-box-is-disposable]] · [[idempotent-self-verifying-operations]] · [[single-source-of-truth]] · [[root-cause-over-patch]] · [[fail-loud-at-boundaries]] · [[observe-what-matters]] · [[verify-the-artifact-under-test]]
**Sources:** `runbooks/drills/2026-07-06-m8-box-rebuild-drill.md`, `README.md` (invariant #4 + "Rebuild from zero")
