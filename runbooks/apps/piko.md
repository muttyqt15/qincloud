# App: Piko (evidence-based AI assistant for Pikira)

- **URL:** https://piko.sparboard.com · image `ghcr.io/muttyqt15/piko:<tag>` · port 8422
- **What:** the Piko cockpit (React SPA + SSE API) **and** its Discord bot, in ONE
  container — controld has no no-route/worker deploys, so both processes ride
  together and the entrypoint dies if either dies (AppDown then pages).
- **Source/deploy script:** the private piko repo (`scripts/deploy-piko.sh` there —
  modeled on `deploy-notes.sh`). The IMAGE is public on ghcr (controld pulls
  unauthenticated); it contains piko source but zero secrets.
- **Auth:** no Caddy basic_auth — every `/api/*` route needs `Authorization:
  Bearer $PIKO_API_TOKEN` (401 otherwise, docs endpoints disabled). The SPA at
  `/` is public static files; it prompts for the token and keeps it in
  localStorage. Umami (website `piko`) tracks the SPA.

## How it's wired

- `-db` → `tenant_db_net`; database provisioned 2026-07-07 via
  `controld provision -app piko -postgres` (role `piko`, database `piko`).
- **DSN gotcha:** piko is SQLAlchemy+asyncpg — the provisioned URL must be
  rewritten `postgresql://` → `postgresql+asyncpg://` and `?sslmode=disable`
  DROPPED (not a valid asyncpg param; in-network plaintext is its default).
- Env lives in the app spec (`apps.env`), passed by `deploy-piko.sh` from the
  piko repo's gitignored `.env.qincloud` — the script refuses to run unless
  EVERY key is present (deploy replaces env wholesale). Keys: PIKO_DB_DSN,
  PIKO_LOOP_MODEL, PIKIRA_GITHUB_REPO, OPENROUTER_API_KEY,
  CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID, GITHUB_TOKEN, PIKO_API_TOKEN,
  DISCORD_TOKEN.
- **GITHUB_TOKEN is currently the broad muttyqt15 oauth token** (repo write) —
  an accepted 2026-07-07 stopgap because the fine-grained PAT had no
  pikira-org/pikira grant. Rotate: mint a pikira-org fine-grained PAT
  (Contents: Read-only), update `.env.qincloud`, rerun `deploy-piko.sh`.

## Deploy / roll / rollback

```sh
# from the piko repo root, on the laptop:
./scripts/deploy-piko.sh              # build amd64 → push ghcr → controld deploy → verify
./scripts/deploy-piko.sh --tag v1     # pin a tag; also how you roll BACK to a good tag
```

The script self-verifies: controld recorded the exact tag, `GET /` is 200,
and a tokenless `POST /api/sessions` is 401 (proves the API answered, not
just static files).

## Known behavior

- **First boot does real work**: shallow clone of the Pikira repo (GitHub
  egress), `alembic upgrade head`, then a live LLM preflight call (OpenRouter
  egress). The image HAS a HEALTHCHECK (GET / every 10s, start-period 30s),
  so controld gates the route on genuine readiness — but a boot needs both
  GitHub and OpenRouter reachable or the container crash-loops (fail-loud by
  design; check `docker logs qc-piko-<id>`).
- The Pikira checkout lives at `/app/.piko/pikira` INSIDE the container
  (ephemeral, re-cloned each boot) — nothing to back up.
- Bot and API share the container; the bot talks to the API over
  `127.0.0.1:8422`. If Discord answers stop but the site serves, check logs —
  by design that state shouldn't persist (either process dying kills the
  container).
- Memory: two Python processes idle ~60 MB total, well inside the 512 MB
  fence; the per-app Grafana gauges watch it.

## DR

Nothing app-specific beyond the standard pattern: the `piko` database rides
the nightly pg_dump → R2; the app spec (env included) restores with the
controld database; after a box rebuild, `controld redeploy -app piko`
recreates container + route from the restored spec. The umami admin password
was rotated 2026-07-07 (was still the default!) — convenience copy at
`/root/.umami-admin-pw` (0600), same pattern as `.umami-db-pw`.
