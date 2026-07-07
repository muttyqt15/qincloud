# Gotchas — shared Postgres/Redis tenancy (`controld provision`)

Operator guide: [`../data-services.md`](../data-services.md). These are the
sharp edges under it.

## psql `-c` does NOT interpolate `-v` variables

- **Symptom:** a role whose password is the literal string `:'pw'`.
- **Rule:** SQL that uses psql variables (`:'pw'`, `:"name"`) must arrive on
  **stdin** (`-f -` or a heredoc), never via `-c`. Provisioning does this
  (`internal/provision/provision.go` → `pgExec`); the manual flow in
  `stack/data/initdb/01-controld.sh` does too.

## Postgres grants PUBLIC CONNECT on every new database

- **Symptom:** tenant role `blog` can `\c umami` — or `\c template1` (table
  privileges still stop reads, but the cross-connect is real, and an idle
  tenant connection parked on template1 blocks every future CREATE DATABASE).
- **Rule:** every tenant database gets `REVOKE CONNECT ON DATABASE x FROM
  PUBLIC`. `provision -postgres` does it on create and re-converges it on
  `-rotate`. The platform databases (`controld`, `postgres`, the superuser's,
  `template1`) are hardened by `stack/data/initdb/01-controld.sh` on every
  fresh volume; the same revokes were applied one-time to the live box
  2026-07-07 (incl. `umami`).
- **DR:** the revoke is a `pg_database` ACL — per-DB dumps (no `--create`)
  and the globals-only dump do NOT carry it. On a rebuild every tenant
  database must be re-created WITH the revoke before its `pg_restore`
  (README step 6 / `../data-services.md` DR section).

## `CREATE DATABASE` has no `IF NOT EXISTS` and cannot run in a transaction

- **Rule:** existence is checked first (`pg_database`), and when the name is
  taken the **owner must match the app** — provision refuses to adopt a
  database owned by anyone else. `psql -f` runs statements one at a time, so
  CREATE DATABASE and the revoke still share one round-trip.

## Redis refuses to boot when its configured aclfile is missing

- **Symptom:** after adding `--aclfile`, redis crash-loops on a fresh volume
  (new box, M8 rebuild) with "aborting" in its log.
- **Rule:** the `redis-acl-init` one-shot service in `stack/data/compose.yml`
  touches `/data/users.acl` before redis starts
  (`depends_on: service_completed_successfully`). Idempotent — touch never
  truncates provisioned users. Don't remove it as "unused".

## An aclfile REPLACES the whole user table — no default line = NO AUTH

- **Symptom (live, 2026-07-07 rollout):** redis booted with `--requirepass`
  AND `--aclfile` pointing at an *empty* users.acl → the default user came up
  **passwordless**. `requirepass` is ignored entirely once an aclfile is
  configured; at boot the file replaces the whole user table, and a file
  with no `user default` line resets default to its built-in `on nopass`.
  The review missed this; the post-rollout verification caught it.
- **Rule:** the aclfile must ALWAYS carry a `user default on #<sha256> …`
  line. The `redis-acl-init` one-shot in `stack/data/compose.yml` rewrites
  exactly that line from `REDIS_PASSWORD` on every `up`, so **.env is the
  single owner of the platform password** and tenant lines are untouched.
  `--requirepass` was removed as dead config — a knob that looks like the
  password owner but isn't is how a rotation silently doesn't happen.
- **Rotating `REDIS_PASSWORD`:** update `.env`, then
  `cd stack/data && docker compose --env-file /opt/qincloud/.env up -d
  --force-recreate redis` — the init converges the default line and the
  container comes back with matching `REDISCLI_AUTH` (healthcheck, backup.sh,
  and provisioning execs all read it). Never rotate with a bare
  `ACL SETUSER default '>NEWPW'`: `>pw` APPENDS a password, the old one
  stays valid forever.

## Secrets cross the exec boundary as env / hashes, never argv

- psql gets the password via exec-process env consumed by `-v pw="$QC_PW"`;
  redis gets only the **sha256 hash** (`ACL SETUSER … #<hex>`). Keep it that
  way — argv is visible to `docker inspect` and in-container `ps`.
- `docker exec` inherits the container's own env, which is why provisioning
  needs **no new platform secrets**: `POSTGRES_USER` (local-socket trust) and
  `REDISCLI_AUTH` are already inside the data containers — the same mechanism
  `scripts/backup.sh` has always used.

## Redis ACL `~prefix` protects values, not key names

- `SCAN` is not in `@dangerous`, so any tenant can enumerate all key
  **names**. Values stay confined to `~<app>:*`. Accepted for this platform
  (single-operator, low-sensitivity caches) — revisit with `-scan` per user
  if a tenant ever stores sensitive key names.

## `-@dangerous` alone breaks common redis clients at connect

- **Symptom:** an app with a fresh provisioned `REDIS_URL` never comes up;
  its logs show `NOPERM ... 'info'`.
- **Root cause:** `@dangerous` contains `INFO`, and common clients send INFO
  in their connection handshake (ioredis ready check, BullMQ/Sidekiq version
  detection — BullMQ verified broken end-to-end without it).
- **Rule:** the tenant ACL is `+@all -@admin -@dangerous +info` (order
  matters — rules apply left to right, the trailing `+info` re-grants it).
  Never re-grant `CONFIG`: `CONFIG GET requirepass` hands a tenant the
  platform password. Users provisioned with an older rule converge on
  `-rotate` (SETUSER `reset` re-applies the full list).

## `users.acl` is state, not config

- It lives in the `redisdata` **volume** (written by `ACL SAVE` at runtime),
  is backed up nightly to R2 under `redis-acl/`, and is restored on rebuild
  with `docker cp … && redis-cli ACL LOAD` (README step 6). Never turn it
  into a bind-mounted repo file — rsync would clobber provisioned users.
