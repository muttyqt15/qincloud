---
title: "M9 — Failure Drills and Blameless Postmortems"
slug: m9-failure-drills-postmortems
type: milestone
milestone: M9
status: stable
difficulty: 3
tags: [qincloud, reliability, alerting, process, sre]
created: 2026-07-07
updated: 2026-07-07
related: ["[[observe-what-matters]]", "[[root-cause-over-patch]]", "[[verify-the-artifact-under-test]]", "[[fail-loud-at-boundaries]]", "[[make-invalid-states-unrepresentable]]", "[[m3-observability-and-alerting]]", "[[m7-slos-and-burn-alerts]]", "[[m8-disaster-recovery]]"]
sources: ["runbooks/drills/failure-catalogue.md", "runbooks/postmortem-template.md", "runbooks/drills/2026-07-07-appdown-pager-drill.md", "stack/observability/prometheus/rules/qincloud.yml"]
---

# M9 — Failure Drills and Blameless Postmortems

> **In one sentence:** we wrote down every way the platform can break, marked honestly which of those actually page a human today, turned the gaps that revealed into new alerts, and drilled one live — then gave incidents a blameless postmortem template so a failure produces a durable rule, not a shrug.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

There is a special cruelty to monitoring: **a broken monitor looks exactly like a healthy one.** Both are green. Both are quiet. The difference only reveals itself the day a real outage sails straight past the alert that was supposed to catch it — and by then it's an incident, not a finding. You cannot tell, by looking, whether your safety net has a hole in it. The only way to know is to *throw something at it on purpose* and watch whether it catches.

M9 built two things. First, a **failure-injection catalogue**: a written list of the ways QinCloud can fail — an app crash-looping, an exporter going dark, a backup silently stalling, a database falling over, the whole box dying — each with the exact command to *cause* it, the alert that *should* fire, and, crucially, an honest note when the answer is "nothing pages today." Second, a **blameless postmortem template**: the discipline that turns an incident into a fixed *class* of bug instead of a patched symptom and a bad memory.

The theme running through both: reliability isn't a feeling of safety, it's evidence. "I think that would page" is a hope. "I stopped the container and watched Discord light up 2m30s later" is a fact.

## 2. The plan (initial approach)

The plan looked like paperwork. Enumerate the failure modes, write a drill for each, write a postmortem template, done. Concretely:

1. Catalogue the failures as drills A–E (crash-loop, target-down, backup-stall, DB-outage, full-box-loss), each grounded in a metric **verified present on the live Prometheus** and an alert **that actually exists** in `qincloud.yml` — no aspirational entries.
2. For each, record the inject command, the expected alert and its timing, the recovery/RTO, and the prod-safety class (safe to run / maintenance window only).
3. Write the postmortem template around **root cause over patch** ([[root-cause-over-patch]]): a 5-whys section, a "what went well" (so working controls survive future cleanups), an honest "where we got lucky," and action items that each kill a *class*.
4. Run at least one drill live to prove the catalogue isn't fiction.

I expected step 1 to be transcription. It turned into an audit.

## 3. Where it deviated

Writing the catalogue *honestly* — with the rule that every entry names the alert that actually exists — forced me to check each failure against the real alert pack. And the pack had holes I would never have found by reading it top-to-bottom:

- **A cleanly stopped app did not page at all.** `ContainerRestartLoop` catches an app that *crash-loops* (boots, dies, restarts, repeat). But `docker stop qc-whoami` — an app that stops and *stays* stopped — produces no restart churn, so nothing fired. `qincloud_app_up{app="whoami"}` dropped to 0 on the dashboard, and **no rule watched it.** The most ordinary failure (an app is just... down) was invisible to the pager.
- **The databases didn't page on their own signal.** There was no alert on `pg_up == 0` or `redis_up == 0`. Postgres paged only *indirectly* — controld reads the DB on every `/metrics` scrape, so a dead Postgres eventually took the controld scrape target down and fired `InstanceDown{job="controld"}` ≈4m40s later. A **Redis** outage paged *nothing*.

None of these were bugs in a drill. They were real gaps in coverage that only became visible the moment I sat down to write "here is the alert that catches this" and had to admit, for three rows, that there wasn't one.

## 4. The fix — and how I found it

The finding *was* the work — the catalogue's honesty requirement is what surfaced the gaps. The fix was to close them at the cause, not to route around them.

Each ❌ became a **fast, direct alert on the underlying gauge** (added to `qincloud.yml` alongside the M7 SLO work): `AppDown` on `qincloud_app_up == 0`, `PostgresDown` on `pg_up == 0`, `RedisDown` on `redis_up == 0` — each `for: 2m` so a scrape blip doesn't page. The principle is [[fail-loud-at-boundaries]] applied to alerting: page on the datastore's *own* signal, at the cause, not three layers downstream via a coincidental scrape failure. A Postgres outage should say "Postgres is down," not "the controld scrape target is unreachable" and leave the on-call to infer why.

Then — because an armed alert is still just a hope until it's *seen* to fire ([[observe-what-matters]]) — I drilled `AppDown` live against the throwaway whoami app. `docker stop qc-whoami-35` at **T0 17:29:55 UTC**; `qincloud_app_up{whoami}` → 0 within a scrape; alert **pending** through the `for: 2m` hold; **firing** at +150s; Alertmanager showed `AppDown{app=whoami}` active and routed to the Discord receiver (the delivery path itself already proven in the [[m3-observability-and-alerting]] pager drill). `docker start` at +155s; recovered and resolved by +175s. **Detection-to-page ≈ 2m30s** — the `for: 2m` anti-flap window plus a scrape/eval cycle — which is the *designed* latency, not a defect: an app blip should not page.

## 5. Going deep (systems level)

**The catalogue** (`runbooks/drills/failure-catalogue.md`) is the operational heart of M9. Its coverage table is deliberately blunt about what pages and what doesn't:

| Failure | Fires today | Rule / signal |
| --- | --- | --- |
| Scrape target down | ✅ | `InstanceDown` (`up == 0`, `for: 3m`) |
| Container crash-loop | ✅ | `ContainerRestartLoop` (`changes(container_start_time_seconds[15m]) > 2`) |
| Backup stalled | ✅ | `BackupStale` (`time() - last_success > 129600`) |
| Disk >85% / Mem >90% | ✅ | `DiskUsageHigh` / `MemoryHigh` |
| App stopped (not looping) | ✅ *(M9)* | `AppDown` (`qincloud_app_up == 0`) |
| Postgres down | ✅ *(M9)* | `PostgresDown` (`pg_up == 0`) — plus the indirect `InstanceDown{job=controld}` |
| Redis down | ✅ *(M9)* | `RedisDown` (`redis_up == 0`) |
| Full-box loss | ❌ by design | — → external uptime check (the one known blind spot, from [[m8-disaster-recovery]]) |

Two pieces of hard-won operational timing live in the catalogue's preamble, because a drill that ignores them "passes" while proving nothing:

- **Hold the failure ≥ 60s past *firing*.** For an `up==0` alert the detection→page path is ≈4m40s (15s scrape + `for: 3m` + eval + `group_wait: 30s`). Recover too early and the alert resolves before `group_wait` flushes — no page is ever sent, and you've tested nothing.
- **Read evidence off the box, not the laptop.** Container uptimes, Prometheus alert state, Alertmanager logs — a local ssh poll can hang mid-observation and lie to you.

The one drill left un-runnable-safely is **E (full-box loss)**: by construction it pages *nothing*, because Alertmanager lives on the box it's meant to watch. The catalogue carries that as a standing action item (an external uptime check) rather than pretending it's covered — the same blind spot [[m8-disaster-recovery]] surfaced, honestly logged in both places.

**The postmortem template** (`runbooks/postmortem-template.md`) enforces the culture side. It is blameless by construction — "if a line makes someone look stupid, rewrite it to describe the trap they fell into" — and structurally refuses to stop at the symptom: a mandatory 5-whys that ends only at a missing invariant, a "what went well" section so controls that earned their keep survive future cleanups, an honest "where we got lucky" (a latent incident with the fuse already lit), and action items that each kill a *class* and prefer "make the bad state unrepresentable / fail loud at the cause" over "be more careful." The template ships with a **worked example** filled from the real M3 drill — the Alertmanager `permission denied` near-miss where a green healthcheck stood in for a working notify path — so the shape is concrete, not abstract. The wiring rule is explicit: **drills** (`drills/`) are the immutable narrative of *what happened*; **gotchas** (`gotchas/`) are the living rules of *what to do*; a postmortem feeds both and lands its rule in the same commit as the fix.

## 6. How this compares to best practice

This is textbook SRE, and deliberately so: **chaos engineering / game days** (inject failure on purpose), **blameless postmortems** (the culture that lets you find root causes instead of scapegoats), and the discipline that an alert isn't "done" until it has fired in anger. Where a big team runs game days against staging with a blast-radius controller and automated fault injection (a Chaos Monkey), QinCloud runs them by hand against a throwaway container on prod, with pre-announce/silence discipline and a maintenance-window classification for the dangerous ones — appropriate to a one-box platform. The genuinely first-class habit here is the **honesty of the coverage table**: most monitoring setups quietly assume they're complete. QinCloud's catalogue enumerates its own blind spots as first-class rows and carries them as tracked debt. A mature team would recognise the missing pieces (automated scheduling of the drills, an external uptime check for the box-loss blind spot) — both are named action items, not surprises.

## 7. The underlying why (the transferable lesson)

**You do not know what your monitoring covers until you make the bad thing happen and watch for the signal — and the act of writing down "here is the alert that catches this" is itself the audit that finds the holes.** M9's real payload wasn't the drills or the template; it was the three missing alerts that only became visible the moment I held every failure mode up against the real rule pack and demanded an honest answer. A quiet, green, armed monitoring stack is not evidence of safety — it is the *absence* of evidence, which looks identical right up until it isn't. So enumerate the failures, name the ones nothing catches, close those at the cause, and prove each fix by firing it on purpose. Reliability is a thing you *demonstrate*, on a schedule, not a thing you *assume*.

---
**Teaches:** [[observe-what-matters]] · [[root-cause-over-patch]] · [[verify-the-artifact-under-test]] · [[fail-loud-at-boundaries]]
**Sources:** `runbooks/drills/failure-catalogue.md`, `runbooks/postmortem-template.md`, `runbooks/drills/2026-07-07-appdown-pager-drill.md`, `stack/observability/prometheus/rules/qincloud.yml`
