---
title: Make Invalid States Unrepresentable
slug: make-invalid-states-unrepresentable
type: concept
status: stable
difficulty: 3
tags: [qincloud, principle, databases, golang]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m4-controld-deploy-engine]]", "[[m6-first-app-umami]]", "[[m5-dashboard]]", "[[fail-loud-at-boundaries]]", "[[single-source-of-truth]]", "[[root-cause-over-patch]]"]
sources: ["controld/internal/store/schema.sql", "controld/internal/deploy/contract.go", "controld/internal/deploy/deploy.go"]
---

# Make Invalid States Unrepresentable

> **The principle in one line:** don't detect-and-repair a bad state after it happens — shape your types and schema so the bad state *cannot be written down in the first place*.

## What it means (plain English)

A form asks for your birth month as a free-text box. People type "Jamuary", "13", "next Tuesday" — so now you write cleaning code to catch every wrong thing, forever, and you'll still miss one. Swap the text box for a dropdown of twelve months and the entire class of error is *gone*: "Jamuary" was never expressible. You didn't validate harder; you removed the ability to be wrong.

That's the move. Instead of accepting anything and policing it downstream, constrain the shape so only valid things fit.

## Why it matters

Repair-after-the-fact is a losing game: you can only guard against the bad states you *thought of*, the guards rot, and two guards can disagree (see [[single-source-of-truth]]). Making a state unrepresentable is a *proof*, not a hope — the compiler or the database rejects it at the boundary, loudly, at the moment of the mistake, not three layers downstream where it's unrecognizable. It also shrinks the code: no cleaning routines, no defensive clutter, no "can this be nil here?" archaeology.

## Where it showed up in QinCloud

- **[[m4-controld-deploy-engine]] — one host, one app.** Two apps claiming the same hostname is an invalid state: the edge would silently give one of them zero traffic. Rather than write code to *check for* collisions, the schema makes it impossible — `CREATE UNIQUE INDEX apps_host_key ON apps (host)` (`schema.sql`). A second claim doesn't get validated and rejected by app code; the *database* refuses the write, and the store translates that into "host already routed by app X." The bad state cannot exist on disk.

- **[[m4-controld-deploy-engine]] — the deferred field trick.** The review-found downtime bug came from deriving a fact ("which container to keep") from a value that could be stale. The deep version of this principle: the pre-projection type simply *omits* the field that could drift, so no code can accidentally read a lagging copy (`deploy.go`, and the pattern write-up in `deploys.md`). If the wrong value can't be named, it can't be used. See [[root-cause-over-patch]].

- **[[m6-first-app-umami]] — secrets you can't leak by accident.** App environment values are secrets. The dashboard renders env *keys* only — the value is never placed into a template that could echo it. And [[m5-dashboard]] uses **distinct types over optional fields** (a subtype for "this context has X" instead of `X?` everywhere), so "forgot to check the optional" stops being a reachable mistake.

## How to apply it

Before writing a validation check, ask: **can I make this state impossible to represent instead?** Reach for the tools that enforce at a boundary — database `UNIQUE`/`CHECK`/`NOT NULL` and foreign keys, non-null types, sum types / enums instead of stringly-typed flags, subtypes instead of `field?: T`, newtypes for units. Push the constraint as close to where data is *born* as you can. Pair it with [[fail-loud-at-boundaries]]: the boundary should reject and shout, never silently coerce.

## Signs you're violating it

- A "cleanup" / "sanitize" / "reconcile" routine that runs after the fact to fix data.
- Nullable fields that are "always set, except sometimes" — and scattered `if x != nil` guards.
- A comment like "this should never happen" next to code handling it happening.
- Bugs that are *invalid combinations* of otherwise-valid fields (status says live, but no container).

---
**Related:** [[fail-loud-at-boundaries]] · [[single-source-of-truth]] · [[root-cause-over-patch]]
