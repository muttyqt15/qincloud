# Backups / R2 gotchas

Evidence: [drills/2026-07-06-m2-backup-restore-drill.md](../drills/2026-07-06-m2-backup-restore-drill.md)

## The R2 S3 keypair is derived, not displayed

Given only an R2 API token value (Cloudflare shows the S3 keypair once, at
creation):

- **Access Key ID** = the token's ID — `GET https://api.cloudflare.com/client/v4/user/tokens/verify` with `Authorization: Bearer <token>` returns it.
- **Secret Access Key** = SHA-256 hex of the token value: `printf %s "$TOKEN" | sha256sum`.

Rotation is therefore two `.env` lines, no console round-trip for the secret.

## Distro rclone is too old for R2

Ubuntu noble ships rclone 1.60 (2022), which predates R2 quirk handling —
**every** upload fails its first attempt with `501 NotImplemented` and
succeeds on internal retry. Deterministic noise like that will eventually mask
a real failure. `scripts/bootstrap.sh` installs upstream rclone (≥1.74) and
replaces a detected 1.60; keep it that way.

## `rclone rcat` always 501s on R2

Streaming uploads (unknown content length) are not accepted; `copyto` with a
real local file is fine. Never pipe dumps straight to rclone — write the file,
verify it's non-empty, then `copyto` (which is what `backup.sh` does, plus a
post-upload size check via `lsl`).

## Scoped tokens lack CreateBucket

rclone probes bucket existence by default and fails on a bucket-scoped token.
`RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true` (set in `backup.sh`).

## Restore rule: globals reset role passwords to the *backed-up* values

`pg_globals` restore rewrites every role password. On a box rebuild, `.env`
must reuse the **original** secret values from the password manager — a
freshly generated `POSTGRES_PASSWORD` leaves controld and both exporters
failing auth while the postgres healthcheck stays green (it checks over local
trust). Full order-of-operations: README → "Rebuild from zero".

## Backup success is a published metric, not a log line

`backup.sh` publishes `qincloud_backup_last_success_timestamp_seconds` via the
textfile collector **only on full success**; `BackupStale` fires at 36h. If
you change the backup script, the metric write must stay the last step — a
partial run that still stamps success would blind the alert.
