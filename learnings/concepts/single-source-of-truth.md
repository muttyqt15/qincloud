---
title: One Source of Truth per Fact
slug: single-source-of-truth
type: concept
status: stable
difficulty: 2
tags: [qincloud, principle, reliability]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m5-dashboard]]", "[[m1-edge-and-tls]]", "[[x3-verified-edge-deploys]]", "[[m4-controld-deploy-engine]]", "[[make-invalid-states-unrepresentable]]", "[[root-cause-over-patch]]"]
sources: ["controld/internal/dashboard/observe.go", "controld/internal/deploy/deploy.go", "runbooks/gotchas/caddy.md", "runbooks/gotchas/deploys.md"]
---

# One Source of Truth per Fact

> **The principle in one line:** every fact has exactly one authority that owns it — everything else *derives* from that authority and never keeps its own copy.

## What it means (plain English)

Imagine a company where the HR system, the payroll spreadsheet, and your manager's sticky note each record your job title — and they disagree. Which one is *true*? Whichever you happened to ask. Now every question about your title is a coin flip, and "fixing" it means updating three places and hoping you didn't miss one.

The cure is to name **one** record as authoritative — say, HR — and make payroll and the sticky note *look it up* instead of storing their own version. Now there is nothing to disagree with. One fact, one owner, one place to change it.

## Why it matters

The moment a fact lives in two places, they *will* drift — not maybe, will. Some code path updates one and not the other, and now the system holds two contradictory truths and behaves according to whichever it reads. These bugs are miserable because the code at the failure site looks correct; the lie was written somewhere else, earlier. Worse, "fix" attempts add a *third* copy ("a cache to reconcile them"), which just adds a third thing to drift.

## Where it showed up in QinCloud

- **[[m5-dashboard]] — the deploys table is the only status.** The dashboard could have kept its own in-memory map of "which deploys are running." It deliberately does not. Every status badge, the history table, the `/metrics` gauges — all *re-read* the Postgres `deploys` table that the deploy engine writes (`observe.go`). Consequence: the dashboard can crash and restart mid-deploy and lose *nothing*, because it never owned the truth in the first place. Adding a local copy would have been the drift bug the projection exists to prevent.

- **[[m4-controld-deploy-engine]] — which container is live.** Exactly one column, `apps.container_id`, owns "the currently serving container." When a bug let a *second* authority (a failure-cleanup routine's assumption) act on stale copies of that fact, a failed deploy could delete the live container. The fix restored single ownership: only the happy path records liveness, keyed on the container it holds in hand (`deploy.go`). See [[root-cause-over-patch]].

- **[[m1-edge-and-tls]] / [[x3-verified-edge-deploys]] — the split that bit us.** This is the *counter-example*, and it hurt. Caddy's routing config has two authors: the `Caddyfile` (the seed) and the live admin API that `controld` programs routes through. They fight — a Caddyfile reload wipes the API-added routes (`caddy.md`). Because the truth was split, every edge change became a fragile "reload, then re-assert all the routes" dance, and a whole afternoon of confusion. The mitigation ([[x3-verified-edge-deploys]]) is a script that treats the reload+restore as one atomic, verified operation — bandaging a seam that a single source of truth would not have.

## How to apply it

For each important fact, ask: **who owns this?** Pick one. Everything else reads from it — a query, a projection, a function call — never a second stored copy. When you feel the urge to cache or mirror a fact "for convenience," treat that as a design smell first (see [[make-invalid-states-unrepresentable]]): can the derived thing be *computed* on read instead? If two authorities are unavoidable (Caddyfile vs admin API), make the reconciliation one explicit, verified operation rather than an implicit hope.

## Signs you're violating it

- The same fact is stored in two tables / files / services and some code "keeps them in sync."
- A bug where the code at the crash site is correct but the data it read was stale.
- Your fix for drift is a *third* copy (a cache, a reconciler) rather than deleting a copy.
- "It depends which one you ask" is a sentence anyone can say about your system.

---
**Related:** [[make-invalid-states-unrepresentable]] · [[root-cause-over-patch]] · [[x3-verified-edge-deploys]]
