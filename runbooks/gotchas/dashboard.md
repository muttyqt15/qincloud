# Dashboard gotchas (M5, templ + htmx)

Code: `controld/internal/dashboard/` — views in `views.templ`
(regenerate with `go tool templ generate`; the generated `views_templ.go`
is committed so the Docker build needs no templ toolchain).

## Two doors: tailnet raw, public behind edge basic auth

`TS_IP:8600` is the unauthenticated tailnet path. https://dash.sparboard.com
is the public path (deliberate operator decision 2026-07-06): Caddy
`basic_auth` (user `admin`, bcrypt hash `DASH_PASSWORD_HASH` in `.env`,
`$$`-escaped — see host.md) proxying over `admin_net`, a bridge carrying
only caddy + controld + prometheus. The dashboard drives docker.sock — any
new route mounted on its mux is automatically behind the edge auth on the
public path, but NEVER weaken the site-block-wide `basic_auth` to
per-path.

### The hash: cost 10, adapt-time, rotation-needs-reload

- **Cost 10, not Caddy's default 14.** The public door lets an
  unauthenticated client force a bcrypt hash per request — cost 14 (~1.4s
  CPU) is a DoS lever on the single shared edge. Cost 10 (~60ms) cuts the
  amplification ~16x and is still uncrackable for a `openssl rand` password.
  Caddy's `hash-password` is fixed at 14, so generate with
  `htpasswd -nbB -C 10`. Real rate-limiting is Cloudflare's job (see below).
- **Adapt-time `{$DASH_PASSWORD_HASH}`, not runtime `{env.…}`.** basic_auth
  base64-wraps its account password; a runtime `{env.}` placeholder lands
  raw where base64 is expected and Caddy crash-loops on provision
  (`base64-decoding password: illegal base64 data`). Adapt-time resolves +
  wraps correctly. Cost: the hash is baked into `autosave.json`, so
  **rotating the password needs a `caddy reload` + redeploy dance**, not a
  container recreate — a plain recreate keeps serving the old hash.

### Residual: public door has no rate limit (Cloudflare pending)

Even at cost 10, an unauthenticated flood still amplifies (~60ms CPU/req)
and the origin IP is exposed while DNS is grey-cloud. The real fix is
fronting dash.sparboard.com with Cloudflare (orange-cloud: WAF + rate limit
+ hidden origin), then locking Caddy's dash site to Cloudflare's IP ranges.
Needs a Cloudflare API token / dashboard access — tracked, not yet done.

## CSRF is the HX-Request header — under basic auth too

A drive-by page in the operator's browser can form-POST to either door (the
browser auto-attaches basic-auth credentials it has cached). Every
state-changing route therefore requires the `HX-Request` header
(`requireHtmx`) — custom headers force a CORS preflight cross-origin, which
the server never grants. Any new POST route must go through the same gate.

## Stats block ~1s; logs are capped

`Runtime.Stats` double-samples via the daemon (that's where the CPU rate
comes from) — poll fragments at ≥5s, and `/metrics` samples apps serially
(fine for a handful; parallelize past that). Log responses are capped at
256KB by `limitedWriter` so a flooding app can't balloon a request; content
is app-controlled text and must always render templ-escaped (regression
test: `TestLogsAreEscaped`).

## One status projection: the deploys table

The UI never keeps deploy state of its own — POST hands off to a background
goroutine and the app list polls the `deploys` table (the same rows the CLI
writes). A dashboard restart mid-deploy loses nothing. Don't add an
in-memory "current deploys" map; that is the drift bug the projection exists
to prevent. Known accepted edge: the 1s fast-fail window is a heuristic — a
Postgres stall can push pre-flight past it, and that failure then lands only
in `docker logs qincloud-controld` (during which the whole dashboard is
visibly degraded anyway).

## Polling swaps must not fight in-flight actions

An unconditional `every 3s` innerHTML swap replaces buttons mid-request,
erasing the `.htmx-request` in-flight guard (double-click protection). The
`#apps` poll is gated: `every 3s [document.querySelectorAll('.htmx-request').length === 0]`.
Keep action buttons inside polled regions only if the poll has such a guard.

## Stop dead polls with HTTP 286

htmx cancels an `every Ns` poll when a response arrives with status 286
(after swapping it in). The detail page's history poll uses it when the app
was destroyed elsewhere — a plain 200 with an empty table would masquerade
as valid state forever. Any polled fragment whose subject can disappear
needs the same treatment.

## Stale non-terminal rows render as "abandoned"

A controld restart mid-deploy leaves the deploys row non-terminal forever.
The UI treats non-terminal + older than 10 minutes as "abandoned" instead of
polling "starting…" indefinitely. If the deploy budget (5min) ever changes,
keep `staleAfter` comfortably above it.
