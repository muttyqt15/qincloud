# Drill: M3 pager path — target death → Discord page → recovery (2026-07-06)

**Goal:** prove the full alerting pipeline with a real failure, not a synthetic
POST: exporter dies → Prometheus detects → `InstanceDown` fires → Alertmanager
→ Discord webhook → and the resolved notification on recovery.

## What happened (all times UTC)

| T | Event |
| --- | --- |
| 10:00 | Alertmanager started with the webhook secret installed `600 root:root` |
| 10:01:46 | First synthetic test alert **failed to notify**: `read webhook_url_file: open /run/secrets/discord_webhook: permission denied` |
| ~10:05 | Root cause fixed: `chown 65534:65534 && chmod 400` on the host file; read verified from inside the container |
| 10:10:17 | Synthetic test alert #2 dispatched cleanly (no notify error) |
| 10:12:07 | **Drill T0** — `docker stop observability-redis-exporter-1` |
| +30s | `InstanceDown` **pending** (the `for: 3m` anti-flap window doing its job) |
| ~10:16:45 | `InstanceDown` **firing** → sent to Alertmanager |
| 10:17:46 | Outage held 75s past firing (so `group_wait: 30s` flushed the page), then exporter restarted |
| ~10:23 | Alert resolved; resolved notification flushed within `group_interval: 5m` |
| 10:12–11:02 | Alertmanager log: **zero** `Notify ... failed` entries — both the firing and resolved Discord messages were dispatched successfully |

Detection-to-page ≈ **4m40s** (15s scrape interval + 3m `for:` + rule eval +
30s `group_wait`). That latency is a deliberate trade: it is the price of not
being paged for a scrape blip.

## Root cause of the failed first attempt

Compose file-secrets are bind mounts that keep **host** permissions, and the
alertmanager image runs as `nobody` (uid 65534) — a `600 root:root` secret is
unreadable at notify time, and the config loads fine so nothing fails until
the first page. The requirement is now documented at the secret definition in
`stack/observability/compose.yml`:
`install -o 65534 -g 65534 -m 400 <src> /opt/qincloud/secrets/discord_webhook`.

## Learnings

1. **A loaded config is not a working pipeline.** Alertmanager healthy +
   config parsed + target up proved nothing about the last hop; only a real
   dispatch surfaced the permission error. Class: container-user vs
   host-file-ownership mismatches fail at *use* time, not *load* time.
2. **Drills must respect the notification pipeline's timing.** Drill attempt
   #1 restarted the exporter seconds after firing; the alert resolved before
   `group_wait` flushed, so no page was ever sent — a "passing" drill that
   proved nothing. The failure must be held past the flush window.
3. **The box is the record, not the laptop.** Both drill attempts' local ssh
   sessions hung mid-poll; container uptimes + Alertmanager logs on the box
   were sufficient (and more trustworthy) evidence.
