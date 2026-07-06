---
title: M2 — Data & Offsite Backups to R2
slug: m2-data-and-backups
type: milestone
milestone: M2
status: stable
difficulty: 3
tags: [qincloud, databases, reliability, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m0-host-baseline]]", "[[m3-observability-and-alerting]]", "[[m8-disaster-recovery]]", "[[the-box-is-disposable]]", "[[idempotent-self-verifying-operations]]", "[[verify-the-artifact-under-test]]", "[[fail-loud-at-boundaries]]", "[[observe-what-matters]]", "[[root-cause-over-patch]]"]
sources: ["stack/data/compose.yml", "scripts/backup.sh", "scripts/restore-drill.sh", "runbooks/drills/2026-07-06-m2-backup-restore-drill.md", "runbooks/gotchas/backups-r2.md"]
---

# M2 — Data & Offsite Backups to R2

> **In one sentence:** stand up Postgres and Redis on private networks, copy their data every night to an off-site vault (Cloudflare R2), and — the part most people skip — actually rehearse pulling it back, timing how long a real recovery takes.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Imagine a restaurant that keeps its only copy of every recipe on a whiteboard in the kitchen. Business is fine — until a pipe bursts and the whiteboard is ruined. Every recipe is gone. Nobody wrote them down anywhere else, and nobody had ever *tried* rewriting them from memory to check it was possible.

QinCloud runs on **one** physical box. All the important state — user accounts, what the control plane deployed, app data — lives in a database on that box. If the box dies, or a bad command wipes a table, that state is the whiteboard. M0 gave us [[the-box-is-disposable]] as an aspiration: we should be able to lose the whole machine and rebuild. But a rebuild only works if the *data* survived somewhere else.

M2 is the "write the recipes down, off-site, every night, and prove you can read them back" milestone. Two databases: **Postgres** (the durable system of record) and **Redis** (fast in-memory cache/queue). Both get copied nightly to Cloudflare R2 — cheap object storage in someone else's building. And then, critically, we do a **restore drill**: fetch last night's copy, rebuild the database from it in a throwaway container, and check it actually contains real tables. A backup you have never restored is a rumour, not a backup.

## 2. The plan (initial approach)

The design was deliberately boring, because boring survives 3am:

- Run Postgres and Redis in Docker, on **private networks only** — never published to the internet. Reachability is a network property; authorization is a password property; keep them separate.
- One shell script, `backup.sh`, run by a systemd timer at 03:00. It should: dump every Postgres database, snapshot Redis, gzip each file with a timestamped name, upload to R2, **verify** each upload is non-empty, and prune to the newest 14 copies so storage doesn't grow forever.
- One shell script, `restore-drill.sh`, that reads the newest dump back from R2 into a *throwaway* container and counts the tables — never touching the live database.
- Wire success into the monitoring from [[m3-observability-and-alerting]] so that a backup which silently stops running *pages a human*.

The whole thing is two ~150-line bash scripts and a compose file. No backup product, no agent. On a one-box platform, `pg_dump` piped to object storage is the right amount of machinery — see [[the-box-is-disposable]].

## 3. Where it deviated

Three surprises, each a small landmine:

1. **The R2 credentials weren't where I expected them.** Cloudflare's S3-compatible API needs an Access Key ID and a Secret Access Key. But if you create an R2 **API token**, Cloudflare shows you the S3 keypair *once* and then only ever shows you the token. I had the token value and no keypair.

2. **Every single upload failed on its first try** with `501 NotImplemented`, then mysteriously succeeded on a retry. Not random — *every* upload, deterministically. Something was systematically wrong, and "it works on retry" is exactly the kind of noise that eventually hides a *real* failure inside the retries.

3. **The alert for "backups stopped" was already shipped in M3 — and did nothing.** It watched a metric that no code had ever produced. An alert on a non-existent metric isn't a safety net; it's a decoration that makes you *feel* safe.

## 4. The fix — and how I found it

**The derived keypair.** Reading Cloudflare's S3 docs closely: the keypair is not random, it is *derived* from the token. **Access Key ID** = the token's ID (fetch it via `GET /user/tokens/verify` with the token as a bearer). **Secret Access Key** = the SHA-256 hex of the token value: `printf %s "$TOKEN" | sha256sum`. So the token is the only secret we store; the keypair is computed. Rotation becomes two `.env` lines with no console visit. (`runbooks/gotchas/backups-r2.md`.)

**The 501 storm.** This was [[root-cause-over-patch]] in miniature. The lazy fix is "retries hide it, ship it." The real cause: Ubuntu noble's apt ships **rclone 1.60 (2022)**, which predates Cloudflare's R2 quirk handling. The patch would have been to raise the retry count; the root-cause fix was to make `bootstrap.sh` install current upstream rclone (≥1.74) and replace any detected 1.60. Clean logs confirmed it — and now a first-attempt failure means something is *actually* wrong, which is the whole point.

**The inert alert.** The fix was to make `backup.sh` publish `qincloud_backup_last_success_timestamp_seconds` as its *last* step, only on full success, and to adopt a rule: an alert isn't done until its metric has been *observed* in Prometheus returning a value below threshold. This is [[observe-what-matters]] and [[fail-loud-at-boundaries]] meeting each other — the signal has to exist before the guard on it means anything.

The drill closed the loop: `restore-drill.sh controld` fetched from R2, restored into a throwaway Postgres, found 2 user tables, and reported a **measured RTO of 4 seconds** (`runbooks/drills/2026-07-06-m2-backup-restore-drill.md`).

## 5. Going deep (systems level)

**Network isolation** (`stack/data/compose.yml`). Both services sit on `data_net` (external, private). Postgres additionally joins `tenant_db_net` so apps deployed with a database can reach it — Redis deliberately does **not** join it. Redis runs `--requirepass` because every container on `data_net` could otherwise `FLUSHALL` or read another app's keys, and `--appendonly yes` (AOF) because RDB snapshots alone can lose everything since the last save. Postgres gets `shm_size: 128mb` (the 64mb default stalls parallel workers) and an `initdb/01-controld.sh` that provisions a dedicated `controld` role — never the superuser — on a fresh volume.

**The backup path** (`scripts/backup.sh`, `set -Eeuo pipefail`, `trap … ERR`):

- Waits on the compose **healthcheck** (`docker inspect -f '{{.State.Health.Status}}'`), not merely "Running" — a `Persistent=true` catch-up run right after boot would otherwise hit a Postgres still replaying WAL (`backup.sh:40`).
- Builds the rclone remote **entirely from env vars** (`RCLONE_CONFIG_R2_*`) — no config file on disk — with `RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true` because scoped tokens lack `CreateBucket` (`backup.sh:49`).
- A single-instance `flock -n 9` guard so a slow run can't overlap the next (`backup.sh:59`).
- `pg_dump -Fc` (custom format) for every non-template database from `pg_database`, plus `pg_dumpall --globals-only` for roles/passwords — the per-DB dumps alone can't rebuild app users. Database names are regex-validated (`^[A-Za-z0-9_-]+$`) before becoming filenames.
- **`upload_verified`** (`backup.sh:70`): refuses an empty local file, `rclone copyto`, then re-lists the object with `rclone lsl` and requires a positive byte size. An empty offsite backup is a silent disaster — this is [[idempotent-self-verifying-operations]].
- Redis: `BGSAVE`, then poll `LASTSAVE` until it advances (the only reliable "snapshot done" signal), then `docker cp` the `dump.rdb`.
- **Last step, success only:** stamp the metric via the node-exporter textfile collector (`/opt/qincloud/metrics`), written tmp-then-`mv` so the collector never reads a half-written file. A failed run leaves the timestamp stale → `BackupStale` fires at 36h.

**The restore drill** (`scripts/restore-drill.sh`) is the artifact test. It picks the newest dump (excluding `_globals`, which is `pg_dumpall` SQL, not a `pg_restore`-able `-Fc` file), spins a throwaway `postgres:16.9-alpine` with a random `openssl rand -hex 16` password, restores with `--no-owner --no-privileges` (prod roles don't exist there), counts user tables, and **always** removes the container via an `EXIT` trap. It only ever *reads* R2 — it never risks the live `pgdata` volume. See [[verify-the-artifact-under-test]].

## 6. How this compares to best practice

A mature managed platform (RDS, Cloud SQL) gives you continuous WAL archiving and point-in-time recovery — an RPO measured in seconds. We chose nightly `pg_dump`: **RPO up to 24h**. For a single-box portfolio platform that's an honest, documented tradeoff; the note to revisit per-app WAL archiving when something genuinely stateful lands is written down, not forgotten.

Where we *match* best practice is the discipline most teams skip: **the tested restore with a measured RTO**, offsite storage in a different failure domain (Cloudflare, not the box), verification on every upload, and monitoring that pages when the pipeline goes quiet. Plenty of "enterprise" setups have none of those and discover it during the outage.

## 7. The underlying why (the transferable lesson)

**A backup you have never restored is not a backup — it's a hope.** The only way to know your recovery works is to *run it* and time it. Everything else in M2 is a corollary: verify each upload so "success" can't be empty ([[idempotent-self-verifying-operations]]); make backup-freshness a metric an alert can watch, not a log line nobody reads ([[observe-what-matters]]); when uploads fail deterministically, fix the old client rather than paper over it with retries ([[root-cause-over-patch]]); and prove the artifact by rebuilding from it in a throwaway ([[verify-the-artifact-under-test]]). Data durability is what finally makes [[the-box-is-disposable]] true instead of aspirational — the box can burn, because the state lives somewhere you have already proven you can read it back from. M8 later drove this drill for real; the seams to make that survivable were cut here.

---
**Teaches:** [[the-box-is-disposable]] · [[idempotent-self-verifying-operations]] · [[verify-the-artifact-under-test]] · [[observe-what-matters]] · [[root-cause-over-patch]] · [[fail-loud-at-boundaries]]
**Sources:** `stack/data/compose.yml`, `scripts/backup.sh`, `scripts/restore-drill.sh`, `runbooks/drills/2026-07-06-m2-backup-restore-drill.md`, `runbooks/gotchas/backups-r2.md`
