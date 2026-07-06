# Failure-injection catalogue

The drills we run *on purpose*, periodically, to prove the monitoring and
recovery paths actually work — because the failure mode of observability is
uniquely nasty: a broken monitor looks identical to a healthy one (green, quiet,
armed) right up until the outage it was supposed to catch sails past unseen. See
[`../../learnings/concepts/observe-what-matters.md`](../../learnings/concepts/observe-what-matters.md):
*done is "I made the bad thing happen and watched the right signal arrive."*

Each drill below is grounded in a metric **verified present in Prometheus on the
box**, and an alert **that actually exists** in
[`../../stack/observability/prometheus/rules/qincloud.yml`](../../stack/observability/prometheus/rules/qincloud.yml).
Where a failure is currently **silent to the pager**, that is stated as a **Gap**
and carried as an action item — an honest catalogue is worth more than a
flattering one.

## Before you inject anything — read this first

- **Respect the pipeline timing** ([`../gotchas/alerting.md`](../gotchas/alerting.md)).
  Detection→page for an `up==0` alert is ≈ **4m40s**: 15s scrape + `for: 3m` +
  rule eval + `group_wait: 30s`. **Hold the failure ≥ 60s past *firing*** or the
  alert resolves before `group_wait` flushes and **no page is sent** — a "passing"
  drill that proved nothing. The resolved notification flushes within
  `group_interval: 5m`.
- **These commands page for real.** On prod, either pre-announce in the channel
  or set an Alertmanager silence (`amtool silence add …`) so you don't wake people
  — but confirm the alert reached *pending/firing* before silencing, or you've
  tested nothing.
- **The box is the record, not the laptop.** Read evidence off the box (container
  uptimes, Prometheus state, Alertmanager logs). Local ssh polls can hang
  mid-observation.
- **Verify queries** run on the box against Tailscale-only Prometheus:
  ```sh
  ssh -i ~/.ssh/qin-vps root@100.125.12.20 \
    'set -a; . /opt/qincloud/.env; set +a; curl -s "http://$TS_IP:9090/api/v1/query?query=<EXPR>"'
  ```
- **Every drill produces a record** in this folder and, if it surfaces a rule,
  a [`../gotchas/`](../gotchas/) update + a [postmortem](../postmortem-template.md)
  in the same commit.

## Safe-on-prod vs maintenance-window — at a glance

| Drill | What it kills | Pages today? | Prod safety |
| --- | --- | --- | --- |
| A · App crash-loop | a throwaway container | ✅ `ContainerRestartLoop` | **Safe** (touches nothing real) — but *pages*: pre-announce/silence |
| B · Exporter / target down | one scrape target | ✅ `InstanceDown` | **Safe** (blinds one metric source, serving unaffected) — *pages* |
| C · Backup stall | the freshness metric only | ✅ `BackupStale` | **Low-risk window** (reversible; masks a real backup failure while set) |
| D · DB outage | `qincloud-postgres` / `qincloud-redis` | ⚠️ partial (see drill) | **Maintenance window** (real outage of controld + tenant apps) |
| E · Full-box loss | everything | ❌ **nothing pages** by construction | **Maintenance window** (it *is* destroying prod; fresh backup first) |

---

## Drill A — App container crash (crash loop)

**Simulates:** a tenant app that boots, dies, and gets restarted repeatedly by
Docker (bad image, missing env, panic-on-start).

**Inject** (a self-crashing *throwaway* container — never a real `qc-*` app):
```sh
# on the box
docker run -d --name qc-drillcrash --restart=on-failure:20 --network app_net \
  alpine sh -c 'exit 1'
```
Docker restarts it on each immediate exit; cAdvisor sees `container_start_time_seconds`
jump on every restart.

**Should happen:** `changes(container_start_time_seconds{name!=""}[15m]) > 2`
crosses threshold → **`ContainerRestartLoop`** (severity critical, `for: 0m`) →
Discord page.

**Gotcha to verify, not fix:** the page's `{{ $labels.name }}` is a **64-hex
container ID, not `qc-drillcrash`** — under Docker's containerd image store
cAdvisor only knows the raw ID (see the note in
[`../../stack/observability/compose.yml`](../../stack/observability/compose.yml)).
Confirm you can map the ID back to the container: `docker ps --no-trunc | grep <id>`,
and that the app's first-class identity is legible via
`qincloud_app_up{app="…"}` on the dashboard.

**Recovery / RTO:** `docker rm -f qc-drillcrash`. This is a drill container, so
there is no service RTO — what you are measuring is *detection + page*, and that
the restart-loop rule fires before an operator would notice by eye.

**Verify:**
```sh
# the restart churn (should exceed 2 within the window)
curl -s "http://$TS_IP:9090/api/v1/query?query=changes(container_start_time_seconds%7Bname!=%22%22%7D%5B15m%5D)"
# alert state
curl -s "http://$TS_IP:9090/api/v1/alerts" | grep -o ContainerRestartLoop
```
then watch Discord for the firing + resolved messages.

**Prod safety:** **safe** — the throwaway container touches no real app, data, or
route. But it fires a real critical page; pre-announce or silence.

> **Gap (today):** a *clean single stop* of a real app that **stays** stopped
> (`docker stop qc-whoami`) does **not** crash-loop and does **not** page.
> `qincloud_app_up{app="whoami"}` drops to 0 on the dashboard, but **no rule
> watches it**. → Action item: add an alert on `qincloud_app_up == 0` (the metric
> already exists, published by controld) so a stopped app pages, not just a
> looping one.

---

## Drill B — Exporter / scrape target down (generalized M3)

**Simulates:** any of the 11 scrape targets going dark — an exporter crash, a
metrics endpoint hanging, a network partition to a target. This is the M3 pager
drill ([`2026-07-06-m3-pager-drill.md`](2026-07-06-m3-pager-drill.md)) generalized
to every target.

**Targets (all verified `up=1` on the box):** `alertmanager`, `alloy`, `caddy`,
`cadvisor`, `controld`, `grafana`, `loki`, `node`, `postgres` (exporter),
`prometheus`, `redis` (exporter).

**Inject** (pick a target; the M3 canonical is redis-exporter):
```sh
# on the box — container name pattern is <compose-project>-<service>-1
docker stop observability-redis-exporter-1
```

**Should happen:** `up{job="redis"} == 0` → **`InstanceDown`** pending at `for: 3m`
→ firing → Discord page ≈ **4m40s** after T0. On `docker start …`, the resolved
notification flushes within `group_interval: 5m`. **Hold the target down ≥ 60s
past *firing*** or the page never flushes.

**Recovery / RTO:** `docker start observability-redis-exporter-1`; the exporter is
stateless, so it is back in seconds. The number that matters here is
detection-to-page latency, not service RTO.

**Verify:**
```sh
curl -s "http://$TS_IP:9090/api/v1/query?query=up%7Bjob%3D%22redis%22%7D"   # expect 0 while stopped
curl -s "http://$TS_IP:9090/api/v1/alerts" | grep -o InstanceDown
```

**Prod safety:** **safe** — stopping an *exporter* blinds one metric source but
does not affect serving. (Stopping cadvisor/node-exporter briefly loses
container/host metrics; still safe.) Pages fire — pre-announce/silence.
**Do not stop `prometheus` itself** as a "target down" drill — that blinds the
whole pipeline, and Prometheus cannot detect its own death; that failure belongs
to Drill E.

---

## Drill C — Backup pipeline stall (`BackupStale`)

**Simulates:** the nightly backup silently stopping — the timer wedged, rclone
failing every night, R2 creds rotated out. The exact class that shipped **inert**
in M3 because its metric didn't exist yet
([`../gotchas/alerting.md`](../gotchas/alerting.md): "an alert on a metric that
doesn't exist is a silent no-op"). This drill is the standing proof that the
signal is now real.

**How the metric works:** `scripts/backup.sh` writes
`qincloud_backup_last_success_timestamp_seconds` into the node-exporter textfile
file `/opt/qincloud/metrics/qincloud_backup.prom` on every success. The alert is
`time() - qincloud_backup_last_success_timestamp_seconds > 129600` (36h).

**Inject** (write a *stale* timestamp — reversible, touches no backup data; use
tmp+`mv` because the collector may read mid-write):
```sh
# on the box — back-date the freshness metric to 37h ago
f=/opt/qincloud/metrics/qincloud_backup.prom
printf 'qincloud_backup_last_success_timestamp_seconds %s\n' "$(( $(date +%s) - 133200 ))" > "$f.tmp"
mv "$f.tmp" "$f"
```

**Should happen:** node-exporter picks up the new value on its next scrape;
`time() - metric` exceeds 129600 → **`BackupStale`** (severity critical, no `for:`
→ fires at the next eval + `group_wait`) → Discord page.

**Recovery:** run the real backup, which rewrites the metric with a true fresh
timestamp and resolves the alert:
```sh
/opt/qincloud/scripts/backup.sh     # or: systemctl start qincloud-backup.service
```

**Verify:**
```sh
curl -s "http://$TS_IP:9090/api/v1/query?query=time()-qincloud_backup_last_success_timestamp_seconds"
# expect > 129600 after injection, then well under it after backup.sh
curl -s "http://$TS_IP:9090/api/v1/alerts" | grep -o BackupStale
```

**Prod safety:** **low-risk maintenance window.** It is reversible and never
touches backup *data* — but while the stale value is in place, a **real** backup
failure would be masked. **Always finish by running `backup.sh`** so the metric
reflects truth again. RTO/RPO context: a real stale-backup incident is bounded by
the M2 restore path (RTO ≈ 4s/DB from R2) and the nightly RPO (≤ 24h).

---

## Drill D — DB outage

**Simulates:** Postgres (or Redis) falling over — the datastore behind controld
and every `-db` tenant app.

**Inject:**
```sh
docker stop qincloud-postgres     # or: docker stop qincloud-redis
```

**Should happen — Postgres:**
- `pg_up → 0` (direct signal, visible on the dashboard).
- controld's `GET /metrics` does a `store.ListApps()` DB read on **every scrape**
  (see `controld/internal/dashboard/observe.go`); with Postgres down that scrape
  returns 500 → `up{job="controld"} → 0` → **`InstanceDown{job="controld"}`** fires
  ≈ 4m40s after T0. **That is the page you get today** for a DB outage.
- Tenant apps with `-db` and the controld dashboard lose the database.

**Should happen — Redis:** `redis_up → 0`; apps using Redis degrade. Note the
redis-*exporter* stays up (it just reports `redis_up=0`), so `InstanceDown` does
**not** fire for the `redis` job.

**Recovery / RTO:** `docker start qincloud-postgres`; the healthcheck
(`pg_isready`) goes green in ~30s (`start_period: 30s`), controld `/metrics`
returns 200, `up{job="controld"}` recovers → resolved page. A stop/start loses
nothing (the `pgdata` volume is intact). If the data itself is bad, the recovery
is the restore path — [`2026-07-06-m2-backup-restore-drill.md`](2026-07-06-m2-backup-restore-drill.md)
(RTO ≈ 4s/DB from R2, RPO ≤ 24h).

**Verify:**
```sh
curl -s "http://$TS_IP:9090/api/v1/query?query=pg_up"                       # 0 while stopped
curl -s "http://$TS_IP:9090/api/v1/query?query=up%7Bjob%3D%22controld%22%7D" # → 0 after the scrape 500s
```

**Prod safety:** **maintenance window only** — this is a real outage of controld
and every `-db` app.

> **Gaps (today):** there is **no direct alert** on `pg_up == 0` or
> `redis_up == 0`. Postgres pages only *indirectly* (via the controld target
> going down), and **a Redis outage does not page at all**. → Action items: add
> `pg_up == 0` and `redis_up == 0` alerts so the datastore pages on its own
> signal, at the cause, not three layers downstream.

---

## Drill E — Full-box loss

**Simulates:** the entire VPS gone — disk death, provider incident, fat-fingered
`rm -rf`. This is the **M8 disaster-recovery drill**; do not re-derive it here.

**Full procedure, numbers, and findings:**
[`2026-07-06-m8-box-rebuild-drill.md`](2026-07-06-m8-box-rebuild-drill.md) — teardown
of all containers/volumes/networks/images + `/opt/qincloud`, then rebuild from
`bootstrap.sh` + the repo + R2 restore + operator secrets. Measured **RTO ≈ 12 min,
RPO ≈ 90s in-drill (≤ 24h in a real loss)**.

**Should happen:** **nothing pages.** Alertmanager lives *on the box* — a full-box
outage is invisible to the pager by construction (M8 finding #5: "while the box is
down, nobody pages"). This is the platform's one known monitoring blind spot.

**Compensating control (action item):** an **external** uptime check
(healthchecks.io / UptimeRobot) on the public site *and* on backup freshness — the
only thing that can page when the box itself is the thing that died.

**Recovery / RTO:** the M8 rebuild procedure. Always take and **verify** a fresh
backup *before* teardown (M8 did, at T−1min) — the pre-drill backup is your RPO
floor.

**Prod safety:** **maintenance window, announced.** This drill destroys prod; run
it only against a verified-fresh backup and with the rebuild runbook open.

---

## Coverage summary & standing action items

| Failure | Fires today | Rule / signal |
| --- | --- | --- |
| Scrape target down | ✅ | `InstanceDown` (`up == 0`) |
| Container crash-loop | ✅ | `ContainerRestartLoop` |
| Backup stalled | ✅ | `BackupStale` |
| Disk >85% / Mem >90% | ✅ | `DiskUsageHigh` / `MemoryHigh` (test by filling a scratch file / a stress run in a window) |
| DB (Postgres) down | ⚠️ indirect | via `InstanceDown{job="controld"}` |
| App stopped (not looping) | ❌ | — → add `qincloud_app_up == 0` |
| DB (Redis) down | ❌ | — → add `redis_up == 0` |
| Postgres down (direct) | ❌ | — → add `pg_up == 0` |
| Full-box loss | ❌ by design | — → external uptime check |

Every ❌/⚠️ above is a real gap in the current alert pack, not an oversight in a
drill — carry them as action items in the next observability iteration and prove
each new rule with the matching drill in this catalogue before calling it done.
