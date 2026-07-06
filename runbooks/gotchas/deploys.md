# controld deploy gotchas & invariants

Code: `controld/internal/deploy/deploy.go` (state machine),
`deploy_test.go` (the regression net — these invariants each have a test).

## The prime invariant: a failed deploy never takes down the running app

The old container keeps serving until the new one is **ready AND routed**:

- pull/start/route failures retire **exactly the container this deploy
  created, by name** (`removeNew`), record `failed`, old route untouched.
- `SetLiveContainer(newID)` happens **before** retiring the old container.
- If you touch the state machine, run `deploy_test.go` first — the
  keeps-old-serving cases (`TestNotReadyKeepsOldContainerServing`,
  `TestRouteFailureKeepsOldContainerServing`,
  `TestFailedDeployAfterStaleLiveRecordKeepsRoutedContainer`) are the contract.

## Never sweep on a remembered ID — the record can be stale

Found by the M5 adversarial review, live in M4 for a while: failure cleanup
used to remove "everything except the recorded live container". But
`SetLiveContainer` can fail *after* a route switch, leaving `container_id`
stale — the next failed deploy's sweep would then keep the stale old
container and **remove the routed one**: hard downtime from a failed deploy.
Rule: a failure path may only remove what it itself created (deterministic
name `qc-<app>-<deployID>`); only the happy path sweeps, keyed on its own
fresh container. Class: any "clean up everything except X" where X is read
from state that can lag reality.

## Reads that feed an action must happen under the action's lock

Redeploy = read spec → deploy it. Reading outside the app lock is a TOCTOU:
a destroy can win the lock between read and deploy, and the stale spec then
*resurrects the destroyed app* — an outcome no serial order produces. Hence
`Deployer.Redeploy(name)` reads the spec inside the lock; callers (dashboard,
future CLI) pass only the name. Never GetApp-then-Deploy around it.

## Never re-type env on a redeploy — use `controld redeploy`

Restoring routes (after a caddy reload or rebuild) by re-running `deploy`
with hand-typed `-env` flags is how a stored secret gets clobbered — it
happened live (umami's `APP_SECRET`, 2026-07-06). `controld redeploy -app X`
re-runs the STORED spec, env included, under the app lock. `deploy` with
full flags is only for genuinely new specs.

## Everything after the route switch runs detached

Once the new container is routed, the deploy has succeeded in the world —
recording the live container, retiring the old one, and stamping `live` all
run on `context.WithoutCancel` (30s budget), like failure recording always
did. The deploy context dying right after the route switch (budget expiry,
Ctrl-C) must not fail the deploy or leave the record lying.
(`TestPostRouteBookkeepingSurvivesDeadDeployContext`)

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
