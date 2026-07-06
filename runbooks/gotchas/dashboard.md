# Dashboard gotchas (M5, templ + htmx)

Code: `controld/internal/dashboard/` — views in `views.templ`
(regenerate with `go tool templ generate`; the generated `views_templ.go`
is committed so the Docker build needs no templ toolchain).

## Auth is reachability; CSRF is the HX-Request header

No login by design — compose binds `:8600` to the Tailscale IP. But
"tailnet-only" does not stop CSRF: a drive-by page in the operator's browser
can form-POST to a tailnet IP. Every state-changing route therefore requires
the `HX-Request` header (`requireHtmx`) — custom headers force a CORS
preflight cross-origin, which the server never grants. Any new POST route
must go through the same gate.

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
