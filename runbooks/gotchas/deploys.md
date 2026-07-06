# controld deploy gotchas & invariants

Code: `controld/internal/deploy/deploy.go` (state machine),
`deploy_test.go` (the regression net — these invariants each have a test).

## The prime invariant: a failed deploy never takes down the running app

The old container keeps serving until the new one is **ready AND routed**:

- pull/start/route failures → clean up only the *new* container
  (`cleanupKeeping(prevID)`), record `failed`, old route untouched.
- `SetLiveContainer(newID)` happens **before** retiring the old container.
- If you touch the state machine, run `deploy_test.go` first — the
  keeps-old-serving cases (`TestNotReadyKeepsOldContainerServing`,
  `TestRouteFailureKeepsOldContainerServing`) are the contract.

## Cleanup must survive a cancelled request

Failure recording and container cleanup run on `context.WithoutCancel(ctx)`
with a 30s budget. A user hitting Ctrl-C mid-deploy must not leave an
orphaned container or a deploy row stuck in `starting`.

## Per-app concurrency = pg advisory lock

`pg_try_advisory_lock(hashtext('qc-app:' || name))` on a **dedicated**
connection; the lock dies with the session, so a crashed deploy can't
wedge the app. Two deploys of the same app: second fails fast. Don't replace
with an in-process mutex — controld restarts would drop it while a container
operation is still in flight.

## Fresh Caddy volume ⇒ routes gone, DB still says live

Postgres (restored from R2) knows which apps should exist; a rebuilt Caddy has
no autosave, so no routes. `controld list` + redeploy each app is the
reconciliation (README "Rebuild from zero" step 9). A controld-side
auto-reconcile is flagged in `stack/controld/compose.yml` but deliberately not
built yet.

## Wiring facts that bite

- controld is on **data_net only** (talks to postgres + the caddy admin
  socket volume; never to app containers directly). Don't add app_net back.
- App containers get **no published ports** — Caddy dials
  `qc-<app>-<deployID>:<port>` over app_net.
- Docker module pin: classic `github.com/docker/docker@v28.x+incompatible`;
  `@latest` resolves to the relocated `github.com/moby/moby/client` and
  breaks the build.
