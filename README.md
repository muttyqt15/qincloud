# QinCloud

A self-hosted mini-PaaS on one VPS, built as an SRE practice ground: deploys,
routing, observability, SLOs, backups, and incident response — operated like
production.

Custom code is **one Go service** (`controld/`). Everything else is vetted
off-the-shelf software, wired together by the config in this repo.

## Architecture

```
                    internet
                       │ 80/443 (only public ports)
                 ┌─────▼─────┐
                 │   Caddy   │  auto-TLS, routing, JSON access logs
                 │  (edge)   │  admin API on 127.0.0.1:2019
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
├── stack/              # docker compose stacks, one project per concern
│   ├── edge/           # Caddy: TLS, routing, access logs (M1)
│   ├── data/           # Postgres + Redis, private networks only (M2)
│   └── observability/  # Prometheus, Grafana, Loki, Alertmanager (M3)
├── scripts/            # bootstrap.sh, backup.sh, restore-drill.sh
├── runbooks/           # runbooks, drills, postmortems — the SRE paper trail
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

## Milestones

| #   | What                                                        | Status |
| --- | ----------------------------------------------------------- | ------ |
| M0  | Host baseline: UFW, fail2ban, sshd, Docker, Tailscale        | ✅     |
| M1  | Edge: Caddy auto-TLS + admin API                             | —      |
| M2  | Data: Postgres/Redis + nightly pg_dump → R2                  | —      |
| M3  | Observability: Prometheus, Grafana, Loki, Alertmanager       | —      |
| M4  | controld core: Docker SDK, Caddy client, deploy state machine| —      |
| M5  | controld dashboard (templ + htmx)                            | —      |
| M6  | Onboard first real app                                       | —      |
| M7  | SLOs + error-budget burn alerts                              | —      |
| M8  | DR rehearsal: restore drill, measured RTO/RPO                | —      |
| M9  | Failure drills + blameless postmortems                       | —      |
| M10 | Agent-ops                                                    | —      |
