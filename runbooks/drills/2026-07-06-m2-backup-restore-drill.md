# Drill: M2 backup → R2 → restore (2026-07-06)

**Goal:** first real offsite backup, a rehearsed restore with a measured RTO,
and a monitored pipeline (a backup that silently stops must page).

## What ran

| Step | Result |
| --- | --- |
| `backup.sh` manual run | exit 0 — 3 postgres dumps (`controld`, `postgres`, `qincloud`) + `pg_globals` + redis RDB, all uploaded to `r2:qcloud` and size-verified |
| `qincloud-backup.timer` | installed + enabled; nightly 03:00 WIB, `Persistent=true` |
| `restore-drill.sh controld` | fetch from R2 → throwaway postgres → `pg_restore` → sanity: 2 user tables. **RTO = 4s** |
| `BackupStale` alert | now armed: `backup.sh` publishes `qincloud_backup_last_success_timestamp_seconds` via the node-exporter textfile collector (`/opt/qincloud/metrics`); fires at 36h staleness |

RPO with the current schedule: up to 24h (nightly). Fine for a portfolio
platform; revisit per-app when something stateful matters.

## Found during setup

1. **noble's apt rclone (1.60, 2022) predates R2 quirk handling** — every
   upload failed its first attempt with `501 NotImplemented` and succeeded on
   rclone's internal retry. Deterministic noise like that would eventually
   mask a real failure. Root cause fixed: `bootstrap.sh` now installs current
   upstream rclone (1.74.x) instead of the apt package; clean logs confirmed.
2. **The R2 S3 keypair is derived, not displayed**, when all you have is an
   API token value: Access Key ID = the token ID (`GET /user/tokens/verify`),
   Secret Access Key = SHA-256 hex of the token value.
3. **An alert on a metric that doesn't exist is a silent no-op** — the
   `BackupStale` rule shipped inert in M3 because its metric was TBD. Rule of
   thumb: an alert isn't done until its metric has been observed in
   Prometheus and the expr returns a value below threshold.
