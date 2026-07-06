# Caddy gotchas

## The autosave is the truth, not the Caddyfile

Caddy runs with `--resume` (`stack/edge/compose.yml`): after first boot it
loads `/config/caddy/autosave.json`, which contains every route controld added
via the admin API. The Caddyfile is a **first-boot seed only**.

- **Never** `caddy reload --config Caddyfile` on a running box — it replaces
  the config wholesale and silently drops all API-added app routes.
- Recovery if it happens (or after a fresh `caddy_config` volume):
  `controld deploy` every app; each deploy re-upserts its route.
- Changing the Caddyfile legitimately (new seed behavior) = accept the route
  wipe: edit → `rm autosave.json` → recreate container → redeploy all apps.

## App routes must land at index 0

The adapted config has catch-all routes (the seed site block). A route
*appended* to the routes array sits behind them and never matches. controld's
upsert is therefore: `PATCH /id/qc-<app>` (in-place replace, atomic), and only
on 404 `PUT .../routes/0` (insert at the front). See
`controld/internal/caddyapi/caddyapi.go`.

## Auto-HTTPS injects invisible redirects

When a host-matched route lands on the :443 server, Caddy auto-provisions the
cert **and** injects an HTTP→HTTPS redirect for that host on :80. The redirect
does not appear in `GET /config/` — so `curl http://host/...` returning `308`
is correct managed-domain behavior, not a routing bug. Health-check over
`127.0.0.1` or https instead.

## Split server topology after TLS

With a domain site block, the adapter creates srv structure like: one server
on `:443` (host-matched routes) + one on `:80` (redirects/health). App routes
must target the :443 server. `pickPublicServer` prefers the single :443
server and errors on ambiguity — don't "fix" that error by picking one
arbitrarily; it means the topology changed and the selection logic needs a
fresh look against real `caddy adapt` output (fixtures in
`caddyapi_test.go` come from real adapter output; keep it that way).

## Per-hostname certs self-heal

A cert for `x.sparboard.com` is requested when its route lands. If DNS isn't
ready yet, issuance fails and Caddy retries on its own — no action needed
beyond fixing DNS. Don't restart Caddy to "kick" it.

## Admin API is a unix socket in a shared volume

`/run/caddy/admin.sock` in the named volume `caddy_admin`, mounted by both
edge and controld. stack/edge must be up **before** stack/controld on a fresh
box (it creates the volume).
