---
title: "M1 — The Edge: Caddy, TLS, and the Admin API"
slug: m1-edge-and-tls
type: milestone
milestone: M1
status: stable
difficulty: 3
tags: [qincloud, networking, security, infra, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[single-source-of-truth]]", "[[fail-loud-at-boundaries]]", "[[layered-trust-defense-in-depth]]", "[[idempotent-self-verifying-operations]]", "[[the-box-is-disposable]]", "[[m4-controld-deploy-engine]]"]
sources: ["stack/edge/Caddyfile", "stack/edge/compose.yml", "runbooks/gotchas/caddy.md", "controld/internal/caddyapi/caddyapi.go", "scripts/deploy-edge.sh"]
---

# M1 — The Edge: Caddy, TLS, and the Admin API

> **In one sentence:** one front door for the whole platform — a single Caddy container that terminates HTTPS, gets certificates automatically, and can be re-routed on the fly through a private control channel — with one sharp trap: after first boot, the live config, not the config *file*, is the truth.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Imagine an office building with dozens of tenants but only **one** front entrance. Every visitor comes through that door. The doorman checks who they're here to see, walks them to the right office, and — because it's a nice building — hands every visitor a verified ID badge on the way in so nobody's identity is in doubt. If you want to add a new tenant, you don't cut a new hole in the wall; you tell the doorman "there's a new office on floor 3, here's its name," and he starts directing people there.

Caddy is that doorman. QinCloud runs everything (databases, dashboards, tenant apps) on one machine, hidden on private internal networks. **Only Caddy is allowed to face the public internet** — it's the sole container that publishes ports 80 and 443. Everything else sits behind it. Caddy does three jobs at the door:

1. **HTTPS termination + auto-TLS** — it fetches and renews the little padlock certificates (from Let's Encrypt) with zero manual work, per hostname.
2. **Routing** — it looks at the requested hostname (`umami.sparboard.com`, `dash.sparboard.com`) and forwards to the right internal container.
3. **A control channel** — a private "admin API" the platform uses to add and remove routes *while Caddy keeps running*, without an outage.

A platform needs a single edge like this because it's the one place to centralize TLS, the one choke point to enforce who-gets-in, and the one knob the deploy engine turns to make a freshly-started app reachable.

## 2. The plan (initial approach)

The intended design was clean: write a `Caddyfile` (Caddy's human-readable config) with the platform's base site, and let the deploy engine — **controld** (see [[m4-controld-deploy-engine]]) — add each new app's route at runtime by calling Caddy's admin API. Caddy would auto-provision a TLS cert the moment a new hostname's route landed. The `Caddyfile` would be the config; the admin API would be the live edits on top. Simple two-layer story.

## 3. Where it deviated

The trap: **which of those two things is the real config after the platform has been running for a while?**

Naively you'd say "the `Caddyfile`, obviously — it's the file on disk." Wrong. Caddy is started with a flag, `--resume`, that changes everything:

```
command: caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume
```

Every time controld adds a route through the admin API, Caddy **autosaves** the entire live configuration to `/config/caddy/autosave.json`. With `--resume`, on any restart Caddy boots from *that autosave file*, not from the `Caddyfile`. The `Caddyfile` becomes a **first-boot seed only** — it's read once, on a truly fresh box, and never again.

The failure this creates is silent and nasty. Someone edits the `Caddyfile` to add a header or tweak a log, then runs the "obvious" command to apply it:

```
caddy reload --config /etc/caddy/Caddyfile
```

Caddy replaces its *entire* live config with the file — wiping every route controld had programmed via the API. The apps' containers keep running, controld's database still says they're "live," but every request now falls through to the seed block's catch-all `respond "qincloud edge ok" 200`. No error. No log line screaming. Just every tenant app quietly 200-ing a placeholder string. This is the exact shape of a bug that costs hours: two sources of truth that *look* interchangeable but aren't (see [[single-source-of-truth]]), failing without a peep (see [[fail-loud-at-boundaries]]).

## 4. The fix — and how I found it

The root cause isn't "someone ran the wrong command" — it's that **the system allowed two writable representations of the same truth to diverge**, and picked the file-shaped one for humans and the JSON-shaped one for boots. You can't delete `--resume` (without it, *any* restart — host reboot, OOM-kill, image bump, `compose` recreate — silently unroutes every deployed app). So the fix is to make the dangerous operation impossible to do casually, and self-healing when it must be done.

That fix is `scripts/deploy-edge.sh` — the **one** self-verifying command for any edge config change. Instead of hand-running the reload, you run this. It:

1. validates the on-disk `Caddyfile` (`caddy adapt`),
2. heals a stale bind-mount (see §5),
3. reloads,
4. **re-deploys every app** so controld re-adds each route the reload just dropped,
5. and **verifies each app hostname actually responds** before exiting — non-zero on any failure.

So a legitimate `Caddyfile` edit becomes: edit → run `deploy-edge.sh` → it accepts the route wipe *and immediately restores every route*, then proves each one answers. The class of bug is gone, not just this instance — because the reload can no longer leave the edge in a silently-broken state. This is [[idempotent-self-verifying-operations]] made concrete: re-runnable, and it proves its own success instead of trusting it.

## 5. Going deep (systems level)

**The admin API is a unix socket, on purpose.** In the `Caddyfile`:

```
admin unix//run/caddy/admin.sock
```

Not a TCP port. If the admin API listened on the network, *any* container on the shared `app_net` bridge could `POST /load` and reprogram the entire edge — a full compromise. Instead it's a socket file in the named volume `caddy_admin`, mounted into **both** the edge and controld containers (`stack/edge/compose.yml:67`). controld drives the edge over a shared file, never over a network. Consequence: on a fresh box, `stack/edge` must come up **before** `stack/controld`, because edge creates the volume.

**Routes must land at index 0.** The adapted config ends with the seed site's catch-all routes. A route *appended* to the array sits behind them and never matches. So controld's upsert (`controld/internal/caddyapi/caddyapi.go`) is: `PATCH /id/qc-<app>` for an in-place atomic replace, and only on a 404 does it `PUT .../routes/0` to insert at the front.

**Auto-HTTPS injects invisible redirects.** When a host-matched route lands on the `:443` server, Caddy both provisions the cert *and* injects an HTTP→HTTPS redirect on `:80` that does **not** appear in `GET /config/`. So `curl http://host/` returning `308` is correct, not a bug — health-check over `127.0.0.1` or HTTPS. The `Caddyfile`'s `:80` block explicitly claims port 80 (keeps a `/healthz` for the container healthcheck, 308s everything else) so Caddy doesn't spin up its own competing redirect server.

**The rsync bind-mount trap.** `rsync` without `--inplace` writes a temp file then renames it — giving the host path a *new inode* while the container still holds the old one. The container serves the pre-edit `Caddyfile`; every `caddy adapt`/`reload` reads stale, with `validate` still passing and no error. Defenses: sync with `rsync --inplace`, and `deploy-edge.sh` checksums the container's mounted file against the on-disk one, recreating the container on mismatch. Check by hand: `docker exec edge-caddy-1 sha256sum /etc/caddy/Caddyfile` vs the host file.

**The dashboard door is layered.** `dash.sparboard.com` is public but sits behind Cloudflare + edge basic-auth ([[layered-trust-defense-in-depth]]): a positive `@cf remote_ip` matcher drops anyone not arriving from a Cloudflare edge IP straight to `404` *before* any bcrypt runs (so hitting the origin IP directly can't exhaust CPU via the hash), then `basic_auth` gates the rest. The bcrypt hash is generated at **cost 10** deliberately — cost 14 (~1.4s) on an unauthenticated public door is a CPU-exhaustion lever. Two silent adapter traps live in that block: the IP matcher must be *positive* (`@cf remote_ip`), never negated (`not remote_ip`) — a negated matcher collides with the `:2019` metrics block's `remote_ip` matcher in Caddy 2.10's adapter and one is silently dropped; and comments in that block must stay plain ASCII (a backtick or `$` in a comment makes the lexer swallow the following directive).

## 6. How this compares to best practice

A managed platform (Vercel, Fly, a cloud LB) hides all of this: TLS, routing, and route-mutation are one control plane you never see split. QinCloud reproduces the *shape* — a single edge, an API-driven route table, per-hostname auto-TLS — on one box for a fraction of the moving parts. Where we match the standard: TLS is fully automated and cert state is persisted (`caddy_data` volume) so restarts don't burn Let's Encrypt rate limits; the admin plane is off the network entirely.

Where we knowingly cut a corner: `--resume` makes the live JSON the source of truth while a human-editable `Caddyfile` still exists — a genuine two-representations risk that a mature platform avoids by having *no* second editable representation. We accept it because `deploy-edge.sh` reconciles them on every change and verifies the result. It would need revisiting the day the `Caddyfile` grows enough hand-maintained logic that "edit file → wipe routes → redeploy all" becomes too heavy — at which point routes should move fully into controld's DB and the `Caddyfile` shrink to a static shell. Note also `--resume` does **not** cover a rebuilt box: a fresh `caddy_config` volume has no autosave, so routes must be reconstructed from controld's DB — which is fine, because [[the-box-is-disposable]] and controld can reconcile.

## 7. The underlying why (the transferable lesson)

When one fact is stored in two places, **something must decide which one wins — and if that decision is implicit, the two will drift and fail silently.** Here the "fact" was the edge's route table, living in both a `Caddyfile` and an autosave JSON. Nothing loudly declared the autosave the winner, so the intuitive action (reload the file) quietly destroyed the real state. The durable fix wasn't a warning comment; it was collapsing the operation into a single self-verifying command that reconciles the two representations and *proves the edge still works* before it returns. Two lessons stack: name one source of truth and make the others derive from it or be reconstructable from it ([[single-source-of-truth]]); and never let a state-changing operation succeed silently — make it verify the end state, so a broken edge fails loud instead of serving a cheerful placeholder ([[fail-loud-at-boundaries]]).

---
**Teaches:** [[single-source-of-truth]] · [[fail-loud-at-boundaries]] · [[layered-trust-defense-in-depth]] · [[idempotent-self-verifying-operations]] · [[the-box-is-disposable]]
**Sources:** `stack/edge/Caddyfile`, `stack/edge/compose.yml`, `runbooks/gotchas/caddy.md`, `controld/internal/caddyapi/caddyapi.go`, `scripts/deploy-edge.sh`
