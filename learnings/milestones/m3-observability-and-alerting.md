---
title: M3 — Observability & a Real Pager
slug: m3-observability-and-alerting
type: milestone
milestone: M3
status: stable
difficulty: 3
tags: [qincloud, observability, reliability, devops]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m1-edge-and-tls]]", "[[m2-data-and-backups]]", "[[x2-per-app-observability]]", "[[verify-the-artifact-under-test]]", "[[observe-what-matters]]", "[[fail-loud-at-boundaries]]", "[[root-cause-over-patch]]"]
sources: ["stack/observability/compose.yml", "runbooks/drills/2026-07-06-m3-pager-drill.md", "runbooks/gotchas/alerting.md"]
---

# M3 — Observability & a Real Pager

> **In one sentence:** stand up the "nervous system" of the box — metrics, logs, and alerts — and then *prove* the alert actually reaches a human by killing a real service and watching the page land in Discord and clear again.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Imagine a hospital with no vital-signs monitors. A patient could be crashing and nobody would know until someone happened to walk past the bed. That is a server with no observability: it can be out of disk, leaking memory, or completely down, and the first person to notice is an angry user.

Observability is the bank of monitors. It has three feeds:

- **Metrics** — numbers over time: CPU, memory, "is this service answering?" (Prometheus collects them, Grafana draws the charts).
- **Logs** — the running commentary each program writes (Loki stores them, Alloy ships them off every container).
- **Alerts** — the part that pokes a human when a number crosses a line (Alertmanager decides *who* gets told and *how*).

But a monitor that beeps into an empty room is worthless. The real deliverable of M3 was not "we installed Prometheus" — anyone can do that. It was **proving the beep reaches someone**: a genuine failure travelling the whole chain, from a dead service all the way to a phone buzzing, and then a second buzz when it recovers. A pager you have never tested is not a pager; it is a decoration.

## 2. The plan (initial approach)

Run the standard, boring, battle-tested stack in one Docker Compose file on the box: Prometheus (metrics), Grafana (dashboards), Loki + Alloy (logs), Alertmanager (routing), plus exporters that translate the host, containers, Postgres, and Redis into metrics. Keep every admin UI off the public internet — bound only to the Tailscale IP, our private admin network. Write one honest alert (`InstanceDown`: a monitored thing stopped answering), point Alertmanager at a Discord webhook, and call it done once the config loaded cleanly.

That last clause — "once the config loaded cleanly" — is exactly where it bit us.

## 3. Where it deviated

Two things looked finished but were inert.

**The page that never sent.** Alertmanager started healthy, parsed its config without complaint, and reported the webhook configured. Everything green. The first real test alert then failed at the *last* hop:

```
read webhook_url_file: open /run/secrets/discord_webhook: permission denied
```

The webhook secret sat on the host as `600 root:root`. But the Alertmanager container runs as user `nobody` (uid 65534), and a Compose *file-secret* is a bind mount that keeps the **host's** permissions. So `nobody` could not read a root-only file. Crucially, the config loads fine regardless — the file is only *read* when a notification actually fires. Green healthcheck, parsed config, target up: none of it exercised the one line that mattered.

**The drill that proved nothing.** The first attempt to run the outage drill restarted the killed service seconds after the alert fired. The alert resolved before Alertmanager's `group_wait` window flushed, so **no page was ever sent** — and the drill "passed". A test that recovers before the pipeline can act is a green checkmark over an untested path.

## 4. The fix — and how I found it

The permission fix is one line, but the *lesson* is where it points:

```sh
install -o 65534 -g 65534 -m 400 <src> /opt/qincloud/secrets/discord_webhook
```

Chown the secret to the container's runtime uid before first launch. The root cause was not "wrong chmod" — it was trusting a *loaded config* as proof of a *working pipeline*. The rule now lives at the secret definition in `stack/observability/compose.yml:232` and in `runbooks/gotchas/alerting.md`: **check the image's runtime uid before installing its secret; a green healthcheck proves nothing about the last hop.**

The drill fix was to respect the pipeline's own clock. Kill the target, then hold it dead well past the flush window so a page is forced, then wait out the resolved window. The real drill (`runbooks/drills/2026-07-06-m3-pager-drill.md`) did exactly this:

- **T0 10:12:07** — `docker stop observability-redis-exporter-1`.
- **+30s** — `InstanceDown` goes **pending** (the `for: 3m` anti-flap window working).
- **~10:16:45** — **firing** → handed to Alertmanager.
- **10:17:46** — held 75s past firing so `group_wait: 30s` flushed the Discord page, *then* restarted the exporter.
- **~10:23** — resolved notification flushed within `group_interval: 5m`.
- **10:12–11:02** — Alertmanager log shows **zero** `Notify … failed` entries: both the firing and resolved messages dispatched.

Detection-to-page came to **≈ 4m40s** (15s scrape + 3m `for:` + rule eval + 30s `group_wait`). That is not slowness to fix — it is the deliberate price of *not* being paged for a one-scrape blip.

## 5. Going deep (systems level)

Operate it from these facts:

- **One stack, three networks.** `stack/observability/compose.yml` joins `obs` (internal scrape/query plane), `admin_net`, and `data_net`. Prometheus is deliberately **not** on `app_net` — being off it is what lets Caddy's `:2019` metrics block allow only the `admin_net` range and stay scrapable (`compose.yml:26`).
- **Nothing admin is public.** Every UI binds `"${TS_IP:?}:PORT"`. The `:?` is load-bearing: if `TS_IP` were empty, `"${TS_IP}:3000:3000"` would silently bind `0.0.0.0` and expose Grafana to the world. Fail loud instead (`compose.yml:5`).
- **No remote reload.** Prometheus runs without `--web.enable-lifecycle`, so nothing reachable can POST `/-/quit` or `/-/reload`. Reload with `docker compose kill -s SIGHUP prometheus` (`compose.yml:17`).
- **Exporters that quietly export *nothing*.** Two traps documented inline: cAdvisor under Docker 29's containerd image store silently creates **zero** per-container series unless pointed at containerd (`--docker=unix:///nonexistent.sock --containerd=…`, `compose.yml:172`); and OOM detection needs `cap_add: SYSLOG` or `container_oom_events_total` is permanently 0 — a per-app OOM, the exact event a memory-capped PaaS most wants, would go unseen (`compose.yml:181`). Per-app labels (`app="<name>"`) come from controld's `/metrics`, not cAdvisor, whose series carry the container ID.
- **Host-side jobs report via the textfile collector.** `backup.sh` writes `qincloud_backup.prom` into `/opt/qincloud/metrics` (mounted ro into node-exporter). Write tmp + `mv`, since the collector may read mid-write.
- **The metric-that-doesn't-exist trap.** An alert whose expr matches no series never pends — it looks armed and is inert. `BackupStale` shipped exactly this way. An alert isn't done until its metric has been observed in Prometheus *and* the expr returns a value below threshold (`runbooks/gotchas/alerting.md`).

## 6. How this compares to best practice

The component choices *are* best practice: Prometheus + Grafana + Loki + Alertmanager is the reference open-source stack, and we picked Alloy over Promtail precisely because Promtail hit EOL (2026-03). A mature team would add long-term metric storage (Mimir/Thanos), an on-call rotation (PagerDuty/Grafana OnCall) instead of a single Discord channel, and richer SLO-burn alerts.

The corner we cut deliberately: one box, one Discord webhook, 15d/4GB retention, and a human-in-the-loop pager rather than escalation policies. For a one-VPS portfolio platform that is correct sizing, not laziness. It would need revisiting the moment a second on-call person or a customer SLA exists. What we did *not* cut is the thing teams most often skip: actually firing the pager against a real outage before trusting it.

## 7. The underlying why (the transferable lesson)

**A pipeline is only as real as its last untested hop.** Every failure in M3 shared one shape: something *loaded* successfully and was mistaken for something that *works*. The config parsed but the secret was unreadable at notify time. The alert existed but its metric didn't. The drill passed but recovered before a page could send. Loading, parsing, and going green all happen *before* the moment of truth; the moment of truth is the notify, the OOM, the real outage. So you must manufacture that moment on purpose — kill a real service, hold it past the flush window, and read the evidence off the box, not the laptop whose ssh session hangs mid-drill. Test the artifact under load, at the boundary where it is actually used, or you have tested nothing.

---
**Teaches:** [[verify-the-artifact-under-test]] · [[observe-what-matters]] · [[fail-loud-at-boundaries]] · [[root-cause-over-patch]]
**Sources:** `stack/observability/compose.yml`, `runbooks/drills/2026-07-06-m3-pager-drill.md`, `runbooks/gotchas/alerting.md`
