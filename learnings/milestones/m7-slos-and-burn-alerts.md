---
title: "M7 — SLOs and Error-Budget Burn Alerts"
slug: m7-slos-and-burn-alerts
type: milestone
milestone: M7
status: stable
difficulty: 4
tags: [qincloud, observability, slo, alerting, reliability]
created: 2026-07-07
updated: 2026-07-07
related: ["[[observe-what-matters]]", "[[single-source-of-truth]]", "[[fail-loud-at-boundaries]]", "[[verify-the-artifact-under-test]]", "[[root-cause-over-patch]]", "[[m3-observability-and-alerting]]", "[[x2-per-app-observability]]", "[[m9-failure-drills-postmortems]]"]
sources: ["stack/observability/prometheus/rules/slo.rules.yml", "stack/observability/prometheus/rules/qincloud.yml", "runbooks/drills/2026-07-07-appdown-pager-drill.md"]
---

# M7 — SLOs and Error-Budget Burn Alerts

> **In one sentence:** we set a public, numeric promise for each app — "up 99.5% of every rolling 30 days" — and wired the pager to fire not when something breaks, but when it is breaking *fast enough to break the promise*, so the alert tracks the commitment instead of every twitch.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Up to this milestone, alerting was binary and twitchy: a thing was either up or down, and any blip could wake you. That is the alarm equivalent of a smoke detector that shrieks at toast. It trains you to ignore it — and an ignored pager is worse than no pager, because it *looks* like coverage.

An **SLO** (Service Level Objective) replaces "is it up right now?" with a grown-up promise: *"this app will be available 99.5% of the time, measured over the last 30 days."* That 0.5% you're allowed to be down is your **error budget** — a bank account of permitted failure. A five-minute outage spends a little; a two-hour outage spends a lot. The insight that makes this powerful: you should not page a human because the budget was *touched*. You should page when it is being *drained so fast* that, if the bleeding continues, you'll be bankrupt before anyone would otherwise notice. That is a **burn-rate alert**, and it is the single most important idea in modern on-call.

The point of M7 was to stop alerting on symptoms and start alerting on the *promise* — and to do it honestly, with the metrics this box actually has, not the ones a textbook assumes.

## 2. The plan (initial approach)

The textbook recipe (Google's SRE workbook) is well-trodden, and the plan was to follow it:

1. Pick the **SLI** (Service Level *Indicator*) — the raw measurement of "good." The canonical availability SLI is **non-5xx requests ÷ total requests**: of every request users sent, what fraction did we answer without a server error?
2. Set the objective at 99.5% over 30 days, making the error budget 0.5%.
3. Build **multiwindow, multiburn** alerts: pair a fast-burn alert (budget gone in ~2 days → page now) with slower ones (gone in ~5 days → page; trending over → ticket), each long window confirmed by a short "reset guard" window so the alert clears quickly once the burn stops.
4. Put a Grafana board on top so the budget is visible, not just alertable.

Everything hinged on step 1 — having a request-success metric to divide.

## 3. Where it deviated

Step 1 had no ground to stand on. **Caddy, on this box, exposes no per-host request metrics at all.** I went looking on the live Prometheus for `caddy_http_requests_total` and `caddy_http_request_duration_seconds_bucket` — the series every "SLO with Caddy" tutorial assumes — and they simply do not exist here. The only `caddy_*` series present are `caddy_admin_http_requests_total` (traffic to the *admin API*, not users), `caddy_config_*`, and `caddy_reverse_proxy_upstreams_healthy`. There is no count of user requests, and no count of 5xx responses. The canonical availability SLI — non-5xx over total — is **not computable**.

A second, smaller surprise waited behind the first: even the fallback signal didn't cover everything. The natural fallback is an **up-ratio** SLI — "what fraction of scrapes was this app up?" — built on controld's own `qincloud_app_up{app=...}` gauge ([[x2-per-app-observability]]). But `qincloud_app_up` has series for `notes`, `umami`, and `whoami` only. **The dashboard itself has no such gauge** — it isn't a deployed tenant, it's the control plane. So one of the four things I most wanted an SLO on had no availability signal in the SLI I'd just chosen.

## 4. The fix — and how I found it

I found it the only honest way: by querying the running Prometheus before writing a single rule, instead of trusting what the tutorial promised would be there. The metric that doesn't exist can't be discovered by reading YAML — only by asking the live system "what series do you actually have?" ([[verify-the-artifact-under-test]]).

**The SLI fix** was to switch from a request-ratio to an **up-ratio**, and to name the swap loudly in the file so the next reader knows it was a deliberate, grounded downgrade, not laziness. `avg_over_time(qincloud_app_up[window])` *is* the availability ratio directly — the fraction of that window the app was up — so `1 - ratio` is the error ratio the burn alerts compare against the 0.5% budget. It measures a slightly different thing than a request SLI (process-up, not request-success), and the file says so in a "Metrics grounding" header block that records exactly which series were confirmed present and why the textbook one was rejected ([[single-source-of-truth]]: the rule file carries its own justification).

**The dashboard-has-no-gauge fix** was a small, exact piece of PromQL. The dashboard is served by the same controld process that exposes `/metrics` on the same port — so if the dashboard is down, `up{job="controld"}` is 0. I proxy the dashboard's availability through its host process's scrape health and `label_replace` it onto `app="dashboard"`, unioned with the three real gauges so all four apps share one uniform keyed SLI series:

```promql
avg by (app) (avg_over_time(qincloud_app_up[5m]))
or
label_replace(avg(avg_over_time(up{job="controld"}[5m])), "app", "dashboard", "", "")
```

The `or` is load-bearing: it's a set union, so the dashboard row only materialises from the second operand because the first never produces an `app="dashboard"` series.

## 5. Going deep (systems level)

**The recorded SLI layer** (`slo.rules.yml`, group `qincloud_slo_sli`). One availability-ratio series per app for every window a burn alert reads — 5m, 30m, 1h, 2h, 6h, 1d, 3d — each the union of the three controld gauges plus the dashboard proxy, projected onto a single `app` label. Recording rules (not inline alert expressions) so the same ratio is computed once and reused by every alert and the Grafana board, and so `promtool check rules` can validate them.

**The burn-alert layer** (group `qincloud_slo_burn`), budget = 0.005. Burn rate *n* means the error ratio exceeds *n* × 0.005. Four multiwindow-multiburn pairs, each a long detection window ANDed with a short reset-guard window:

| Alert | Long window | Guard | Burn | Budget gone in | Severity |
| --- | --- | --- | --- | --- | --- |
| `SLOErrorBudgetBurnFast` | 1h | 5m | 14.4× | ~2 days | critical (page) |
| `SLOErrorBudgetBurnSlow` | 6h | 30m | 6× | ~5 days | critical (page) |
| `SLOErrorBudgetBurnTicket3x` | 1d | 2h | 3× | — | warning (ticket) |
| `SLOErrorBudgetBurnTicket1x` | 3d | 6h | 1× | — | warning (ticket) |

The AND with the short window is what makes the alert *clear fast*: the moment the recent burn stops, the guard operand goes false and the alert resolves, even while the long window is still averaging in the past outage. Everything routes to the one Discord receiver via the existing severity convention (critical = page, warning = ticket) established in [[m3-observability-and-alerting]] — no new delivery wiring.

**The fast layer** (`qincloud.yml`) is the deliberate complement to the slow burn alerts. Burn alerts are, by design, patient — they wait to be sure the *budget* is at risk. But a tenant app dropping dead should page in minutes, not hours. So M7 also shipped three fast, direct alerts on the underlying gauges — `AppDown` (`qincloud_app_up == 0`), `PostgresDown` (`pg_up == 0`), `RedisDown` (`redis_up == 0`), each `for: 2m` to ride out a scrape blip. These close the gaps the failure catalogue flagged (a cleanly-stopped app, and the datastores, previously paged only indirectly or not at all) and were drilled live — see [[m9-failure-drills-postmortems]].

The whole pack loads with no `prometheus.yml` change: `rule_files` globs `/etc/prometheus/rules/*.yml` and both files match.

## 6. How this compares to best practice

The *structure* is exactly best practice: Google-SRE multiwindow-multiburn, error budgets, recording rules feeding uniform alerts, a budget-burn board. Where QinCloud honestly diverges is the **SLI quality**. A mature team measures user-visible success — request status codes, latency percentiles — because that is what the user actually feels. An up-ratio is a *coarser* proxy: it can't see an app that is "up" but serving 500s, or slow. QinCloud accepts that gap deliberately, because the alternative (standing up a request-metrics pipeline — Caddy metrics module or a log-derived SLI through Loki/Alloy) was more machinery than a one-box platform's traffic justifies *right now*, and the rule file names the corner cut and the upgrade path. The genuinely correct move here isn't the fancy SLI — it's refusing to *fake* one: a request SLI built on a metric that doesn't exist would be a beautiful, lying dashboard. A grounded up-ratio that says exactly what it measures is worth more than a textbook SLI that measures nothing.

## 7. The underlying why (the transferable lesson)

**An SLO is only as honest as the SLI underneath it, and the SLI is only as real as the metric you actually have — not the one the tutorial assumes.** The reflex M7 teaches is to query the running system for its real series *before* designing the alert, and when the ideal signal is missing, to pick the best grounded substitute and *document the substitution at the point of use* — so the gap is a known, named limitation instead of a hidden lie. The most dangerous monitoring artifact is not the alert that's missing; it's the confident dashboard built on a metric that was never there. Alert on the promise, measure the promise with a signal you've verified exists, and write down where the signal falls short of the ideal.

---
**Teaches:** [[observe-what-matters]] · [[single-source-of-truth]] · [[fail-loud-at-boundaries]] · [[verify-the-artifact-under-test]]
**Sources:** `stack/observability/prometheus/rules/slo.rules.yml`, `stack/observability/prometheus/rules/qincloud.yml`, `runbooks/drills/2026-07-07-appdown-pager-drill.md`
