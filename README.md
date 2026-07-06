# QinCloud

A self-hosted mini-PaaS on one VPS, built as an SRE practice ground: deploys,
routing, observability, SLOs, backups, and incident response — operated like
production.

Custom code is **one Go service** (`controld/`). Everything else is vetted
off-the-shelf software, wired together by the config in this repo.

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

## Repo layout

```
qincloud/
├── controld/           # Go control plane — the only custom code (M4+)
│                       #   serve = /healthz + web dashboard on :8600 (M5)
├── stack/              # docker compose stacks, one project per concern
│   ├── edge/           # Caddy: TLS, routing, access logs (M1)
│   ├── data/           # Postgres + Redis, private networks only (M2)
│   └── observability/  # Prometheus, Grafana, Loki, Alertmanager (M3)
├── scripts/            # bootstrap.sh, backup.sh, restore-drill.sh
├── runbooks/           # runbooks, drills, postmortems — the SRE paper trail
│   └── gotchas/        # living per-domain rules; update in the same commit as the fix
└── README.md
```

Each `stack/*` dir is an independent compose project joined by the external
`app_net` / `data_net` bridges, so one stack can restart without touching
the others.

## Invariants

1. **Config in git, state in volumes + R2, secrets in gitignored `.env`**
   (`.env.example` documents the names). Never commit a running system.
2. **Only Caddy publishes public ports** (80/443). Docker-published ports
   bypass UFW, so the rule is "don't publish", not "firewall it later".
3. **Admin surfaces** (Grafana, Prometheus, controld) bind to the Tailscale
   IP only.
4. **The box is disposable.** `bootstrap.sh` + `git clone` + restore-from-R2
   must rebuild it from zero. Never hand-edit the running system.

## Bootstrap a fresh box

```sh
scp scripts/bootstrap.sh root@<box>:/root/
ssh root@<box> 'bash /root/bootstrap.sh'
ssh root@<box> 'tailscale up'        # open the printed auth URL
```

## Rebuild from zero (invariant #4 as a procedure)

Order matters; each step fails loud if a dependency is missing.

1. **Host baseline** — bootstrap + Tailscale (section above). Creates the
   `app_net`/`data_net` bridges every stack joins.
2. **Repo** — clone (or rsync) this repo to `/opt/qincloud`.
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
   reset passwords to the backed-up values), then restore each database
   INTO the one initdb already created — not `--create`:
   `pg_restore --clean --if-exists --no-owner --role=<owning-role> -d <db>`
   (`--no-owner --role=controld` also migrates dumps taken under an older
   superuser-owned layout onto the dedicated role).
7. **stack/observability** — up; then install the backup schedule:
   `cp scripts/systemd/qincloud-backup.* /etc/systemd/system/ && systemctl daemon-reload && systemctl enable --now qincloud-backup.timer`
8. **stack/controld** — up (`--build`). `controld list` shows which apps the
   restored database says should exist.
9. **Redeploy every app** — `controld deploy` each one: the rebuilt Caddy has
   no autosave so no app routes exist yet, and the app containers are gone —
   a deploy recreates both.
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
| M5  | controld dashboard (templ + htmx)                            | ✅ apps/status/history + deploy/redeploy/destroy on :8600 (tailnet) |
| M6  | Onboard first real app                                       | ✅ Umami analytics live at analytics.sparboard.com ([runbook](runbooks/apps/umami.md)); AppSpec env + tenant_db_net |
| M7  | SLOs + error-budget burn alerts                              | —      |
| M8  | DR rehearsal: restore drill, measured RTO/RPO                | ✅ full box rebuild 2026-07-06: wipe → serving in ≈12 min ([drill](runbooks/drills/2026-07-06-m8-box-rebuild-drill.md)) |
| M9  | Failure drills + blameless postmortems                       | —      |
| M10 | Agent-ops                                                    | —      |
