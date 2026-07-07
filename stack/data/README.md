# stack/data/ — Postgres + Redis, shared and fenced

The stateful heart of the platform: **one Postgres and one Redis**, shared by
the control plane and every tenant app. A one-box PaaS cannot afford a database
per app, so everyone shares one instance — safely, by giving each app its own
credential that can only touch its own data.

## Why shared (and why it's safe)

Sharing conflates two questions that must be split:

- **Can the app reach the server?** — the network (`tenant_db_net`), joined via
  `-db` at deploy. Reachability only.
- **What may it do there?** — a **per-app principal**: a Postgres role that owns
  database `<app>` (with `PUBLIC`'s default CONNECT revoked, so no
  cross-database hops), and a Redis ACL user fenced to the `<app>:*` key prefix
  with admin/dangerous commands stripped.

One command mints both: `controld provision -app X -postgres -redis`
([`../../controld/internal/provision/`](../../controld/internal/provision/)).
The password is generated once, printed once, and stored only in the app's own
spec — the platform keeps no copy. The full model is
[shared data-services tenancy](../../learnings/concepts/shared-data-services-tenancy.md); the operator guide is
[`../../runbooks/data-services.md`](../../runbooks/data-services.md).

## Mental model: the aclfile *replaces*, it doesn't *merge*

The single most dangerous thing in this folder, found live minutes after a
26-agent review missed it. With `--aclfile` configured, **Redis replaces its
entire user table from that file at boot.** An *empty* aclfile therefore resets
the default user to `nopass` — no authentication at all — and `--requirepass` is
*silently ignored*. For a few minutes the platform Redis had no auth.

The fix is structural: a `redis-acl-init` one-shot container rewrites the
`user default on #<sha256(REDIS_PASSWORD)>` line from `.env` on **every** `up`,
making `.env` the single owner of the platform password (`--requirepass` was
deleted as a decoy, not left dangling). Redis then depends on that init
completing successfully. If you touch the Redis config, read
[`../../runbooks/gotchas/data-services.md`](../../runbooks/gotchas/data-services.md)
first — the aclfile semantics, `>pw`-appends-not-replaces, and why `+info` must
follow `-@dangerous` (BullMQ/ioredis send `INFO` at connect) are all there.

## What's in here

- `compose.yml` — Postgres 16 + Redis 7, both on private nets only, the
  `redis-acl-init` one-shot, healthchecks, and fail-loud `${VAR:?}` secret
  interpolation (which fires on `down` too — a real DR surprise, see M8).
- `initdb/` — first-boot SQL/shell that runs *only* on a fresh data volume:
  creates the `controld` role/DB and applies the lockdown REVOKEs (PUBLIC
  CONNECT, template1, etc.) that harden a brand-new cluster.

## How it interacts

- **controld** reads/writes its own `controld` database over `data_net`, and
  reaches *into* the Postgres/Redis containers (via `docker exec`) to run
  provisioning as the trusted local user.
- **tenant apps** reach Postgres/Redis over `tenant_db_net` using their
  provisioned credential.
- **observability** scrapes `postgres_exporter` / `redis_exporter` (the
  `pg_up` / `redis_up` signals behind the `PostgresDown` / `RedisDown` alerts).
- **scripts/backup.sh** runs the nightly `pg_dump` → R2 and backs up the Redis
  `users.acl`.

## SRE concepts here

- **Multi-tenancy by least privilege** — data isolation via per-principal
  credentials, not per-app servers. Honest limit: it's *data* isolation, not
  *resource* isolation — a runaway query still hurts neighbors, and `SCAN` leaks
  key *names* (never values).
- **Backups are worthless until restored** — the pg_database CONNECT revoke is
  an ACL that *no dump carries*, so DR must re-create tenant DBs and re-apply the
  revoke before restoring. Rehearsed in [M8 — disaster recovery](../../learnings/milestones/m8-disaster-recovery.md).
