---
title: Adversarial Review (Prove It Wrong)
slug: adversarial-review
type: concept
status: stable
difficulty: 3
tags: [qincloud, principle, reliability]
created: 2026-07-06
updated: 2026-07-06
related: ["[[verify-the-artifact-under-test]]", "[[root-cause-over-patch]]", "[[fail-loud-at-boundaries]]"]
sources: ["controld/internal/deploy/deploy.go:82", "controld/internal/dashboard/dashboard.go", "controld/internal/dashboard/observe.go", "stack/edge/Caddyfile:49-73"]
---

# Adversarial Review (Prove It Wrong)

> **The principle in one line:** point a fresh reviewer at your work whose only job is to make it *lie or fall over* — then check each finding against the running system, because a claim you can test empirically must not be argued.

## What it means (plain English)

There are two ways to look at something you built. The first asks *"does it work?"* — you run the happy path, it works, you move on. The second asks *"how does it break, and how does it fool me?"* — you actively hunt for the input, the timing, the failure that makes it misbehave.

Think of a building inspector who doesn't wait to be shown the finished rooms. She yanks on the railings, floods the drain, kills the power mid-lift, and asks "what happens *now*?" The builder was proving the building *right*; the inspector's whole job is to prove it *wrong*. The gap between those two mindsets is where every serious bug hides — because the builder already, unconsciously, avoids the paths that break it.

## Why it matters

The happy path passing tells you almost nothing about safety. The dangerous bugs live one "what if this write fails *here*?" away from the code you were proud of. One confident pass by the author can't find them, because the author shares the assumptions that created them. A second reader whose success is measured in *defects found*, not in agreeing, breaks that symmetry.

## Where it showed up in QinCloud

- **The M4 latent-downtime bug was found by the M5 review, not by testing M4.** Reviewing the dashboard ([[m5-dashboard]]), someone traced adversarially through "what if `SetLiveContainer` fails *after* the route switched?" and found that a *failed* deploy's cleanup sweep, reading a stale `container_id`, would remove **the container actually serving traffic** ([[m4-controld-deploy-engine]] `deploy.go:82`). Hard downtime, invisible in isolation.
- **The M5 dashboard review found three ways the UI lied** — a stopped container rendering a confident `cpu 0.0% · mem 0 MiB`, a 3s poll wiping the double-click guard, and a destroyed app's page polling forever. Each was "the page looks fine, the state underneath is wrong."
- **The public-dashboard review surfaced a DoS lever** ([[x1-public-dashboard-cloudflare]]): an anonymous flood could force one bcrypt hash per request, so the `@cf` matcher was made to 404 *before* any hash runs. The same stopped-container "0% is a lie" class recurred in per-app metrics ([[x2-per-app-observability]]), fixed by emitting `qincloud_app_up 0` and omitting the fake numbers.

## How to apply it

Assign the reviewer the goal **"make this lie or crash,"** not "confirm it works." Walk every write and ask "what if this one fails and the next runs anyway?" And the non-negotiable step: **any finding that can be checked against the running system must be** — `docker exec … grep` the config the server actually holds ([[verify-the-artifact-under-test]]), `curl` the origin IP and expect a 404, kill a container and read the metric. A verified finding is a fact; an argued one is a guess.

## Signs you're violating it

- The only reviewer is the author, and review means re-reading the happy path.
- Findings are debated in the abstract when a one-line command would settle them.
- "It works on my machine" closes a thread instead of "here's the failing case."
- Bugs get patched where they *surface*, never traced to where the wrong state was *born* ([[root-cause-over-patch]], [[fail-loud-at-boundaries]]).

---
**Related:** [[verify-the-artifact-under-test]] · [[root-cause-over-patch]] · [[fail-loud-at-boundaries]]
