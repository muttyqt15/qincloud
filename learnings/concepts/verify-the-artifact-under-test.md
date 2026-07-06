---
title: Verify the Artifact Actually Reached the System
slug: verify-the-artifact-under-test
type: concept
status: stable
difficulty: 3
tags: [qincloud, principle, reliability, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[x1-public-dashboard-cloudflare]]", "[[x3-verified-edge-deploys]]", "[[m3-observability-and-alerting]]", "[[root-cause-over-patch]]", "[[idempotent-self-verifying-operations]]", "[[observe-what-matters]]"]
sources: ["commit 5383d66", "runbooks/gotchas/dashboard.md:49", "stack/edge/Caddyfile:49-73"]
---

# Verify the Artifact Actually Reached the System

> **The principle in one line:** before you debug why a fix "doesn't work," confirm the running system is actually executing your fix — a change you can't observe on the live artifact is a hypothesis, not a fix.

## What it means (plain English)

You email the chef a new recipe. The dish comes out wrong, so you rewrite the recipe and send it again. Still wrong. You rewrite it five times — each version genuinely better — and every plate comes back broken. The problem was never the recipe. The chef never got your emails; he's been cooking from the laminated card taped to the wall the whole time. You were editing a document nobody in the kitchen could see.

Software debugging has the same trap. You change a file, redeploy, test, and the bug persists — so you assume your change was wrong and try the next one. But there is a hidden link in the chain: the change on your disk has to actually *reach* the process that's running. If that link is broken, every correct fix looks like a failure, and you'll burn an afternoon fixing a thing that was never the problem.

## Why it matters

The cardinal debugging sin is trusting the loop without confirming its input. You run *edit → deploy → test → conclude*, but the conclusion "my fix was wrong" is only valid if the deploy step truly delivered the fix. When it silently doesn't, you get a stream of false negatives that all point you *away* from the real defect — which lives in the delivery layer, not the code you keep rewriting. Worse, plausible-but-innocent bugs surface along the way ("red herrings sitting underneath the real bug") and eat hours because you have no way to tell a real failure from an undelivered one.

## Where it showed up in QinCloud

- **[[x1-public-dashboard-cloudflare]] — the stale bind mount.** Five correct Caddyfile edits all "failed": direct-to-origin hits that should have 404'd kept reaching the login page. The Caddyfile is a bind-mounted *single file*; `rsync`'s default writes a temp file and `rename()`s it, giving the host path a **new inode**, while the container stays pinned to the *old* inode from container-start. Host tools (`caddy adapt`, `grep`, `caddy reload`) all read the fresh file; only Caddy inside the mount read the ghost. The breakthrough was looking through the container's own eyes: `docker exec edge-caddy-1 grep '@cf' /etc/caddy/Caddyfile` — the marker wasn't there. Fix: `rsync --inplace` (`runbooks/gotchas/dashboard.md:49`).

- **[[x3-verified-edge-deploys]] — the ritual made permanent.** That afternoon is exactly why the edge deploy became a single self-verifying command that greps the marker *inside the container* after every change, instead of trusting that the copy landed.

- **[[m3-observability-and-alerting]] — "green ≠ tested."** A loaded, parsing Alertmanager config *looked* like a working pipeline, but the first real page died at the last hop. Same shape: a healthy-looking artifact is not proof the behavior actually runs end-to-end.

## How to apply it

When a fix "doesn't work," don't reach for the next fix — first prove the system is running *this* one. Inspect the artifact from the running process's vantage point, not your own: `docker exec` and read the file the container holds, hit the live endpoint and assert the behavior, check the version/hash the process reports. Bake the check into the deploy so it's [[idempotent-self-verifying-operations|self-verifying]] rather than a thing you remember to do. Only after the input to your debug loop is confirmed can a "still broken" result be trusted.

## Signs you're violating it

- You're on the second or third "correct fix that mysteriously failed" (the stale-artifact trap — go verify, don't keep patching, per [[root-cause-over-patch]]).
- You've confirmed the change on the file you *sent*, never on the file the process *reads*.
- Your evidence is host-side (`grep` on disk, `git log`, "the deploy said OK"), never observed from inside the running unit.
- "It reloaded / it's green / it parsed" is standing in for "the new behavior actually executes."

---
**Related:** [[root-cause-over-patch]] · [[idempotent-self-verifying-operations]] · [[observe-what-matters]]
