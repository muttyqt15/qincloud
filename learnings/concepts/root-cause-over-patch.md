---
title: Root Cause Over Patch
slug: root-cause-over-patch
type: concept
status: stable
difficulty: 2
tags: [qincloud, principle, reliability, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m3-observability-and-alerting]]", "[[m4-controld-deploy-engine]]", "[[x1-public-dashboard-cloudflare]]", "[[fail-loud-at-boundaries]]", "[[make-invalid-states-unrepresentable]]", "[[adversarial-review]]"]
sources: ["controld/internal/deploy/deploy.go:82", "stack/observability/compose.yml:232", "runbooks/gotchas/dashboard.md:49", "runbooks/gotchas/alerting.md"]
---

# Root Cause Over Patch

> **The principle in one line:** a visible failure is a *pointer* to a real defect — keep asking "why was this even possible?" until you reach the origin, then fix that so the whole *class* of bug cannot recur.

## What it means (plain English)

Your kitchen ceiling has a wet stain. You can repaint it. The stain comes back next month, so you repaint again. The paint is a **patch**: it hides the symptom. The **root cause** is a cracked pipe in the floor above. Fix the pipe once and *no* stain — this one or the next — can ever appear again.

Software is the same. The crash, the missed alert, the stale file: each is the stain. The temptation is to slap a fix exactly where it hurts — a null-check here, a retry there. But if you stop at the stain you have signed up to repaint forever, because the pipe is still cracked and it will leak somewhere new next time.

## Why it matters

A patch makes *one instance* disappear. A root-cause fix makes the *category* impossible. Patch-driven work quietly multiplies: every symptom gets its own special case, the code fills with defensive clutter, and the same bug keeps resurfacing wearing a new hat. You end up maintaining the workarounds instead of the system. The tell: after your fix you still think *"this could break again the same way."* If so, you stopped too early.

## Where it showed up in QinCloud

- **[[m3-observability-and-alerting]] — the page that never sent.** Alertmanager was green, config parsed, target up — but the first real alert died at the last hop: `permission denied` reading the Discord webhook (`600 root:root`, container runs as uid 65534). The patch was one `chmod`. The root cause was deeper: *trusting a loaded config as proof of a working pipeline*. The durable fix (`compose.yml:232`) plus the drill that holds a service dead past the flush window kills the whole class — "green ≠ tested."

- **[[m4-controld-deploy-engine]] — the stale container id.** Failure-cleanup swept "every container except the one the DB says is live," but that column can lag reality, so a *failed* deploy could delete the *routed* container and cause hard downtime. The patch was "re-read the record / add a nil-check." The root-cause fix (`deploy.go:82`) made the dangerous move *unavailable*: failure paths only remove containers by the deterministic name they created; only the happy path sweeps, keyed on the fresh `newID` it holds in hand. See [[make-invalid-states-unrepresentable]] and [[adversarial-review]].

- **[[x1-public-dashboard-cloudflare]] — the stale mount.** The dashboard served a stale Caddyfile no matter how many times it reloaded. Hours went into two plausible culprits (colliding `remote_ip` matchers, a comment eating directives) — real bugs, but red herrings. The origin (`dashboard.md:49`): `rsync` renames a temp file, giving the host path a *new inode* while the container clings to the old one. One root cause under several patches. Fix: `rsync --inplace`.

## How to apply it

When something breaks, ask **"why was this state even reachable?"** and keep going one level down — past the crash site, past the bad value, to the assumption or missing invariant that let the path exist. Then fix *there*, and — per coding principle #3 — write the regression test that is RED without the fix. Prefer making the bad state unrepresentable over guarding against it (see [[make-invalid-states-unrepresentable]], [[fail-loud-at-boundaries]]).

## Signs you're violating it

- Your fix is a `try/catch`, `?? default`, retry, or nil-check exactly where it crashed.
- You can't name what *produced* the bad input — only where it landed.
- The same bug keeps coming back wearing a slightly different hat.
- You're on the third patch for "the same area" and each looked plausible (the stale-mount trap).
- After shipping, you still think: *"this could break again the same way."*

---
**Related:** [[fail-loud-at-boundaries]] · [[make-invalid-states-unrepresentable]] · [[adversarial-review]]
