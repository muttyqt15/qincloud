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

### Cloudflare fronts the door (done 2026-07-06)

dash.sparboard.com is orange-clouded (explicit proxied A record → origin IP,
overriding the grey-cloud wildcard): Cloudflare's WAF/DDoS mitigation is in
front and the origin IP is hidden from DNS. Zone SSL mode is **Full**, so
Caddy serves a self-signed cert (`tls internal`) on the dash site and needs
no ACME (HTTP-01 can't pass the proxy). Caddy then locks the dash site to
**Cloudflare's IP ranges** so the still-public origin IP can't be hit
directly: `@cf remote_ip <CF ranges>` → `handle @cf { basic_auth; reverse_proxy }`,
else `handle { respond 404 }`. Verified: direct-to-origin-IP hits get 404,
only Cloudflare traffic reaches the bcrypt door.

Three silent traps cost hours here (all: config looks applied, gate just
isn't there):

1. **rsync breaks a bind-mounted single file.** `rsync` writes a temp file
   then renames it, so the host path gets a NEW inode while the container
   keeps the OLD one — the container serves a STALE Caddyfile and every
   `caddy adapt`/`reload` reads stale. Use `rsync --inplace` for bind-mounted
   files, or `--force-recreate` the container to re-establish the mount. This
   was the real cause; the two below are real but were red herrings under it.
2. **Two `remote_ip` matchers in one Caddyfile collide** in Caddy 2.10's
   adapter — one silently drops from the compiled config (`validate` still
   passes). Keep the dash `@cf` gate as the ONLY remote_ip matcher; the
   :2019 metrics listener stays plain (see its RESIDUAL note in the Caddyfile).
3. **Backticks / `{$…}` / `{env.}` in a Caddyfile comment** make the lexer
   swallow following directives. Keep in-block comments plain ASCII.

Verify a Caddyfile change actually landed: `docker exec edge-caddy-1 grep <marker>
/etc/caddy/Caddyfile` before trusting `caddy adapt` output.

Rate-limiting is Cloudflare's now; an explicit CF rate-limit rule needs a
token with Firewall-edit perms (optional — the origin lock already makes the
bcrypt door unreachable to a direct flood).

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
