---
title: "M4 — controld: The Deploy Engine"
slug: m4-controld-deploy-engine
type: milestone
milestone: M4
status: stable
difficulty: 4
tags: [qincloud, golang, devops, reliability, infra]
created: 2026-07-06
updated: 2026-07-06
related: ["[[root-cause-over-patch]]", "[[make-invalid-states-unrepresentable]]", "[[fail-loud-at-boundaries]]", "[[idempotent-self-verifying-operations]]", "[[adversarial-review]]", "[[single-source-of-truth]]", "[[the-box-is-disposable]]"]
sources: ["controld/internal/deploy/contract.go", "controld/internal/deploy/deploy.go", "controld/internal/store/store.go", "controld/internal/dockerx/dockerx.go", "runbooks/gotchas/deploys.md"]
---

# M4 — controld: The Deploy Engine

> **In one sentence:** the only bespoke code in QinCloud — a small Go control plane that pulls an app's image, starts its container, points the edge at it, and records what it did, written as one linear state machine whose single unbreakable rule is *a failed deploy never takes down the app that was already running*.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Everything else in QinCloud is off-the-shelf: Caddy is the front door, Postgres holds the data, Prometheus and Grafana watch the vitals. `controld` is the one piece we actually wrote, and it does the job a stagehand does in a theatre. You want to swap the actor on stage for a new one. You don't turn the house lights off, drag the old actor away, and hope the new one knows their lines. You bring the new actor up behind the curtain, let them warm up, and only when they're ready do you swing the spotlight over — and *then* the old actor walks off. If the new one trips on the way up, the spotlight never moves and the audience never notices.

That's a **deploy**. "Deploy" just means: take a new version of an app and make it the one serving real users, without users seeing a gap. The whole reason to build a deploy engine — rather than manually `docker stop old; docker run new` — is that the manual version *is* turning the house lights off: there's a window where nothing is serving, and if the new thing is broken you've now got broken-and-down instead of old-but-working.

## 2. The plan (initial approach)

Model a deploy as a **state machine** — a short, fixed list of stages a deploy walks through, one after another, with no branches or loops. The stages (`controld/internal/deploy/contract.go:58`):

```
pending → pulling → starting → routing → live
   └────────┴─────────┴──────────┴──→ failed
```

Read top to bottom, that's the whole engine: create a record, pull the image, start the container and wait for it to be ready, route traffic to it, mark it live. Any stage can fall through to one terminal `failed` state. We write each stage to Postgres *before* running it, so the `deploys` table always tells you exactly where a deploy died.

The design leans on a deliberate split (`contract.go`): the state machine talks to four **interfaces** — `Docker` (run containers), `Router` (program Caddy), `Store` (persist), `Locker` (one deploy per app at a time) — and never to a real daemon. The messy real implementations live elsewhere (`internal/dockerx`, `internal/caddyapi`, `internal/store`). That keeps the state machine a pure decision-maker you can unit-test with fakes, no Docker required.

## 3. Where it deviated

The prime invariant sounds obvious once stated, but the first version of the failure-cleanup code quietly violated it. When a deploy failed, cleanup ran a sweep meaning *"remove every container for this app except the one the database says is live."* Reasonable on its face — kill the strays, keep the live one.

The trap: the database's idea of "which container is live" **can lag reality**. `SetLiveContainer` is its own write, and it can fail *after* the route has already been switched to a new container. Now the `container_id` column points at the *old* container while the *new* one is actually serving traffic. The next deploy fails at some early stage, runs the sweep, reads that stale column, keeps the old container it names — and removes **the routed one**. A *failed* deploy just caused *hard downtime*. That is the exact opposite of the one rule the engine exists to guarantee.

This wasn't caught by testing M4 in isolation. It surfaced during the **M5 dashboard review**, when someone traced adversarially through "what if the write between the route switch and the record fails?" — see [[adversarial-review]].

## 4. The fix — and how I found it

The patch would have been "add a nil-check / re-read the record." The [[root-cause-over-patch]] fix was to make the dangerous operation *unavailable* to the code paths that can't be trusted to run it safely. Two rules (`deploy.go:82`, `runbooks/gotchas/deploys.md`):

1. **A failure path may only remove what it itself created** — by the container's *deterministic name*, `qc-<app>-<deployID>` (`removeNew`, `deploy.go:169`). It never consults the stored "live" record, so a stale record can't misdirect it.
2. **Only the happy path sweeps** (`cleanupKeeping`, `deploy.go:235`), and it keeps `newID` — the container *this very deploy* just routed, fresh in a local variable, never read back from state. A sweep keyed on a value that cannot be stale cannot delete the wrong thing.

Any stray a failed cleanup leaves behind is harmless and gets swept by the *next* successful deploy's happy-path retirement. The generalised class, worth remembering: **any "clean up everything except X" is a landmine when X is read from state that can lag reality.** Fix it by keying on something you hold in hand, not something you look up.

## 5. Going deep (systems level)

**The happy path, exactly** (`deployLocked`, `deploy.go:90`): `UpsertApp` → `CreateDeploy` (returns the deploy `id`) → `step(pulling, Pull)` → `step(starting, StartApp + WaitReady)` → `step(routing, UpsertRoute)` → `SetLiveContainer(newID)` → `cleanupKeeping(newID)` → `SetDeployStatus(live)`. The old container is dialled by Caddy right up until `UpsertRoute` swings the route to `qc-<app>-<id>:<port>`; only after that does the old one get retired.

**Detached cleanup.** Everything after the route switch runs on `context.WithoutCancel(ctx)` with a fresh 30s budget (`deploy.go:144`). Reason: the deploy's own context dying — the CLI's 5-minute budget expiring, a Ctrl-C — is itself a *common failure cause*. If you recorded the failure (or the success) on the already-dead context, that write would fail instantly, leaving the `deploys` row stuck mid-state exactly when the "history tells you which step it died in" promise matters most. So `failDeploy`, `removeNew`, and the post-route bookkeeping all detach first.

**Order within the tail.** `SetLiveContainer(newID)` is recorded *before* retiring the old container (`deploy.go:151`). Dying between the two leaves only an extra unrouted container (swept later). The reverse order could leave `container_id` pointing at a container that's already gone. And if `SetLiveContainer` *fails*, the code removes **nothing** and fails loud (`deploy.go:156`) — both containers stay up, app still serving, next deploy re-records and sweeps. That's [[fail-loud-at-boundaries]]: never warn-and-continue on a write that governs which container is real.

**Per-app locking.** Every CLI call is its own `docker exec` process, so an in-process mutex is useless. The lock is a Postgres advisory lock, `pg_try_advisory_lock(hashtext('qc-app:' || name))` on a *dedicated* connection (`contract.go:133`, runbook). `try` = fail fast, don't block: a second concurrent deploy of the same app errors out immediately rather than interleaving its route-switch with the first's sweep. The lock dies with the session, so a crashed deploy can't wedge the app forever.

**Redeploy is TOCTOU-safe.** `Redeploy(app)` reads the stored spec **inside** the lock (`deploy.go:65`). Read it outside, and a concurrent `Destroy` could win the lock, remove the app, and then your stale spec would *resurrect a destroyed app* — an outcome no serial ordering of the two operations can produce. This is also why the runbook says never re-type `-env` flags by hand to restore a route: it clobbered umami's stored `APP_SECRET` live on 2026-07-06. `controld redeploy` replays the *stored* spec, secrets included.

**Readiness is a pure function.** `decideReadiness(state, runningFor, grace)` (`dockerx.go:221`) is the one piece of real judgement, pulled out of the daemon-polling loop so it unit-tests with no Docker: healthcheck image → Docker's verdict wins; no healthcheck → "running continuously for 3s" is ready; `exited`/`dead`/`restarting` → fatal, stop polling now (during a deploy one crash means the deploy is bad).

**The resource fence.** Every app container runs capped at 512MB memory, 2 of 4 vCPUs, 256 PIDs, plus `no-new-privileges` (`dockerx.go:71`). A memory cap alone won't stop a fork bomb from exhausting host PIDs and hanging the whole box — so pids and CPU are fenced alongside memory. One buggy tenant image must never take the box down.

## 6. How this compares to best practice

A managed PaaS (Fly, Render, Heroku) does the same *shape* — health-gated rolling deploy, old kept until new is ready — but with orchestrator-grade machinery: multiple replicas, gradual traffic shifting, automatic rollback on error-rate spikes, a reconciler loop that continuously drives real state toward desired state. controld is the honest single-box distillation: one replica, an atomic route flip instead of gradual shift, and **no background reconciler** — a rebuilt Caddy (fresh volume, no routes) is reconciled *manually* by `controld list` + redeploy each app (runbook; see [[the-box-is-disposable]]). That reconciler is stubbed-out on purpose in `stack/controld/compose.yml`, a deliberate [[idempotent-self-verifying-operations]] corner cut: every operation (`Init`, `RemoveContainer`, `DeleteRoute`, "absent is not an error") is already idempotent, so the reconciler *could* be a simple resweep — we just haven't needed it enough to build it. The tradeoff we accepted: after a full edge rebuild, routes are gone until a human runs the reconciliation. That's fine at one box and one operator; it's the first thing that needs building the day this grows past that.

## 7. The underlying why (the transferable lesson)

The engine is trustworthy because it treats **the last-written record as a possibly-stale opinion, not as truth.** Every dangerous action is keyed on a value the code is *holding* (the deterministic container name, the fresh `newID`), never on a value it *looked up* and might be lagging. That single discipline — decide destructive actions from data you own this instant, not from persisted state that could have drifted — is what turns "delete everything except the live one" from a landmine into a safe sweep. The deploy engine is small enough to hold in your head precisely because it refuses to branch on stale reads; it just walks a straight line and, when a line ends early, cleans up only what it personally put there. Root-cause over patch, invariants over checks, one straight path over a web of special cases.

---
**Teaches:** [[root-cause-over-patch]] · [[make-invalid-states-unrepresentable]] · [[fail-loud-at-boundaries]] · [[idempotent-self-verifying-operations]] · [[adversarial-review]] · [[single-source-of-truth]] · [[the-box-is-disposable]]
**Sources:** `controld/internal/deploy/contract.go`, `controld/internal/deploy/deploy.go`, `controld/internal/store/store.go`, `controld/internal/dockerx/dockerx.go`, `runbooks/gotchas/deploys.md`
