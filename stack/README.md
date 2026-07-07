# stack/ — the docker compose stacks

Everything QinCloud *runs* is defined here as Docker Compose. Each subfolder is
an **independent compose project** — its own `compose.yml`, brought up and down
on its own — joined to the others only by shared **external networks** (Docker
bridges created once by `bootstrap.sh`). That independence is the whole point:
you can restart the edge without touching the databases, or rebuild the control
plane without disturbing observability.

## The four stacks

| Stack | What runs | Public? |
| --- | --- | --- |
| [`edge/`](edge/) | **Caddy** — the one front door. Auto-TLS, routing, access logs. | Yes — the *only* stack that publishes 80/443 |
| [`data/`](data/) | **Postgres + Redis** — shared datastores for controld and tenant apps. | No — private networks only |
| [`controld/`](controld/) | The **control plane** container (builds [`../controld/`](../controld/)). | Dashboard on Tailscale + behind Caddy |
| [`observability/`](observability/) | **Prometheus, Grafana, Loki, Alertmanager, exporters** — metrics, logs, alerts. | No — Tailscale-only |

## Mental model: reachability vs authorization

The stacks are wired by a small set of Docker bridges, and the key idea is that
**being on a network grants *reachability*, not *permission*.**

```
app_net        apps ↔ Caddy (so the edge can proxy to them)
data_net       controld ↔ Postgres/Redis (the control plane's own DB)
tenant_db_net  postgres + redis + apps opted in with `-db` at deploy
admin_net      caddy + controld + prometheus only (fixed subnet; gates :2019 metrics)
```

An app joined to `tenant_db_net` can *reach* Postgres — but it still needs a
per-app credential (minted by `controld provision`) to *do* anything. The
network is the hallway; the credential is the room key. This split is the
subject of [shared data-services tenancy](../learnings/concepts/shared-data-services-tenancy.md).

## The one invariant that shapes everything

**Only the edge stack publishes public ports.** Docker's published ports bypass
the UFW firewall (they're inserted into iptables ahead of it), so the rule is
"don't publish," not "firewall it later." Every other stack binds either to the
Tailscale IP (`${TS_IP}`) or to nothing at all — reachable only over private
bridges or the tailnet. If you ever find yourself adding a `ports:` mapping to a
non-edge stack, stop: that's the invariant breaking.

## Ordering (it matters)

Stacks have a boot order because they create resources each other mount:

1. **edge** first — it creates the `caddy_admin` volume (the admin socket
   controld mounts to add routes).
2. **data** — wait for the Postgres healthcheck.
3. **observability** — metrics/logs/alerts.
4. **controld** (`--build`) — needs the admin socket from step 1 and the DB
   from step 2.

A stack started before a shared resource existed must be **recreated**
(`up -d --force-recreate`), not just reloaded — a bind mount pins the old layout
otherwise. The full ordered procedure is the README's "Rebuild from zero" and
[M8 — disaster recovery](../learnings/milestones/m8-disaster-recovery.md).

## Working with a stack

```sh
# each command targets ONE project by its directory
docker compose --project-directory /opt/qincloud/stack/<name> --env-file /opt/qincloud/.env up -d
```

Secrets come from the gitignored `/opt/qincloud/.env` (documented by name in
`.env.example`) and `secrets/` — never hard-coded, never committed.
