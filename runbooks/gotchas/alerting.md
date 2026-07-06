# Alerting gotchas

Evidence: [drills/2026-07-06-m3-pager-drill.md](../drills/2026-07-06-m3-pager-drill.md)

## Container-user vs host-file-ownership fails at *use* time

Compose file-secrets are bind mounts that keep **host** permissions, and
alertmanager runs as `nobody` (uid 65534). A `600 root:root` webhook file
loads fine at startup and only fails on the first real notify
(`permission denied`). Rule for any secret consumed by a non-root container:

```sh
install -o 65534 -g 65534 -m 400 <src> /opt/qincloud/secrets/discord_webhook
```

(documented at the secret definition in `stack/observability/compose.yml`).
Generalization: check the image's runtime uid *before* installing its secret;
a green healthcheck proves nothing about the last hop.

## An alert on a metric that doesn't exist is a silent no-op

Prometheus evaluates the expr against nothing, gets nothing, and never
pends — it looks armed and is inert. `BackupStale` shipped in exactly this
state. **An alert isn't done until its metric has been observed in Prometheus
and the expr returns a value below threshold.** Wire the metric first (or in
the same change), then verify with an instant query.

## Drills must outlive the pipeline's timing

The path is: scrape interval (15s) + `for:` window (3m) + `group_wait` (30s),
resolved notifications flush within `group_interval` (5m). A drill that
recovers the failure before `group_wait` flushes sends **no page** and proves
nothing. Hold the failure ≥ 60s past *firing*, then wait out `group_interval`
for the resolved message. Detection-to-page ≈ 4m40s is the accepted trade
against scrape-blip pages — don't shorten the windows to make drills faster.

## The box is the record, not the laptop

Local ssh sessions polling a drill can hang mid-observation. Container
uptimes, Prometheus state, and Alertmanager logs on the box are the
trustworthy evidence — write the timeline from those.

## Host-side jobs publish metrics via the textfile collector

Anything running outside a container (backup.sh, future cron jobs) reports to
Prometheus by writing a `.prom` file into `/opt/qincloud/metrics` (mounted ro
into node-exporter, `--collector.textfile.directory`). Write tmp + `mv` —
the collector may read mid-write. Pattern lives at the end of
`scripts/backup.sh`.
