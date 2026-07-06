---
title: M5 — The Dashboard (templ + htmx)
slug: m5-dashboard
type: milestone
milestone: M5
status: stable
difficulty: 3
tags: [qincloud, devops, golang, observability, reliability]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m4-controld-deploy-engine]]", "[[m3-observability-and-alerting]]", "[[x1-public-dashboard-cloudflare]]", "[[x2-per-app-observability]]"]
sources: ["controld/internal/dashboard/dashboard.go", "controld/internal/dashboard/observe.go", "controld/internal/dashboard/views.templ", "runbooks/gotchas/dashboard.md"]
---

# M5 — The Dashboard (templ + htmx)

> **In one sentence:** a server-rendered web dashboard for deploying and watching apps on the box, with no JavaScript framework — the server draws the page, htmx re-fetches slices of it, and every scrap of state lives in one Postgres table so a restart forgets nothing.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Up to M5 the only way to run an app on the box was the command line: type `controld deploy`, read the output, type `controld ps` to check on it. That works for the person who built it and nobody else. M5 is the **window into the box** — a web page where you can see every app, its live CPU and memory, its logs, its deploy history, and buttons to deploy, redeploy, or destroy.

Think of a restaurant kitchen with a **pass** — the counter where finished plates appear and the expeditor reads, at a glance, what's cooking, what's done, what burned. The dashboard is the pass for the box. The important design choice: the pass **holds no state of its own**. It doesn't remember what's cooking; it just reads the kitchen's own order tickets and shows them. Tear the pass down and rebuild it and nothing is lost, because the tickets were never on the pass — they were in the kitchen. In our case the "tickets" are rows in the `deploys` table, written by the same deploy engine ([[m4-controld-deploy-engine]]) the CLI uses.

That single decision — the dashboard is a *view*, never a *source* — is what makes it trustworthy. Two people can watch it, the CLI can run a deploy behind its back, the whole process can crash and restart, and the page still tells the truth, because the truth was never inside the page.

## 2. The plan (initial approach)

No React, no build step, no client state store. The stack is deliberately boring:

- **templ** — a Go templating library that compiles HTML templates into type-checked Go functions. The template `AppList(apps, latest, now)` is a real function; if the data shape changes, the build breaks, not the page.
- **htmx** — a tiny (~14KB) JS library that lets an HTML element fetch a URL and swap the returned HTML into itself. No JSON, no client-side rendering — the server sends HTML fragments and htmx pastes them in. `hx-trigger="every 3s"` turns a `<div>` into a self-refreshing panel; `hx-post="/deploy"` turns a form into an async action, all declared in attributes.

The plan: server renders full pages on navigation, htmx polls small fragments for liveness (`#apps` list, `#stats`, `#logs`, `#history`), and all displayed status is *derived* from the `deploys` table by pure Go helpers — `statusFor`, `deployStatusView`, `took` in `dashboard.go` — so the markup carries zero logic. One projection, one source ([[single-source-of-truth]]).

Security came for free from the architecture rather than a login system. There are **two doors** ([[layered-trust-defense-in-depth]]): the tailnet path (`TS_IP:8600`, where reachability *is* the auth, same as Grafana) and the public path (`dash.sparboard.com`, behind Caddy `basic_auth`, fronted by Cloudflare — see [[x1-public-dashboard-cloudflare]]). Since there's no session, there's no CSRF token to manage; instead every state-changing route requires the `HX-Request: true` header, which browsers refuse to attach cross-origin without a CORS preflight the server never grants (`requireHtmx`, `dashboard.go:113`).

## 3. Where it deviated

The happy path worked on the first try. Then an **adversarial review** — deliberately trying to make the UI lie ([[adversarial-review]]) — found three ways it did, each a case of "the page looks fine, the state underneath is wrong":

1. **A stopped container showed a confident `cpu 0.0% · mem 0 MiB`** instead of admitting it was down. The stats fragment happily rendered zeros for a container that wasn't running. A dashboard that reports "0%" for a dead app is worse than one that reports nothing — it invents calm.
2. **The 3-second poll ate the double-click guard.** htmx marks an in-flight button with a `.htmx-request` class; that's the double-click protection. But the `#apps` list refreshes every 3s with an `innerHTML` swap — and that swap replaced the buttons mid-request, wiping the class and re-arming a second destroy click on an app already being destroyed.
3. **A destroyed app's detail page polled forever.** Destroy an app in one tab while its detail page is open in another: the history fragment kept polling `/apps/{name}/history`, kept getting a `200 OK` with an empty table, and kept believing that was valid state — an empty page pretending to be a live one, refreshing every 3s into eternity.

## 4. The fix — and how I found it

Each fix pushed the correction back to where the wrong state was *born*, not where it surfaced ([[fail-loud-at-boundaries]]):

1. **Stopped container.** `fragmentApp` and `appStats` (`observe.go`) now treat a failed `Stats` call as a fact to *state*, not a zero to *render*: `render(w, r, MutedLine("stats unavailable — is the container running?"))`. The `/metrics` exporter does the same structurally — it emits `qincloud_app_up{app="x"} 0` for a container it couldn't stat and simply **omits** the cpu/mem series, so Prometheus records "down" rather than "0%". Absence, not a fake number.
2. **Poll vs. in-flight action.** The `#apps` poll trigger became conditional: `hx-trigger="every 3s [document.querySelectorAll('.htmx-request').length === 0]"` (`views.templ:53`). The list only refreshes when *no* htmx request is in flight anywhere on the page — the swap can never land on a button mid-action.
3. **Dead poll.** `appHistory` now checks whether the app still exists and, if gone, returns HTTP **286** — the status htmx treats as "swap this in, then *stop polling*" (`dashboard.go:176`, `htmxStopPolling = 286`). The fragment shows a flash — `"app x no longer exists — destroyed"` — and the poll dies. Same treatment in `fragmentApp` for the stats/logs fragments.

The through-line of the fixes: **an empty or zeroed success is a lie an operator will trust.** The right response to "the thing you're watching is gone or broken" is to say so and stop, not to keep rendering a plausible-looking nothing.

## 5. Going deep (systems level)

**Route map** (`Register`, `dashboard.go:94`, Go 1.22 method-path patterns). `GET /{$}` pins the index to the exact root so unknown paths 404. GET routes render pages/fragments; the three POSTs (`/deploy`, `/apps/{name}/redeploy`, `/apps/{name}/destroy`) are each wrapped in `requireHtmx`.

**The fast-fail handoff** (`runAsync`, `dashboard.go:242`). A deploy can take minutes; an HTTP handler can't block that long. So `runAsync` launches the deploy on a **detached** context (`context.WithTimeout(context.Background(), deployBudget)` — 5 min) and races it against a 1-second `fastFailWindow`. Pre-flight failures (validation, lock held, host conflict) are pure DB work and lose the race, so they surface inline as a flash. Anything still running at 1s has *already written its `deploys` row*, so it hands off: "deploy started — status appears in the list", and the polling `#apps` list takes over reporting. Destroy uses `context.WithoutCancel(r.Context())` for the same reason (`dashboard.go:226`) — a closed browser tab must never abort a half-done destroy, leaving the route deleted but containers alive.

**Status projection** (`deployStatusView`, `dashboard.go:358`). One switch turns a `DeployRecord` into a `{Label, Class}` badge: `live` → live, `failed` → failed, non-terminal-but-older-than-`staleAfter` (10 min) → `abandoned`, else → `starting…`. The `abandoned` branch is how a controld crash mid-deploy stops polling `starting…` forever — a non-terminal row that outlived the 5-min budget is declared dead. `staleAfter` must stay comfortably above `deployBudget`.

**Per-app metrics** (`metrics`, `observe.go:87`). `GET /metrics` is a Prometheus exposition giving every app a first-class `app="<name>"` label — cAdvisor can't, because under Docker's containerd image store its series carry only the raw container ID ([[x2-per-app-observability]]). Each `Stats` call blocks ~1–1.5s (the daemon double-samples to compute a CPU rate), so sampling is bounded-concurrent (`metricsConcurrency = 8`) or a 6-app box would blow the 10s scrape timeout. Note the exposition emits **one HELP/TYPE block per metric**, iterating all apps inside each — a repeated TYPE line for one metric is a Prometheus parse error.

**Secrets discipline.** `envKeys` (`dashboard.go:310`) renders env var *names* only — values are secrets and the detail page must never echo them. App-controlled log text is always templ-escaped (regression test `TestLogsAreEscaped`) and capped at 256KB so a flooding app can't balloon a response.

**Edge gotchas** (`runbooks/gotchas/dashboard.md`) that cost hours on the public door: bcrypt at **cost 10 not 14** (an unauthenticated client forces a hash per request — cost 14 is a ~1.4s-CPU DoS lever on the shared edge); the hash resolved at Caddy **adapt-time** not runtime (a runtime `{env.}` placeholder lands raw where base64 is expected and Caddy crash-loops); and `rsync --inplace` for the bind-mounted Caddyfile (plain rsync renames a temp file, giving the host a new inode while the container serves the stale old one).

## 6. How this compares to best practice

A managed PaaS (Heroku, Render, Fly) ships a heavyweight SPA talking to a JSON API, with sessions, RBAC, and audit logging. We deliberately cut all of it. **Where we match:** the state-is-in-the-database, UI-is-a-projection discipline is exactly how mature dashboards avoid drift — nobody serious keeps a "current deploys" map in browser memory. **Where we cut a corner:** no per-user auth (reachability + basic_auth is the whole model), no client framework (htmx polling instead of websockets/SSE — fine for a single-operator box, would need rethinking at multi-tenant scale or sub-second liveness). The tradeoff we accepted: ~200 lines of Go and zero build step buys a dashboard that can't drift, can't leak client state, and survives its own restart — at the cost of features a single operator doesn't need. Revisit if the box ever grows real multi-user access.

## 7. The underlying why (the transferable lesson)

**A view must never become a source.** The moment a UI caches its own copy of authoritative state, it can disagree with reality — and a confident, wrong UI is more dangerous than no UI. Keep one projection reading one source ([[single-source-of-truth]]), make restart-survival a property of the architecture not a feature ([[the-box-is-disposable]]), and when the thing you're displaying is gone or unmeasurable, **say so and stop** — never render a plausible zero ([[fail-loud-at-boundaries]]). The bugs that survive the happy path are the ones only [[adversarial-review]] finds: not "does it work?" but "how does it lie?"

---
**Teaches:** [[single-source-of-truth]] · [[the-box-is-disposable]] · [[fail-loud-at-boundaries]] · [[adversarial-review]]
**Sources:** `controld/internal/dashboard/dashboard.go`, `controld/internal/dashboard/observe.go`, `controld/internal/dashboard/views.templ`, `runbooks/gotchas/dashboard.md`
