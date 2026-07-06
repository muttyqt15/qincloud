# App: Umami analytics (M6 — first real app)

- **URL:** https://analytics.sparboard.com · image `ghcr.io/umami-software/umami:postgresql-v2.19.0` · port 3000
- **First login:** `admin` / `umami` — CHANGE THE PASSWORD immediately, then create a website entry to get the tracking `<script>` for any site you want measured.
- Deployed 2026-07-06 via `controld deploy -app umami … -db -env DATABASE_URL=… -env APP_SECRET=…`.

## How it's wired

- `-db` puts the container on `tenant_db_net` (postgres reachable; redis,
  exporters, controld are not — verified from inside the container).
- `DATABASE_URL` / `APP_SECRET` live in the app's spec (`apps.env` in the
  controld database) — they survive redeploys, back up to R2 nightly with
  everything else, and the dashboard renders env KEYS only.
- A convenience copy of the umami DB password sits in `/root/.umami-db-pw`
  (0600). The spec is the source of truth; this file just saves a trip to
  `psql` when rotating.

## Provisioning pattern (for the next DB-backed app)

```sh
UPW=$(openssl rand -hex 24)
printf "CREATE ROLE <app> LOGIN PASSWORD :'pw';\nCREATE DATABASE <app> OWNER <app>;\n" \
  | docker exec -i qincloud-postgres psql -U qincloud -d postgres -v ON_ERROR_STOP=1 -v pw="$UPW"
```

psql gotcha: `-c` commands do NOT interpolate `-v` variables — pipe SQL via
stdin (as above and in `stack/data/initdb/01-controld.sh`).

## DR

Nothing app-specific: the nightly backup dumps every database (umami
included), globals carry the umami role + password, and the app spec (env,
`use_db`) restores with the controld database. After a box rebuild it comes
back with `controld deploy`/dashboard redeploy like every other app.

## Known behavior

- First boot runs Prisma migrations; the route lands before they finish
  (grace-based readiness — the image has no HEALTHCHECK), so the first
  ~30s can 502. Subsequent deploys reuse the migrated schema and are quick.
