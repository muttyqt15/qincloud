---
title: Observe What Matters (and Prove the Signal)
slug: observe-what-matters
type: concept
status: stable
difficulty: 3
tags: [qincloud, principle, observability, reliability]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m3-observability-and-alerting]]", "[[x2-per-app-observability]]", "[[verify-the-artifact-under-test]]", "[[fail-loud-at-boundaries]]"]
sources: ["stack/observability/prometheus/rules/qincloud.yml", "controld/internal/dashboard/observe.go", "runbooks/gotchas/alerting.md", "stack/observability/alertmanager/alertmanager.yml"]
---

# Observe What Matters (and Prove the Signal)

> **The principle in one line:** a metric or alert isn't done until you've *seen it fire correctly*, and it must be labelled the way a human asks the question.

## What it means (plain English)

A smoke detector you installed but never tested is not safety equipment — it's a plastic disc on the ceiling. You don't *know* it works until you've held a match under it and heard it scream, and then heard it go quiet when the smoke clears. Monitoring is the same. Installing Prometheus, writing an alert rule, wiring a dashboard panel — none of that is "done." Done is: *I made the bad thing happen and watched the right signal arrive.* And the signal has to speak your language — if the fire alarm just prints a serial number instead of "kitchen," you'll still burn the house down looking it up.

## Why it matters

Observability's whole job is to be trustworthy at 3am. Every untested piece is a lie you'll believe exactly when you can least afford to. The failure mode is uniquely nasty: a broken monitor looks *identical* to a healthy one — green, quiet, armed — right up until the outage it was supposed to catch sails past unseen. You don't get an error; you get silence, and you mistake silence for "all clear."

## Where it showed up in QinCloud

- **The inert alert on a metric that never existed.** In [[m3-observability-and-alerting]], `BackupStale` shipped referencing `qincloud_backup_last_success_timestamp_seconds` — a metric no job published yet. Prometheus evaluated the expr against an empty vector, matched nothing, and *never pended*. The rule looked armed and was a no-op (`stack/observability/prometheus/rules/qincloud.yml`). An alert isn't done until its metric has been observed in Prometheus *and* the expr returns a value.
- **Labelling by the question, not the container.** In [[x2-per-app-observability]], cAdvisor labels every series by raw container ID (a 64-hex string) — because that's all it knows. But operators and alerts ask `app="umami"`, not `name="3f9a…"`. So controld publishes its own `qincloud_app_*` gauges with first-class `app="<name>"` labels (`controld/internal/dashboard/observe.go`), since the component that *deployed* the container is the only honest source of its identity.
- **The drill that must outlive `group_wait`.** [[m3-observability-and-alerting]]'s first outage drill restarted the killed service seconds after firing — the alert resolved before Alertmanager's `group_wait: 30s` flushed, so *no page sent*, and the drill "passed." You must hold the failure ≥60s past firing, then wait out `group_interval: 5m` for the resolved page.

## How to apply it

- Wire the metric *before or with* the alert; verify with an instant query that returns a value below threshold.
- Manufacture the failure end-to-end: kill the real target, hold it past the whole timing chain (scrape 15s + `for:` + `group_wait`), read evidence off the box.
- Label series with the identity humans query by; own that label in the component that knows it.

## Signs you're violating it

- "The config loaded / healthcheck is green" stands in for "it works."
- An alert you've never seen change from inactive → pending → firing.
- Series keyed by IDs nobody types into a query box.
- A drill fast enough to be convenient — it recovered before the pipeline could act.

---
**Related:** [[verify-the-artifact-under-test]] · [[fail-loud-at-boundaries]]
