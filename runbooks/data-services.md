# Data services — giving apps the shared Postgres & Redis

How an app gets a database or a cache on QinCloud. One shared Postgres 16 and
one shared Redis 7 run in `stack/data`; apps never receive the platform
passwords. Instead each app gets **its own principal, named after the app**:

| Service | The app's principal | What it can reach |
| --- | --- | --- |
| Postgres | role `<app>` owning database `<app>` | its own database only (full DDL inside it) |
| Redis | ACL user `<app>` | keys/channels under the `<app>:*` prefix only; no admin/dangerous commands |

Two layers, deliberately separate: `-db` at deploy time attaches the app to
`tenant_db_net` (**reachability** — postgres and redis, nothing else), and the
credentials from `controld provision` do the **authorization**. The platform
`default`/superuser identities stay with the platform (healthcheck, backups,
exporters, controld).

`controld provision` is the single entry point. The generated password is
printed **once** and stored nowhere by the platform — you immediately hand it
to the app via `deploy -env`, which stores it in the app spec (`apps.env` in
the controld database, backed up nightly, redeploy-safe).

## New app that needs Postgres

```sh
docker exec qincloud-controld controld provision -app blog -postgres
# DATABASE_URL=postgresql://blog:<48-hex>@qincloud-postgres:5432/blog?sslmode=disable

docker exec qincloud-controld controld deploy -app blog \
  -image ghcr.io/example/blog:v1 -port 8080 -host blog.sparboard.com \
  -db -env DATABASE_URL='postgresql://blog:…@qincloud-postgres:5432/blog?sslmode=disable'
```

Dashboard path: **+ Deploy app → Advanced → check "needs the shared
Postgres/Redis" → paste the URL into the Environment textarea.**

Notes:
- The app runs its own migrations — it owns its database.
- Some images want the URL split up (`DB_HOST=qincloud-postgres`,
  `DB_USER=blog`, …) or without `?sslmode=disable`; the URL carries every
  piece, reshape to taste.
- First boot often runs migrations behind a grace-based readiness (no image
  HEALTHCHECK) — expect the first ~30s to 502 (known from umami).

## New app that needs Redis

```sh
docker exec qincloud-controld controld provision -app blog -redis
# REDIS_URL=redis://blog:<48-hex>@qincloud-redis:6379/0
```

**The app must prefix every key with `<app>:`** — the ACL is
`~blog:* &blog:*`, so an unprefixed `SET sessions 1` fails with NOPERM. Most
apps have a key-prefix/namespace setting; set it to `blog:`. An app that
cannot prefix its keys cannot use the shared Redis — give it its own small
redis container instead (deploy it as a normal app, no route).

Need both? `controld provision -app blog -postgres -redis` prints both URLs.

## What the app can and cannot do (the isolation model)

- **Postgres:** owner of its own database — full DDL/DML inside it. It cannot
  connect to any other database: provisioning revokes PUBLIC's default
  CONNECT (postgres grants CONNECT to everyone on every new database unless
  told otherwise — see `gotchas/data-services.md`).
- **Redis:** read/write only under its prefix; `+@all -@admin -@dangerous
  +info` blocks FLUSHALL/FLUSHDB/KEYS/CONFIG/SHUTDOWN and friends. `INFO` is
  deliberately re-granted: common clients send it in their connection
  handshake (ioredis ready check, BullMQ/Sidekiq version detection — BullMQ
  verifiably breaks without it). It exposes read-only server telemetry, the
  same accepted class as the SCAN caveat below. `CONFIG` stays blocked —
  `CONFIG GET requirepass` would hand a tenant the platform password.
- **Honest limits** (data isolation, not resource isolation):
  - A runaway query or hot loop in one app degrades the shared instance for
    everyone. Fine for a handful of small apps; watch `pg_stat_activity` /
    Grafana when packing more on.
  - Redis `SCAN` is not in `@dangerous`, so a tenant can enumerate other
    tenants' key **names** (never values). Acceptable here; know it.
  - No per-app storage quota. Disk is fenced by the box-level DiskUsageHigh
    alert, not per tenant.
  - When an app outgrows any of this, promote it to its own container.

## Migrating an existing app onto the shared services

**From its own sidecar postgres container** (or any reachable postgres):

```sh
# 1. provision the destination
docker exec qincloud-controld controld provision -app blog -postgres
# 2. copy the data — dump from the old, restore into the new
docker exec <old-pg> pg_dump -Fc -U <olduser> <olddb> > /tmp/blog.dump
docker exec -i qincloud-postgres pg_restore -U qincloud --no-owner --role=blog \
  -d blog < /tmp/blog.dump
# 3. roll the app onto it (stored spec now carries the new URL).
#    ⚠ deploy REPLACES the stored env WHOLESALE — repeat -env for EVERY
#    variable the spec holds (check the keys on the dashboard app page),
#    not just the one that changed; an omitted var is deleted.
docker exec qincloud-controld controld deploy -app blog -image <same> -port <same> \
  -host <same> -db -env DATABASE_URL='postgresql://blog:…/blog?sslmode=disable' \
  -env APP_SECRET='<unchanged>' # …and every other stored var
# 4. verify the app against the shared DB, then retire the sidecar
```

`--no-owner --role=blog` is load-bearing: it re-homes every object onto the
app's role regardless of who owned it in the source (same trick the M8
rebuild uses). Do the dump/restore during a quiet window; anything written to
the old database after the dump does not travel.

**From an external/managed database:** same shape — `pg_dump` from the
laptop against the external host, restore into the provisioned database.

**Redis:** a cache usually migrates by *not* migrating — deploy against the
shared Redis and let it warm. If the old data genuinely matters, it almost
certainly lacks the `<app>:` prefix, so a raw RDB copy cannot work; re-import
at the app level (or keep the sidecar).

**Apps already on the shared Postgres from the manual era (umami):** already
conformant — same role-per-app shape, provisioned by hand. The PUBLIC-connect
revoke was applied to the existing databases as one-time hardening
(2026-07-07); a future `provision -rotate` converges it automatically.

## Rotating credentials

```sh
docker exec qincloud-controld controld provision -app blog -postgres -rotate
# new DATABASE_URL — the old password is dead as of now
docker exec qincloud-controld controld deploy -app blog … -db \
  -env DATABASE_URL='<new>' -env APP_SECRET='<unchanged>' # …and every other stored var
```

**⚠ `deploy` replaces the stored env wholesale** — pass `-env` for every
variable in the spec, not only the rotated one; an omitted var is deleted
from the spec and the next container boots without it. Check the current
keys on the dashboard app page before rotating. If you already wiped one:
values live in last night's `postgres/controld/` dump, or re-key that
service with another `-rotate`.

Between the rotate and the redeploy the app's *established* connections keep
working (both postgres and redis check credentials at connect time), but any
reconnect fails — rotate and redeploy in one sitting. `-rotate` **replaces**
the principal's password (redis: the whole user, via `reset`); it never
accumulates old ones.

Rotating the **platform** redis password: update `REDIS_PASSWORD` in `.env`,
then recreate redis (`cd stack/data && docker compose --env-file
/opt/qincloud/.env up -d --force-recreate redis`) — the `redis-acl-init`
one-shot rewrites the default user's aclfile line from `.env`, which is the
single owner of that password. Why it works this way (and why an aclfile
without a default line means NO auth at all): the aclfile entry in
`gotchas/data-services.md`.

## Deprovisioning (manual, on purpose)

`controld destroy` removes the app, its route, and its containers — **it
never touches data**. That asymmetry is deliberate: destroy + redeploy is
data-safe, and deleting data is a human decision:

```sh
docker exec qincloud-controld controld destroy -app blog
printf 'DROP DATABASE blog;\nDROP ROLE blog;\n' \
  | docker exec -i qincloud-postgres psql -U qincloud -d postgres -v ON_ERROR_STOP=1
docker exec qincloud-redis redis-cli ACL DELUSER blog
docker exec qincloud-redis redis-cli ACL SAVE
```

(Re-provisioning an app whose role still exists refuses and points at
`-rotate` — which re-keys the principal and leaves the data in place.)

## DR — what restores what

| R2 object | Restores |
| --- | --- |
| `postgres/<app>/…dump.gz` | the app's data |
| `postgres/_globals/…sql.gz` | roles + password hashes → stored `DATABASE_URL`s keep working |
| `redis-acl/…acl.gz` | redis ACL users + hashes → stored `REDIS_URL`s keep working |
| `redis/…rdb.gz` | cache contents (usually optional) |
| `postgres/controld/…` | app specs, including each app's env URLs |

Two DR steps are easy to miss because the dumps do NOT carry them:

1. **Tenant databases must be re-created before their restore.** A fresh
   initdb has only `controld`; per-database dumps are restored *into* an
   existing database (no `--create`), and the PUBLIC-connect revoke is a
   `pg_database` ACL that neither the per-DB dumps nor the globals dump
   carries. For each tenant database in the R2 `postgres/` listing (roles
   already exist from the globals restore):

   ```sh
   docker exec qincloud-postgres psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 \
     -c 'CREATE DATABASE <app> OWNER <app>' \
     -c 'REVOKE CONNECT ON DATABASE <app> FROM PUBLIC'
   # then the pg_restore from README step 6
   ```

2. **Restore the redis ACL file** alongside the database restores:

   ```sh
   gunzip redis-acl_<newest>.acl.gz
   docker cp redis-acl_<newest>.acl qincloud-redis:/data/users.acl
   docker exec qincloud-redis redis-cli ACL LOAD
   ```

   The `redis-acl/` prefix is legitimately absent when no tenant redis user
   was ever provisioned (backup.sh skips an empty `users.acl`). Disambiguate
   against the restored specs: if no app's env carries a `REDIS_URL`, skip
   this step; if one does and the prefix is missing, the backup pipeline was
   broken — re-key with `provision -app <x> -redis -rotate` and redeploy.

Everything else is the standard rebuild: specs restore with the controld
database, `controld redeploy` brings each app back with its env intact.

## Reserved names

`default`, `controld`, `qincloud`, `postgres`, `template0`, `template1` are
platform principals — `provision` refuses them. This is what stops
`provision -app qincloud -rotate` from re-keying the cluster superuser.
