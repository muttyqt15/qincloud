# Drill: M8 full box rebuild — wipe → bootstrap + git + R2 → serving (2026-07-06)

**Goal:** prove invariant #4 ("the box is disposable") end to end on the real
box: destroy everything Docker + `/opt/qincloud`, rebuild only from
`bootstrap.sh` + the repo + R2 backups + operator secrets, measure real
RTO/RPO.

## Numbers

| Metric | Value |
| --- | --- |
| RTO (teardown start → app serving + 10/10 targets) | **≈ 12 min** |
| RPO (fresh backup taken pre-drill) | **≈ 90 s** (nightly schedule ⇒ ≤ 24 h in a real loss) |
| What survived the wipe | OS packages, tailscale auth, `/root` vault (stand-in for the password manager) |
| What was destroyed | all 14 containers, all 10 volumes (incl. pgdata + caddy certs), both bridges, all images (2.96 GB), `/opt/qincloud`, systemd backup units |

## Timeline (UTC)

| T | Event |
| --- | --- |
| 13:21:51 | pre-drill backup uploaded + verified (all 5 prefixes) |
| 13:22:54 | **T0** — teardown begins |
| ~13:24 | box scorched: 0 containers, 0 volumes, site DOWN |
| ~13:26 | bootstrap.sh re-run clean (idempotent ✅); repo rsynced; `.env` + webhook secret restored from vault |
| ~13:28 | edge + data up; fresh initdb created the dedicated `controld` role; LE certs re-issued on their own |
| ~13:30 | R2 restore: globals + `controld` + `qincloud` databases; `apps` table shows whoami, 5 deploys of history |
| ~13:32 | observability up (9 services), backup timer re-enabled |
| ~13:34 | controld image rebuilt; **whoami redeployed through the M5 dashboard**: `starting…` → `live`; site 200 over fresh TLS |
| 13:35:04 | rebuilt box completed its own backup to R2; BackupStale metric re-armed in Prometheus |

## Found during the drill

1. **Repo ahead of box turns DR into a migration.** The repo's data stack had
   evolved (dedicated `controld` role, `CONTROLD_DB_PASSWORD`, initdb shell
   script) while the box still ran the older superuser-DSN layout. The
   rebuild had to bridge it: generate the new credential, rewrite
   `CONTROLD_DSN`, and restore dumps owned by the old role into the new one.
   Rule: when a stack's compose/env contract changes, reconcile the running
   box in the same sitting — the rebuild path always follows the repo.
2. **`compose down` fails loud on missing `${VAR:?}` too.** The fail-loud
   interpolation guard blocks teardown, not just startup — the data stack
   would not even `down` without the new variable. Workaround: dummy-set the
   var or `docker rm -f` directly.
3. **README step 6 said `pg_restore --create` — wrong against a fresh
   initdb**, which already creates the `controld`/`qincloud` databases. The
   correct shape (now in the README):
   `pg_restore --clean --if-exists --no-owner --role=<owning-role> -d <db>` —
   `--no-owner --role` also performs the old-owner → dedicated-role migration
   from finding 1 in the same pass.
4. **docker.io from this VPS is flaky** (TLS handshake timeouts) — with all
   images pruned, pulls needed retries. Pull retry loops are now part of the
   procedure; consider a registry mirror if it worsens.
5. **While the box is down, nobody pages.** Alertmanager lives on the box —
   a full-box outage is invisible by construction. Future hardening: an
   external uptime check (healthchecks.io / UptimeRobot) on the public site
   and on backup freshness.
6. **The restored control plane drove the recovery.** `controld list` read
   the restored DB and knew exactly what should exist; one dashboard
   redeploy per app recreated container + route + cert. Deploy-ID sequences
   restored correctly (next deploy = 6 after 5 restored rows) — no
   container-name collisions because teardown removed all `qc-*` containers,
   exactly as the initdb DR note prescribes.
