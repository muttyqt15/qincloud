# Gotchas — the "don't repeat this" reference

Durable operational knowledge, one file per domain. Drill records under
`../drills/` are the immutable narrative of *what happened*; these files are
the living rules of *what to do about it*, updated as the platform evolves.

| File | Domain |
| --- | --- |
| [caddy.md](caddy.md) | Edge routing, admin API, auto-HTTPS |
| [alerting.md](alerting.md) | Prometheus → Alertmanager → Discord pipeline |
| [backups-r2.md](backups-r2.md) | rclone, R2 credentials, restore rules |
| [data-services.md](data-services.md) | Shared Postgres/Redis tenancy, `controld provision`, ACLs |
| [deploys.md](deploys.md) | controld state machine invariants |
| [dashboard.md](dashboard.md) | M5 web dashboard: auth model, polling, htmx |
| [host.md](host.md) | Ubuntu 24.04 baseline, UFW vs Docker |
| [dev-process.md](dev-process.md) | Building controld with parallel agents |

## Rules of the folder

1. **Update in the same commit as the fix.** A gotcha discovered but not
   written down here will be rediscovered the hard way.
2. **Entry shape:** symptom → root cause → the rule going forward, plus a
   pointer to where the fix lives (script, compose comment, drill record).
3. **These files are the source of truth for the rule; drills are evidence.**
   If a rule changes, edit it here — never rewrite a drill record.
