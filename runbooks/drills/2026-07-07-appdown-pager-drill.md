# Drill: M7/M9 — AppDown alert fires on a real app outage (2026-07-07)

**Goal:** prove the new `AppDown` alert (added with the SLO work) actually
pages — not just that it is armed. Same discipline as the M3 pager drill: an
alert is not done until it has been *seen* to fire and route.

## Why this alert exists

The existing `InstanceDown` (`up == 0`) only covers **scrape targets** —
exporters, controld, Caddy. Deployed app containers (whoami, umami, notes)
are **not** scraped, so before this alert a dead app paged nothing. `AppDown`
watches controld's own `qincloud_app_up{app=...}` gauge, the only signal that
a deployed app has stopped running. The SLO burn alerts (`slo.rules.yml`) are
deliberately slow (multiwindow error-budget); `AppDown` is the fast one.

## What ran (times UTC)

| T | Event |
| --- | --- |
| 17:29:55 | **T0** — `docker stop qc-whoami-35` (the live whoami container) |
| +30s | `qincloud_app_up{whoami}` → 0; `AppDown` **pending** (the `for: 2m` hold) |
| +150s | `AppDown` **firing** |
| +~150s | Alertmanager shows `AppDown{app=whoami}` active → routed to the `discord` receiver (delivery path itself proven in the M3 drill) |
| +~155s | `docker start qc-whoami-35` |
| +~175s | `qincloud_app_up{whoami}` → 1; https://whoami.sparboard.com back to 200; alert resolves |

Detection-to-page ≈ **2m30s** = the `for: 2m` anti-flap hold + a scrape/eval
cycle. Deliberate: an app blip should not page.

## Confirmed

- The SLI recording rules compute (`qincloud:sli_availability:ratio_rate5m` =
  1.0 per app when healthy), and the burn + down alerts all load and sit
  `inactive` until breached.
- `AppDown`, `PostgresDown`, `RedisDown` close the gap the failure-catalogue
  flagged: fast pages for an app / datastore going down, which `InstanceDown`
  never covered.

## Note

whoami is the throwaway demo app, chosen deliberately for a low-stakes live
drill. Running the same drill against a real app should use a maintenance
window — see `failure-catalogue.md` (drill A).
