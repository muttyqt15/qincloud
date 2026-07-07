# stack/observability/ — metrics, logs, alerts

The platform's senses. This stack answers three questions: *what is happening
right now* (metrics), *what happened just before* (logs), and *who do we wake up*
(alerts). It's entirely internal — bound to the Tailscale IP, never public.

## The pieces (and the unfamiliar words)

| Tool | Role | One-line what/why |
| --- | --- | --- |
| **Prometheus** | metrics DB + alert engine | Scrapes numeric time-series from every target and evaluates alert rules. The heart. |
| **Grafana** | dashboards | Draws Prometheus/Loki data as graphs (the SLO board, per-app resources). |
| **Loki** | logs DB | "Prometheus for logs" — stores and queries log lines by label, cheaply. |
| **Alloy** | log shipper | Collects container logs and ships them to Loki. |
| **Alertmanager** | notification router | Takes firing alerts from Prometheus, de-dupes/groups them, and pages Discord. |
| **exporters** | metric adapters | `node` (host), `cadvisor` (containers), `postgres`, `redis` — translate a system's state into metrics Prometheus can scrape. |

## Mental model: Prometheus *pulls*, it isn't pushed

This is the concept that makes the whole stack click. Prometheus doesn't wait
for services to send it data — **it reaches out and scrapes a `/metrics` HTTP
endpoint on each target every 15s.** Consequences worth holding:

- A target that goes dark simply stops answering scrapes → `up == 0` → the
  `InstanceDown` alert. The *absence* of data is itself a signal.
- To monitor a thing, you make it *expose* `/metrics` (or run an exporter beside
  it). controld exposes its own — the `qincloud_app_up` gauge — because deployed
  apps aren't scrape targets and something had to speak for them.
- The pull model means **Prometheus can't detect its own death**, and it lives
  on the box — so a full-box outage pages nothing. That blind spot is documented
  honestly (see below and [M9 — failure drills](../../learnings/milestones/m9-failure-drills-postmortems.md)).

## Mental model: an alert isn't done until it has *fired*

A loaded rule that has never fired is a hope, not a control — a broken monitor
looks identical to a healthy one (green, quiet, armed). So every alert here has
a matching **drill** that makes the bad thing happen and watches the page
arrive. That discipline is [observe what matters](../../learnings/concepts/observe-what-matters.md), and the catalogue of
drills is [`../../runbooks/drills/failure-catalogue.md`](../../runbooks/drills/failure-catalogue.md).

## What's in here

- `prometheus/` — scrape config + rule files. `rules/qincloud.yml` (the fast
  alerts: `InstanceDown`, `AppDown`, `PostgresDown`, `RedisDown`,
  `ContainerRestartLoop`, `BackupStale`, disk/mem) and `rules/slo.rules.yml`
  (the availability SLI recording rules + multiwindow-multiburn budget alerts).
- `alertmanager/` — routing to the Discord receiver (webhook via
  `webhook_url_file`, owned to the container's runtime uid — a real M3 gotcha).
- `grafana/provisioning/` — dashboards checked into git (the `QinCloud — apps`
  board, the SLO board), so they rebuild from source, not clicks.
- `alloy/`, `loki/` — the log pipeline config.
- `compose.yml` — runs all of the above, Tailscale-bound.

## How it interacts

- **Scrapes** controld (`qincloud_app_up`), Caddy `:2019` (over `admin_net`),
  the exporters, and itself.
- **Reads** the backup freshness metric that
  [`../../scripts/backup.sh`](../../scripts/) writes to a node-exporter textfile
  (the `BackupStale` signal).
- **Pages** through Alertmanager → Discord, proven live in the M3 and AppDown
  pager drills.

## SRE concepts here

- **SLOs & error budgets** — page on *burn rate* (breaking fast enough to miss
  the promise), not every blip. See [M7 — SLOs and burn alerts](../../learnings/milestones/m7-slos-and-burn-alerts.md).
- **Alert at the cause** — `PostgresDown` on `pg_up == 0` pages "the database is
  down," not "a scrape target three layers away is unreachable."
- **The monitoring blind spot** — the pager lives on the box it watches; the
  compensating control is an *external* uptime check, carried as a tracked
  action item, not pretended away.

## Editing rules

```sh
# validate before shipping — a broken rule file silently loads nothing
promtool check rules stack/observability/prometheus/rules/*.yml
```

Read [`../../runbooks/gotchas/alerting.md`](../../runbooks/gotchas/alerting.md)
first: an alert on a metric that doesn't exist is a silent no-op, and drill
timing (`for:` + `group_wait`) will make a "passing" test prove nothing if you
recover too early.
