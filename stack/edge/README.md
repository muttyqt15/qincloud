# stack/edge/ — Caddy, the front door

Every public request to QinCloud enters here. This stack runs **one Caddy
container**, and it is the *only* thing on the box that listens on ports 80 and
443. Everything else — apps, databases, dashboards — sits behind it on private
networks.

## Why Caddy (briefly)

Caddy is a web server / reverse proxy whose headline feature is **automatic
HTTPS**: point a domain at the box, and Caddy obtains and renews a Let's Encrypt
TLS certificate on its own, no cron job, no certbot, no manual renewal. For a
one-person platform that is enormous — TLS is the kind of thing that silently
expires at 3am otherwise. It also reverse-proxies (forwards a hostname to an
internal container) and reconfigures **at runtime via an admin API**, which is
exactly what controld needs to add a route the moment a new app is deployed.

We chose Caddy over nginx/Traefik because auto-TLS is built in (not a plugin),
the config is small and readable, and the runtime admin API is first-class.

## Mental model: the file is the *seed*, the admin API is the *truth*

This is the one trap in the whole stack, and it will bite you if you don't hold
it. The `Caddyfile` is the **initial** config Caddy loads at first boot. After
that, controld mutates the *running* config through the admin socket
(`/run/caddy/admin.sock`, shared via the `caddy_admin` volume). So:

- On first boot: the `Caddyfile` is the config.
- After any deploy: the **live config** (in Caddy's memory / autosave) is the
  config, and the `Caddyfile` is stale.

Reload the Caddyfile carelessly and you can wipe controld's routes. The rules —
`--resume` vs autosave, route index 0, `basic_auth` `{$…}` vs `{env.}`
interpolation — live in [`../../runbooks/gotchas/caddy.md`](../../runbooks/gotchas/caddy.md).
**Read that before editing the Caddyfile.**

## What's in here

- `Caddyfile` — the seed config: the static public hosts (e.g. the dashboard at
  `dash.sparboard.com` behind `basic_auth`, Cloudflare-fronted, origin locked to
  CF IPs) and the TLS/logging defaults. Per-app routes are *not* here — controld
  adds those at deploy time.
- `compose.yml` — runs Caddy, publishes 80/443, mounts the `caddy_admin` volume,
  and exposes `:2019` admin metrics gated to `admin_net`.

## How it interacts

- **controld** ([`../controld/`](../controld/)) adds/removes routes over the
  admin socket. Edge must be up *first* so that socket exists — otherwise every
  deploy fails at the routing step.
- **observability** scrapes Caddy's `:2019` metrics over `admin_net`. Note the
  SLO caveat: Caddy exposes *no per-host request metrics* here, which is why the
  availability SLI is an up-ratio, not a request-success ratio (see
  [M7 — SLOs and burn alerts](../../learnings/milestones/m7-slos-and-burn-alerts.md)).
- **apps** are reached over `app_net`.

## SRE concept: the single ingress point

One front door means one place to terminate TLS, one place for access logs, one
choke point to reason about the attack surface. It also means Caddy is a single
point of failure for *all* public traffic — accepted deliberately on a one-box
platform, and the reason edge is the first stack rebuilt in DR.

## Deploying an edge change

Never hand-edit the running container. Use the self-verifying script:

```sh
scripts/deploy-edge.sh    # validate → heal the mount → reload → restore routes → confirm each site 200s
```

The story behind that one-command discipline: [x3 — verified edge deploys](../../learnings/milestones/x3-verified-edge-deploys.md).
