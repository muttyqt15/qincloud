---
title: QinCloud Learnings
slug: readme
type: index
status: stable
tags: [qincloud, meta, index]
created: 2026-07-06
updated: 2026-07-06
related: ["[[conventions]]", "[[agents]]"]
---

# QinCloud Learnings

This is a **teaching vault**. Every milestone we built while turning a bare rented server into QinCloud is written up as a story that ramps from plain English an intelligent non-engineer can follow all the way down to the systems level a practitioner can operate from — commands, configs, `file:line`. Threaded through those stories are **atomic, reusable concepts**: the load-bearing principles the builds kept hitting, written once so they compound as every new milestone links back into them.

The point is not a changelog. It's that the *reasoning* survives the box — the approach we planned, where reality deviated, how we found the fix, and the transferable lesson underneath.

## How to read this

Every milestone note is built the same way, so you can stop wherever your curiosity does:

- **§1–4 are plain English.** What we were building and why it matters, the initial plan, where it went sideways, and how the real fix was found — each opened with an analogy. No prior knowledge assumed.
- **§5+ descend to the systems level** — the exact happy path, the gotchas, the `file:line` provenance, how it compares to production best practice, and the one transferable lesson.

The **`difficulty`** field in each note's frontmatter tells you the depth of the *deep* half: **1** = any reader, **5** = deep systems. It never gates the top of a note — §1 is always approachable — it just tells you how far the bottom goes.

## Start here

- **New to the project?** Read **[[m0-host-baseline]]** first — the foundation pour, and the clearest example of "the thing you assumed was guarding you wasn't."
- **Want the one piece we actually wrote?** Read **[[m4-controld-deploy-engine]]** — the deploy engine, a single linear state machine with one unbreakable rule.
- **Want the spine that runs through everything?** Read the concept **[[root-cause-over-patch]]**.

## Milestones

The build, in order. Each is one note: the narrative *and* the teaching.

| Milestone | Note | The hook |
| --------- | ---- | -------- |
| M0 | [[m0-host-baseline]] | Turn a raw, internet-exposed box into a locked, self-defending base with one rerunnable script — before any service lands. |
| M1 | [[m1-edge-and-tls]] | One Caddy front door: auto-HTTPS and on-the-fly rerouting — with one trap, that after first boot the *live* config, not the file, is truth. |
| M2 | [[m2-data-and-backups]] | Postgres and Redis on private nets, nightly off-site backups to R2 — and the part most skip: rehearsing the restore. |
| M3 | [[m3-observability-and-alerting]] | Stand up metrics, logs, and alerts — then *prove* a page reaches a human by killing a real service and watching it land in Discord. |
| M4 | [[m4-controld-deploy-engine]] | The only bespoke code: a Go state machine that deploys apps, whose single rule is *a failed deploy never takes down what was already running*. |
| M5 | [[m5-dashboard]] | A server-rendered dashboard with no JS framework — server draws the page, htmx re-fetches slices, all state in one Postgres table. |
| M6 | [[m6-first-app-umami]] | Go from "deploys a toy container" to running a real stateful product — which forces the deploy contract to grow secrets and a database. |
| M8 | [[m8-disaster-recovery]] | Deliberately destroy the whole server, then rebuild it from script, git, and offsite backups in ~12 minutes — proving the disposability claim. |
| Extra | [[x1-public-dashboard-cloudflare]] | Put the box-deleting dashboard on the public internet behind three independent locks — and the afternoon a stale config file stole. |
| Extra | [[x2-per-app-observability]] | Answer "how much is *this one app* using, and what is it logging?" after the standard container-metrics agent went silent on Docker 29. |
| Extra | [[x3-verified-edge-deploys]] | One idempotent command that applies an edge config change and *proves* it — validate, heal the mount, reload, restore routes, confirm each site responds. |

## Concepts

The atomic, reusable principles the milestones keep hitting. These are the compounding core — link into them, and they get richer with every build.

| Concept | The principle |
| ------- | ------------- |
| [[root-cause-over-patch]] | A failure is a pointer to a real defect — fix the origin so the whole *class* of bug can't recur, not just the spot that hurt. |
| [[fail-loud-at-boundaries]] | Validate at every trust boundary and stop at the cause — silently substituting a default for bad input is broken and lying about it. |
| [[single-source-of-truth]] | Every fact lives in exactly one place; copies drift. Derive from the source, never duplicate it. |
| [[make-invalid-states-unrepresentable]] | Don't guard against the bad state — make it impossible to construct in the first place. |
| [[adversarial-review]] | Point a fresh reviewer at your work whose only job is to make it lie or fall over — then test each finding against the running system. |
| [[layered-trust-defense-in-depth]] | No single wrong assumption should be fatal; stack independent controls so a gap in one is caught by the next. |
| [[idempotent-self-verifying-operations]] | Build operations you can safely re-run and that prove their own success before exiting. |
| [[the-box-is-disposable]] | The server is cattle, not a pet — the blueprint is what matters; you must be able to lose the box and rebuild it exactly. |
| [[verify-the-artifact-under-test]] | Before debugging why a fix "doesn't work," confirm the running system is actually executing your fix — otherwise it's a hypothesis, not a fix. |
| [[observe-what-matters]] | A metric or alert isn't done until you've seen it fire correctly, labelled the way a human asks the question. |
| [[shared-data-services-tenancy]] | One shared Postgres/Redis, many principals: the network grants reachability, per-app credentials grant authorization, one command owns provisioning. |

## For agents

Extending this vault? Read **[[agents]]** for how to add and link notes, and **[[conventions]]** for the frontmatter, filename, and linking rules. The teaching contract is non-negotiable: every note ramps from plain English to the systems level, and links liberally with `[[slug]]` so the graph stays navigable.
