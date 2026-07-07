# controld/ — the control plane

This is the **one piece of QinCloud we wrote instead of bought.** Everything
else in the repo is off-the-shelf software (Caddy, Postgres, Prometheus, …)
wired together by config. `controld` is a single Go binary that owns the app
lifecycle: it deploys apps, routes them, provisions their databases, and
reports their health. Think of it as a tiny, opinionated Heroku for one box.

## What it does (and the one rule)

You give controld an image name and a hostname; it pulls the image, starts a
container, tells Caddy to route the hostname to it, and records the whole
thing in Postgres. It also mints per-app database credentials, exposes its
own metrics, and serves a dashboard.

The single unbreakable rule, the reason this is a state machine and not a
script: **a failed deploy never takes down what was already running.** The old
container keeps serving until the new one is confirmed up *and* routed. If you
internalise one thing before reading the code, internalise that — every
design choice bends toward it.

## Mental model: read it as one linear path

The whole binary is a CLI (`cmd/controld/main.go`) whose subcommands each walk
a short, near-linear chain. The most important chain is a deploy:

```
main (parse flags)
  → deploy.Run
      → store          (write the next state row: pending → pulling → …)
      → dockerx        (pull image, inspect EXPOSE, start container)
      → caddyapi       (add the route over the admin socket)
      → store          (mark live)
```

Read it top-to-bottom and it reads like a sentence. Each `internal/` package
is one noun in that sentence — none of them call *across* to each other, they
only get called *down into* by `deploy`/`dashboard`/`provision`. That flatness
is deliberate — wiring lives at the top, work lives in the leaves, and the
callstack stays linear enough to hold in your head.

## The packages (`internal/`)

Each is a black box with a small surface — you can understand one without the
others:

| Package | What it owns | Talks to |
| --- | --- | --- |
| `store` | The **single source of truth**: the `apps` + `deploys` tables in Postgres. Every state transition is a row written *before* the step runs. | Postgres (pgx) |
| `dockerx` | The Docker daemon, wrapped. Pull, inspect, run, stop, exec, stats — and `ExecCapture` for running commands *inside* a container (how provisioning reaches psql/redis-cli). | `docker.sock` |
| `caddyapi` | The edge router, wrapped. Add/remove a host→upstream route via Caddy's admin API over a unix socket. | `caddy_admin` socket |
| `deploy` | The **state machine** and the deploy *contract* (`AppSpec`, name validation). Orchestrates store + dockerx + caddyapi into one safe rollout. | store, dockerx, caddyapi |
| `provision` | Per-app **tenancy**: a Postgres role+DB and a Redis ACL user fenced to the app's key prefix. Secrets cross as exec-env and password *hashes*, never argv. | dockerx (→ psql/redis-cli) |
| `dashboard` | The **M5 web UI**: templ-rendered HTML + htmx, plus `observe.go` (the `/metrics` exporter — `qincloud_app_up` and per-app resource gauges). | store, dockerx |
| `applock.go` (root) | Per-app **advisory locks** so two concurrent deploys of the same app can't interleave and corrupt each other. | (in-process) |

## Unfamiliar tech, briefly

- **Docker Go SDK** — controld talks to the Docker daemon over its unix socket
  the way `docker` the CLI does, but from Go. That socket is one of only two
  capabilities the container holds (the other is the Caddy admin socket); it
  is why controld can start your app's container.
- **Caddy admin API** — Caddy can be reconfigured at runtime by POSTing JSON
  to an admin endpoint. controld uses it to add a route the instant a new
  container is ready. See [`../stack/edge/`](../stack/edge/) for the trap: after
  first boot, the *live* config (not the Caddyfile) is truth.
- **templ + htmx** — server-rendered HTML with no JavaScript framework. `templ`
  compiles `.templ` files to Go functions (`views_templ.go`, committed);
  `htmx` lets an HTML attribute re-fetch a *fragment* of the page (e.g. poll an
  in-flight deploy) without a SPA. The server draws the page; htmx swaps slices
  of it.
- **pgx** — the Postgres driver. controld writes hand-authored SQL (no ORM),
  because the schema is tiny and the queries are few.

## SRE concepts living here

- **State machine over imperative script** — persisting each transition before
  the work means a crash mid-deploy is *recoverable and legible*, not a mystery
  half-state. The `deploys` table is the audit log.
- **Single source of truth** — the DB is the only place that knows what should
  exist. After a full-box rebuild, `controld list` reads the restored table and
  drives its own recovery (see [M8 — disaster recovery](../learnings/milestones/m8-disaster-recovery.md)).
- **Least privilege** — the container is handed exactly two sockets and nothing
  else; provisioning passes secrets as hashes, never on the command line.

## Where to go next

- The sharp edges before you touch the state machine:
  [`../runbooks/gotchas/deploys.md`](../runbooks/gotchas/deploys.md).
- The full teaching write-up: [`../learnings/milestones/m4-controld-deploy-engine.md`](../learnings/milestones/m4-controld-deploy-engine.md)
  (engine) and [`m5-dashboard.md`](../learnings/milestones/m5-dashboard.md) (UI).
- How it's built and run on the box: [`../stack/controld/`](../stack/controld/).

## Building

```sh
go tool templ generate      # regenerate views_templ.go after editing .templ
go build ./...
go test ./...               # every package has black-box tests with in-memory fakes
```
