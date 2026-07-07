# stack/controld/ — running the control plane

This stack **builds and runs** the control plane. The *code* lives in
[`../../controld/`](../../controld/); this folder is just how it's containerised
and wired onto the box. Small on purpose — one service, two mounts, one port.

## What the compose file grants (and why it's the whole security story)

controld exists to hold exactly two capabilities, and the `volumes:` block is
where it gets them:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock   # start/stop/inspect containers
  - caddy_admin:/run/caddy                       # add/remove edge routes
```

Those two sockets are the platform's crown jewels — the ability to run any
container and to reconfigure the public router. **Nothing else on the box gets
either.** That's the containment: the blast radius of a controld compromise is
"can control the box," so controld's own surface is kept tiny (tailnet-only
admin, no untrusted input) rather than sandboxed after the fact.

## Ordering trap

`stack/edge` must be up **from its current compose file** before this stack,
because edge creates and owns the external `caddy_admin` volume mounted above.
An edge stack started before the admin-socket layout existed must be
*recreated*, not reloaded:

```sh
docker compose --project-directory /opt/qincloud/stack/edge up -d --force-recreate
```

Otherwise every deploy dies at the routing step with
`dial unix /run/caddy/admin.sock: no such file or directory`. The compose file's
header comment says this too — it's the single most common boot mistake.

## What's in here

- `compose.yml` — `build: ../../controld`, the two socket mounts, `CONTROLD_DSN`
  (Postgres connection, fail-loud `${…:?}`), and the dashboard port
  `${TS_IP}:8600` (Tailscale-only; also fronted publicly at
  `dash.sparboard.com` via Caddy `basic_auth`).

## How it interacts

- Builds [`../../controld/`](../../controld/) (the Go binary + templ views).
- Mounts the `caddy_admin` socket from [`../edge/`](../edge/) to add routes.
- Connects to Postgres in [`../data/`](../data/) over `data_net` for its state.
- Exposes `/metrics` scraped by [`../observability/`](../observability/) — the
  `qincloud_app_up` gauge that the `AppDown` alert and the availability SLO both
  read.

## Running it

```sh
docker compose --project-directory /opt/qincloud/stack/controld \
  --env-file /opt/qincloud/.env up -d --build

# then drive the CLI inside the container:
docker exec qincloud-controld controld list
docker exec qincloud-controld controld deploy -app whoami -image traefik/whoami:v1.10 -port 80 -host whoami.sparboard.com
```

## SRE concept: capability confinement

A control plane is inherently privileged. The discipline is not "make it
unprivileged" (impossible — it must start containers) but "make the *set* of
privileges explicit, minimal, and auditable in one glance." This compose file is
that glance.
