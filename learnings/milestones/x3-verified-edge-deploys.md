---
title: "Extra — deploy-edge.sh: A Self-Verifying Apply"
slug: x3-verified-edge-deploys
type: milestone
milestone: X3
status: stable
difficulty: 4
tags: [qincloud, infra, networking, reliability, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m1-edge-and-tls]]", "[[m4-controld-deploy-engine]]", "[[idempotent-self-verifying-operations]]", "[[verify-the-artifact-under-test]]", "[[fail-loud-at-boundaries]]", "[[single-source-of-truth]]", "[[root-cause-over-patch]]"]
sources: ["scripts/deploy-edge.sh", "runbooks/gotchas/caddy.md", "controld/internal/caddyapi/caddyapi.go"]
---

# Extra — deploy-edge.sh: A Self-Verifying Apply

> **In one sentence:** one idempotent command that applies an edge (Caddy) config change and *proves* it worked — validating the real on-disk file, healing a stale bind mount, reloading, restoring every app route the reload drops, and confirming each site actually responds before it exits non-zero.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Picture the front door of the whole platform. Every visitor to every app on the box — the dashboard, the analytics site, anything else — comes in through one doorman. That doorman is **Caddy**, the *edge*: it terminates HTTPS, checks who's asking, and hands each request to the right app inside. Change the doorman's instructions and you've changed how the entire building is reachable.

Now the catch. There are **two** copies of the doorman's instructions. One is a written script we keep in the repo (the `Caddyfile`). The other is a live, running memory the doorman builds up over time as our deploy engine ([[m4-controld-deploy-engine]]) walks up and says "also, send *this* hostname to *that* new app." When you reload the doorman from the written script, he throws away everything he memorized and starts from the script alone. Every app route added since first boot vanishes.

So "just edit the Caddyfile and reload" is a trap. Do it naively and you take the whole building offline — every app 404s until someone re-announces it. The naive path also has a nastier trap underneath it (§3) where the doorman is quietly reading an *old* copy of the script and nobody can tell. This note is about the one command that does the whole dance correctly and, crucially, refuses to declare success until it has walked to each door and knocked.

## 2. The plan (initial approach)

The mental model was simple and, on paper, right: an edge config change is a four-step ritual.

1. **Validate** the new Caddyfile so a typo can't take the edge down.
2. **Reload** Caddy so it picks up the change.
3. **Redeploy every app** through controld to re-add the routes the reload wiped ([[m4-controld-deploy-engine]]).
4. Done.

That order is documented in `runbooks/gotchas/caddy.md` under *"The autosave is the truth, not the Caddyfile."* Caddy runs with `--resume`, so its real source of truth after first boot is `/config/caddy/autosave.json` — the accumulated set of API-added routes. The Caddyfile is a **first-boot seed only**. Accept the wipe, then rebuild. Fine.

The plan's blind spot was the word "Done." Steps 1–3 are *actions*. None of them *observe the result*. And the two ugliest failures live exactly in that unobserved gap.

## 3. Where it deviated

Two surprises, one afternoon lost.

**Surprise one: a valid config that never reached Caddy.** We sync the repo to the box with `rsync`. Plain `rsync` doesn't edit a file in place — it writes a temp file next to the target and *renames* it over the top. On Linux that rename gives the host path a **brand-new inode**. But the Caddy container bind-mounted the *old* inode at container start, and it keeps holding it. Result: you edit the Caddyfile, `rsync` it up, `caddy validate` passes, `caddy reload` succeeds — and Caddy is serving the **pre-edit file the entire time**. No error. No warning. `validate` passes because it's validating the new host file while the container reads the stale one. This "silently cost hours once" — it's written up in `runbooks/gotchas/caddy.md` as the bind-mount trap.

**Surprise two: a reload that half-works.** Even with a fresh file, the reload wipes routes, and step 3 re-adds them by calling controld per app. But controld could fail on one app, or an app could come back up slow (umami runs DB migrations on boot and 502s for a bit), or a redeployed route could land in the wrong place. Any of these leaves the edge in a *plausible-looking* state — Caddy is healthy, most sites work — while one host is quietly dark. The script that "succeeds" and leaves a site down is worse than one that fails, because it stops you from looking.

Both failures share a shape: **the apply path could not prove it had worked.** That's where the hours went.

## 4. The fix — and how I found it

The fix was to make proof a required step, not an optional afterthought — and to make the whole thing safe to run again and again ([[idempotent-self-verifying-operations]]). `scripts/deploy-edge.sh` is that command. It does the ritual *and* closes both gaps:

- **Validate the true on-disk file, mount-independently.** It pipes the host Caddyfile over stdin into the validator, so it checks the real bytes regardless of what the container's mount holds (`deploy-edge.sh:56`).
- **Heal the stale mount by checksum.** Before reloading, it compares `sha256sum` of the host file against the file *inside* the container. If they differ, the mount is stale — it force-recreates the container to re-establish it (`deploy-edge.sh:69-76`). This directly kills Surprise one, no matter how the repo was synced.
- **Restore every route, then knock on every door.** After the reload it redeploys each app controld knows about, then loops over every host and actually curls it, waiting up to 90s for each to come up, and **exits non-zero if any host never responds** (`deploy-edge.sh:100-131`). This kills Surprise two.

The insight wasn't a clever trick; it was refusing to trust an unobserved action ([[verify-the-artifact-under-test]]). The root cause of the wasted afternoon wasn't the rename semantics of `rsync` — that's just Unix. It was that nothing in the pipeline ever asked *"is Caddy serving the bytes I think it is?"* ([[root-cause-over-patch]]). Once that question is a step, the class of bug can't recur.

## 5. Going deep (systems level)

The script is `set -Eeuo pipefail` with an `ERR` trap that reports the failing line and command (`deploy-edge.sh:21-26`), so any unhandled failure dies loud with context ([[fail-loud-at-boundaries]]).

**Run it:**
```bash
rsync -a --inplace <repo>/ root@<box>:/opt/qincloud/     # --inplace: belt to the checksum's suspenders
ssh root@<box> 'bash /opt/qincloud/scripts/deploy-edge.sh'
ssh root@<box> 'bash /opt/qincloud/scripts/deploy-edge.sh --dry-run'   # validate only, mutate nothing
```

**Preflight** (`:44-50`) fails fast: must be root, needs `docker`/`curl`/`sha256sum`, the Caddyfile must exist at `/opt/qincloud/stack/edge/Caddyfile`, and both `edge-caddy-1` and `qincloud-controld` must be running.

**1 — Validate, mount-independent** (`:56`):
```bash
compose exec -T caddy caddy validate --config /dev/stdin --adapter caddyfile < "$CADDYFILE"
```
Piping over stdin is the whole point — it validates the host's true bytes, not the container's possibly-stale mount. `--dry-run` exits here.

**2 — Freshness guard** (`:69-76`):
```bash
host_sum=$(sha256sum "$CADDYFILE" | awk '{print $1}')
cont_sum=$(docker exec "$CADDY" sha256sum /etc/caddy/Caddyfile | awk '{print $1}') || cont_sum=""
[[ "$host_sum" != "$cont_sum" ]] && compose up -d --force-recreate   # + sleep 2 to settle the admin socket
```
Checksum mismatch ⇒ recreate to re-bind the current inode (this also picks up any `compose.yml` change). To diagnose by hand: `docker exec edge-caddy-1 sha256sum /etc/caddy/Caddyfile` vs the host file.

**3 — Reload** (`:80`): `caddy reload --config /etc/caddy/Caddyfile`. On failure the previous config keeps serving — a failed reload is not an outage.

**4 — Restore routes** (`:84-97`): parse `controld list`, and for each app `controld redeploy -app "$app"`. Each redeploy re-upserts the app's route via Caddy's admin API. Order matters at the API level: controld does `PATCH /id/qc-<app>` in place, falling back to `PUT .../routes/0` on 404 so the route lands at **index 0**, in front of the seed catch-all (`controld/internal/caddyapi/caddyapi.go`; runbook: *"App routes must land at index 0"*). A route appended behind the catch-all never matches — another silent failure this design forecloses.

**5 — Verify** (`:100-131`): Caddy health must be `healthy` or `starting`, then `verify_host` polls `https://$host/` for up to `VERIFY_TIMEOUT=90`s:
```bash
code=$(curl -sS -o /dev/null -w '%{http_code}' -m 10 "https://$host/" || echo 000)
case "$code" in 000|5??) keep waiting ;; *) routed & responding ;; esac
```
The success set is deliberate: `2xx/3xx/401/403/404` all mean "routed and answering" — `401` is the dashboard's own auth challenge ([[m5-dashboard]]), a healthy signal. Only `000` (no connection / bad gateway) and `5xx` count as broken-or-still-booting. Any host that never clears the timeout sets `failed=1`, and the script dies: *"the edge is in a bad state, investigate now."* One skipped subtlety it accounts for: a managed domain returns `308` on plain `http://` because Caddy auto-injects an HTTP→HTTPS redirect — so verification goes over `https` to avoid mistaking correct behavior for a bug (runbook: *"Auto-HTTPS injects invisible redirects"*).

## 6. How this compares to best practice

A mature platform separates these concerns into named machinery: a config store with atomic swaps (Consul/etcd), a control loop that reconciles desired vs actual, and a **post-deploy health gate** in the pipeline that rolls back on failure. Kubernetes readiness probes are exactly the §5 step 5 idea — don't send traffic, and don't call a rollout done, until the thing answers.

QinCloud collapses all of that into one Bash script, which is the honest right-sized choice for one box ([[the-box-is-disposable]]). Where we match the standard: **validate-before-apply**, **fail-closed** (a bad reload leaves the old config serving), and **verify-after-apply with a non-zero exit**. Where we cut a corner: there's no *automatic rollback* — on failure the script stops and yells rather than reverting. On a single box with a human at the other end of the SSH session that's the right tradeoff; the moment this runs unattended in CI ([[m4-controld-deploy-engine]]), the verify step needs to trigger a revert, not just an alarm. The split-source-of-truth between Caddyfile-seed and autosave-runtime is a genuine wart we manage rather than eliminate ([[single-source-of-truth]]) — the script's route-restore step is the tax we pay for it every apply.

## 7. The underlying why (the transferable lesson)

**An apply path that cannot prove it worked is where hours go to die.** Every failure in this story — the stale mount, the half-restored routes, the misplaced route behind the catch-all — was invisible precisely because the pipeline performed an action and never observed its effect. Validation is not verification: `caddy validate` passing told us the *file* was well-formed, not that Caddy was *serving* it. The gap between "I ran the command" and "the system is in the state I intended" is exactly where silent failure lives, and it's wide enough to lose an afternoon in.

The discipline that closes it is cheap and universal: after any mutation, **read the system back through the same door your users use** and refuse to exit success until it answers. Curl the real host over the real protocol; checksum the bytes the process is actually reading; make the proof a required step with teeth (a non-zero exit), not a comment that says "should work now." That turns a whole class of ghost failures into a loud, immediate, single-command red — which is the only kind of failure you can actually fix.

---
**Teaches:** [[idempotent-self-verifying-operations]] · [[verify-the-artifact-under-test]] · [[fail-loud-at-boundaries]] · [[single-source-of-truth]] · [[root-cause-over-patch]]
**Sources:** `scripts/deploy-edge.sh`, `runbooks/gotchas/caddy.md`, `controld/internal/caddyapi/caddyapi.go`
