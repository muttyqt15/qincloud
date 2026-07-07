# QinCloud

A mini-PaaS on a single 4-vCPU VPS, operated like production: real deploys,
real TLS, real SLOs, a real pager, and a paper trail of every drill, outage,
and mistake. I built it to practice SRE the only way that sticks — against a
live system where a bad deploy takes down my own apps.

Custom code is **one Go service** (`controld/`, ~5k lines with its tests).
Everything else
is vetted off-the-shelf software — Caddy, Postgres, Redis, Prometheus,
Grafana, Loki, Alertmanager — wired together by the config in this repo. The
rule was: build only the piece worth learning from, buy everything else.

## Live

| URL | What |
| --- | --- |
| [notes.sparboard.com](https://notes.sparboard.com) | **Start here** — the learnings vault: every milestone written up as what-I-planned / what-broke / why, plus the concepts underneath. Published straight from [`learnings/`](learnings/). |
| [whoami.sparboard.com](https://whoami.sparboard.com) | Demo app, deployed by controld |
| [analytics.sparboard.com](https://analytics.sparboard.com) | Umami — first real tenant, on the shared Postgres |
| [dash.sparboard.com](https://dash.sparboard.com) | controld's dashboard (basic-auth; Cloudflare-fronted, origin locked to CF IPs) |

## Architecture

```
          internet  (sparboard.com, *.sparboard.com)
                       │ 80/443 (only public ports)
                 ┌─────▼─────┐
                 │   Caddy   │  auto-TLS, routing, JSON access logs
                 │  (edge)   │  admin API on unix socket; :2019 metrics-only
                 └─────┬─────┘
              app_net  │
        ┌──────────┬───┴──────┬────────────┐
   ┌────▼───┐ ┌────▼───┐ ┌────▼─────┐ ┌────▼──────────────┐
   │ app A  │ │ app B  │ │ controld │ │ Grafana/Prometheus│◄─ Tailscale
   └────┬───┘ └────┬───┘ │ (Go)     │ │ Loki/Alertmanager │   only
        │ data_net │     └────┬─────┘ └───────────────────┘
   ┌────▼──────────▼──────────▼────┐
   │       Postgres · Redis        │  never published publicly
   └───────────────┬───────────────┘
                   │ pg_dump nightly
              ┌────▼────┐
              │   R2    │  offsite backups
              └─────────┘
```

## controld — the control plane

The one piece I wrote instead of bought. A single Go binary (Docker SDK,
Caddy admin API, pgx, templ + htmx — no JS framework) that owns the app
lifecycle:

- **Deploys as a persisted state machine** — `pending → pulling → starting →
  routing → live`, every transition written to Postgres before the step runs.
  The old container keeps serving until the new one is ready *and* routed, so
  a bad image never takes an app down. Per-app advisory locks stop two
  deploys from eating each other ([`deploy.go`](controld/internal/deploy/deploy.go)).
- **Image-aware deploys** — the dashboard pulls and inspects the image you
  name, verifies it exists, and reads the container port off its `EXPOSE`
  metadata so you don't guess.
- **Tenant provisioning** — `controld provision -app X -postgres -redis`
  mints per-app credentials on the shared data services: a Postgres role +
  database and a Redis ACL user fenced to the app's own key prefix. Secrets
  cross into the containers as exec env and password *hashes*, never argv
  ([`provision.go`](controld/internal/provision/provision.go)).
- **Its own observability** — controld exports `qincloud_app_up` and per-app
  resource gauges; the availability SLO and the fast `AppDown` page are built
  on them because deployed apps aren't scrape targets.

## The paper trail

The part of this repo I'd actually show an interviewer. Every alert has been
*seen* to fire, every restore has been rehearsed, and every mistake became a
written rule:

- **Full box rebuild, measured**: wipe → serving again in ≈12 min
  ([drill record](runbooks/drills/2026-07-06-m8-box-rebuild-drill.md)).
- **Pager drills**: real outage → Prometheus → Alertmanager → Discord page →
  resolve, twice ([M3](runbooks/drills/2026-07-06-m3-pager-drill.md),
  [AppDown](runbooks/drills/2026-07-07-appdown-pager-drill.md)).
- **SLOs done properly**: 99.5%/30d availability with multiwindow-multiburn
  error-budget alerts, Google SRE style
  ([rules](stack/observability/prometheus/rules/slo.rules.yml)).
- **[`runbooks/gotchas/`](runbooks/gotchas/)**: the "don't repeat this" files,
  one per domain, updated in the same commit as the fix. My favorite entry:
  a Redis aclfile with no `default` line silently disables auth entirely —
  found by post-rollout verification minutes after a 26-agent adversarial
  review missed it ([data-services](runbooks/gotchas/data-services.md)).
- **[`learnings/`](learnings/)**: the Obsidian teaching vault behind
  notes.sparboard.com — 13 milestone write-ups, 11 concept notes, all
  linked.

## Repo layout

```
qincloud/
├── controld/           # Go control plane — the only custom code
├── stack/              # docker compose stacks, one project per concern
│   ├── edge/           # Caddy: TLS, routing, access logs
│   ├── data/           # Postgres + Redis, private networks only
│   ├── controld/       # runs the control plane
│   └── observability/  # Prometheus, Grafana, Loki, Alertmanager
├── scripts/            # bootstrap, backup, restore drill, self-verifying deploys
├── runbooks/           # drills, postmortem template, per-app runbooks
│   └── gotchas/        # living per-domain rules; update in the same commit as the fix
├── learnings/          # the teaching vault (published at notes.sparboard.com)
└── sites/notes/        # Quartz build of learnings/ → static site container
```

Each `stack/*` dir is an independent compose project joined by external
bridges, so one stack can restart without touching the others.

## Invariants

1. **Config in git, state in volumes + R2, secrets in gitignored `.env`**
   (`.env.example` documents the names). Never commit a running system.
2. **Only Caddy publishes public ports** (80/443). Docker-published ports
   bypass UFW, so the rule is "don't publish", not "firewall it later".
3. **Admin surfaces** (Grafana, Prometheus, controld) bind to the Tailscale
   IP only — with one deliberate exception: the controld dashboard is also
   published at https://dash.sparboard.com behind Caddy `basic_auth`,
   proxied over the `admin_net` bridge that carries only caddy + controld
   + prometheus.
4. **The box is disposable.** `bootstrap.sh` + `git clone` + restore-from-R2
   must rebuild it from zero. Never hand-edit the running system.

## Bootstrap a fresh box

```sh
scp scripts/bootstrap.sh root@<box>:/root/
ssh root@<box> 'bash /root/bootstrap.sh'
ssh root@<box> 'tailscale up'        # open the printed auth URL
```

## Rebuild from zero (invariant #4 as a procedure)

Order matters; each step fails loud if a dependency is missing. Rehearsed
for real — see the [drill record](runbooks/drills/2026-07-06-m8-box-rebuild-drill.md).

1. **Host baseline** — bootstrap + Tailscale (section above). Creates the
   `app_net`/`data_net` bridges every stack joins.
2. **Repo** — clone this repo to `/opt/qincloud`, or rsync from a laptop
   working copy. The off-box copy is what makes this step possible.
3. **Secrets** — recreate the gitignored pieces:
   - `.env` from `.env.example` — **reuse the ORIGINAL secret values** (from
     your password manager), don't generate fresh ones. Step 6 restores
     `pg_globals`, which resets every role password to the *backed-up*
     values; a freshly generated `POSTGRES_PASSWORD` would leave controld
     and both exporters failing auth while the postgres healthcheck stays
     green (it checks over local trust).
   - `install -o 65534 -g 65534 -m 400 <webhook-file> /opt/qincloud/secrets/discord_webhook`
4. **stack/edge** — up first: it creates the `caddy_admin` volume (admin
   socket) that stack/controld mounts.
5. **stack/data** — up; wait for the postgres healthcheck.
6. **Restore from R2** — the real restore is manual by design
   (`restore-drill.sh` only rehearses into a throwaway container, never the
   real cluster). Fetch the newest `postgres/_globals/` and per-database
   `postgres/<db>/` objects (rclone env config as in `backup.sh`), `psql`
   the globals (errors on pre-existing roles are noise; the ALTER ROLEs
   reset passwords to the backed-up values). Restores go INTO an existing
   database — not `--create` — and a fresh initdb only has `controld`, so
   first re-create each TENANT database with its lockdown (the
   PUBLIC-connect revoke is a pg_database ACL no dump carries):
   `psql -c 'CREATE DATABASE <app> OWNER <app>' -c 'REVOKE CONNECT ON DATABASE <app> FROM PUBLIC'`.
   Then per database:
   `pg_restore --clean --if-exists --no-owner --role=<owning-role> -d <db>`
   (`--no-owner --role=controld` also migrates dumps taken under an older
   superuser-owned layout onto the dedicated role).
   Also restore the redis ACL users (per-app credentials from `controld
   provision -redis` — without them every stored `REDIS_URL` fails auth):
   fetch the newest `redis-acl/` object, then
   `docker cp <users.acl> qincloud-redis:/data/users.acl && docker exec
   qincloud-redis redis-cli ACL LOAD`. (`redis-acl/` legitimately absent =
   no tenant redis user ever provisioned; if a restored spec carries a
   `REDIS_URL` yet the prefix is missing, re-key via `provision -redis
   -rotate` — see `runbooks/data-services.md` DR section.)
7. **stack/observability** — up; then install the backup schedule:
   `cp scripts/systemd/qincloud-backup.* /etc/systemd/system/ && systemctl daemon-reload && systemctl enable --now qincloud-backup.timer`
8. **stack/controld** — up (`--build`). `controld list` shows which apps the
   restored database says should exist.
9. **Redeploy every app** — `controld redeploy -app <name>` for each app in
   `controld list`: the rebuilt Caddy has no autosave so no app routes exist
   yet, and the app containers are gone — a redeploy recreates both from the
   restored spec (env included; never re-type `-env` values by hand).
10. **Close public ssh** — `scripts/close-public-ssh.sh` (refuses to run
    unless tailscale is up). From here sshd answers on the tailnet only;
    recovery without tailscale = provider web console → `ufw limit 22/tcp`.

## Milestones

| #   | What                                                        | Status |
| --- | ----------------------------------------------------------- | ------ |
| M0  | Host baseline: UFW, fail2ban, sshd, Docker, Tailscale        | ✅     |
| M1  | Edge: Caddy auto-TLS + admin API                             | ✅     |
| M2  | Data: Postgres/Redis + nightly pg_dump → R2                  | ✅ nightly timer live; BackupStale alert armed |
| M3  | Observability: Prometheus, Grafana, Loki, Alertmanager       | ✅ pager drill 2026-07-06: real outage → Discord page → resolved |
| M4  | controld core: Docker SDK, Caddy client, deploy state machine| ✅ deploy/list/destroy live; whoami e2e; auto-TLS on sparboard.com |
| M5  | controld dashboard (templ + htmx)                            | ✅ apps/status/history/stats/logs + deploy/redeploy/destroy; tailnet :8600 + https://dash.sparboard.com (basic auth) |
| M6  | Onboard first real app                                       | ✅ Umami analytics live at analytics.sparboard.com ([runbook](runbooks/apps/umami.md)); shared-Postgres tenancy |
| M7  | SLOs + error-budget burn alerts                              | ✅ 99.5% availability SLO, multiwindow-multiburn alerts + fast AppDown/PostgresDown/RedisDown, Grafana SLO board |
| M8  | DR rehearsal: restore drill, measured RTO/RPO                | ✅ full box rebuild 2026-07-06: wipe → serving in ≈12 min ([drill](runbooks/drills/2026-07-06-m8-box-rebuild-drill.md)) |
| M9  | Failure drills + blameless postmortems                       | ✅ postmortem template + failure-injection catalogue; AppDown pager drill 2026-07-07 |
| M10 | Agent-ops                                                    | next: a scheduled "platform doctor" (Prometheus/Loki → Discord health summary) |

## License

[MIT](LICENSE)
