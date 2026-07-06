---
title: "Extra — Per-App Resource Metrics & Logs"
slug: x2-per-app-observability
type: milestone
milestone: X2
status: stable
difficulty: 4
tags: [qincloud, observability, golang, devops, reliability]
created: 2026-07-06
updated: 2026-07-06
related: ["[[m3-observability-and-alerting]]", "[[m4-controld-deploy-engine]]", "[[m5-dashboard]]", "[[m6-first-app-umami]]"]
sources: ["controld/internal/dashboard/observe.go", "stack/observability/compose.yml", "commit — cadvisor containerd repoint + controld /metrics"]
---

# Extra — Per-App Resource Metrics & Logs

> **In one sentence:** answer "how much is *this one app* using, and what is it logging?" — both on the dashboard's app page and in Grafana — after discovering that the industry-standard container-metrics agent had quietly stopped reporting anything per-container on Docker 29.

*This note ramps from plain English (§1–4) to the systems level (§5+). Read as far down as you want to go.*

## 1. What we were building — and why it matters (plain English)

Imagine an apartment building where you, the landlord, pay one electricity and water bill for the whole block. That is fine until a tenant asks "why is my rent going up?" or a neighbour complains someone is flooding the basement. You suddenly need a *per-apartment* meter, not just the building total.

QinCloud is that building: one small server running many little apps in Docker containers. Up to this point we could see the *whole box* — total CPU, total memory, total disk (that was [[m3-observability-and-alerting]]). What we couldn't answer was the per-tenant question: **is the Umami analytics app the thing eating memory, or is it Postgres?** And when an app misbehaves, the operator wants its **logs** — the running commentary a program prints about what it's doing — filtered down to just that one app, not the firehose of every container at once.

So this milestone adds two per-app views: a live **resource readout** (cpu %, memory used / cap) and a **log tail**, both on the app's page in the dashboard, and the same numbers pushed into Grafana so you can graph them over time and alert on them.

## 2. The plan (initial approach)

The textbook answer is a tool called **cAdvisor** (Container Advisor) — Google's standard agent that watches Docker and emits a Prometheus metric for every container: `container_cpu_usage`, `container_memory_working_set_bytes`, and so on. Prometheus is our time-series database; it *scrapes* (periodically fetches) these numbers and stores them. The plan was the boring, correct one:

1. Run cAdvisor in the observability stack, pointed at the Docker socket.
2. Let it emit per-container series automatically.
3. Graph and alert in Grafana; done.

For the live dashboard readout and log tail, controld (our Go control plane) already talks to the container runtime, so it would just ask the runtime for a one-shot stats snapshot and the last 100 log lines and render them.

## 3. Where it deviated

cAdvisor came up healthy, its `/healthz` was green — and it emitted **zero** per-container series. Not wrong numbers. *No* numbers. The building total worked; the per-apartment meters were all blank.

The cause was a silent platform shift underneath us. **Docker 29 switched to the containerd image store.** Older Docker kept image layers in a "layerdb" directory that cAdvisor's Docker handler reads to attribute usage to a container. Under the new containerd-backed store that directory is gone, so cAdvisor's docker factory can't resolve a container's read-write layer — and instead of erroring loudly, it just **declines to create the series**. A green healthcheck the whole time.

There was a second, subtler problem waiting even once metrics flowed: cAdvisor labels a series by the raw **container ID** (a 64-hex string), because that is all it knows. But an operator — and an alert — wants `app="umami"`, not `name="3f9a…"`. cAdvisor structurally *cannot* give us our app identity, because it doesn't know it; only controld, which deployed the container, does.

## 4. The fix — and how I found it

The finding was the hard part: a healthy component emitting nothing looks like a config typo, not a platform change. The tell was that the *box-level* cAdvisor machine series existed while *per-container* ones didn't — that isolates the fault to the container-attribution path, i.e. the Docker layer store, which pointed straight at the Docker 29 store migration.

The fix has two halves, and it maps cleanly onto the two problems:

**Half one — repoint cAdvisor at the real source.** Docker 29 runs containerd underneath, in a namespace called `moby`. cAdvisor can read containerd directly. So we tell it to stop using the broken docker factory (point it at a dead socket so it stands down) and monitor through containerd instead — `stack/observability/compose.yml:172-174`:

```yaml
- --docker=unix:///nonexistent.sock
- --containerd=/run/containerd/containerd.sock
- --containerd-namespace=moby
```

Series come back. But they carry the container ID in `name`, still not our app name.

**Half two — controld exports its own app-labelled gauges.** Rather than fight cAdvisor to inject a label it can't know, we make controld — the one component that *does* know which container is which app — publish its own Prometheus endpoint at `GET /metrics` with first-class `app="<name>"` labels. This is the file-header contract in `controld/internal/dashboard/observe.go:1-5`. The two exporters are complementary: cAdvisor gives rich, high-cardinality container internals keyed by ID; controld gives the small set of gauges an operator and an alert actually name apps by (`qincloud_app_up`, `qincloud_app_cpu_percent`, `qincloud_app_memory_bytes`, `qincloud_app_memory_limit_bytes`).

## 5. Going deep (systems level)

**The exporter (`observe.go:87-148`).** On each scrape, `metrics` lists every app, snapshots each running container's stats, and prints Prometheus text exposition. Two implementation details are load-bearing:

- **Grouped HELP/TYPE, not per-app.** Prometheus treats a repeated `# TYPE` line for the same metric as a *parse error* — it would reject the whole scrape. So the code emits one HELP/TYPE header per metric, then loops all apps under it (`observe.go:119-126`), never interleaved. Get this wrong and every app's metrics vanish at once.
- **Parallel sampling with a semaphore.** Getting a container's CPU *percent* requires the daemon to sample twice a fraction of a second apart to compute a rate — each `Stats` call blocks ~1–1.5s. Done serially, the scrape time grows linearly with app count and **blows past Prometheus's 10s scrape timeout at ~6 apps** (`observe.go:79-84`). The fix is a bounded fan-out: a `chan struct{}` semaphore of width `metricsConcurrency = 8` plus a `sync.WaitGroup`, so total latency stays near *one* call's latency regardless of app count. `up` and stats write into a pre-sized `samples` slice by index, so there's no shared-map contention (`observe.go:99-117`). Down apps (`ContainerID == ""`) are recorded as `qincloud_app_up 0` and skipped — an app that isn't running still has a *known* state, which is exactly what you alert on.

**Scrape wiring.** Prometheus reaches controld at `qincloud-controld:8600` over the `admin_net` network, deliberately *not* `app_net` (`compose.yml:28-33`). Prometheus is off the app network so the scrape plane and the tenant plane stay separated. cAdvisor lives on the `obs` network with the box-level exporters.

**The OOM footgun.** cAdvisor needs `/dev/kmsg` and `CAP_SYSLOG` to detect out-of-memory kills; without them `container_oom_events_total` is permanently 0 (`compose.yml:175-182`). For a memory-*capped* PaaS, a per-app OOM is the single event you most want to catch — so the cap is granted deliberately, with the reasoning inline.

**The live dashboard views (`observe.go:20-45`).** The app page polls `appStats` and `appLogs`. Both route through `fragmentApp` (`observe.go:50-68`), which returns `done=true` after already writing a response for the two non-happy states: the app was destroyed (HTTP `286`, htmx's stop-polling code, so the browser quits hammering a dead endpoint) or it has never deployed (a muted "not deployed yet" line). `statsText` formats one snapshot the way an operator reads it — `cpu 12.4% · mem 84 MiB / 256 MiB` — via a `>>20` bit-shift to MiB (`observe.go:71-77`).

## 6. How this compares to best practice

A mature managed platform (Fly, Railway, ECS) runs essentially this shape: a node agent (cAdvisor/otel-collector) for container internals, plus a control-plane-owned exporter that stamps *its* identities (app, tenant, region) onto the series, because only the control plane knows them. We match that split.

Where we cut corners knowingly: controld samples on-demand *inside the scrape* rather than maintaining a background sample loop, so a slow daemon can slow a scrape (bounded by the semaphore, capped by the timeout). At QinCloud's scale — a handful of apps on one box — that is simpler and fine; at hundreds of containers you'd move to a push/background-sampled model. And we accept cAdvisor's 30s `housekeeping_interval` (default 1s) to spare CPU on a 4-vCPU box — coarser resolution traded for headroom.

## 7. The underlying why (the transferable lesson)

Three things generalise past this box:

1. **A green healthcheck is not "it works."** cAdvisor was *live* and *useless* — it answered `/healthz` while emitting nothing. Liveness proves the process is up, not that it is producing the artifact you need. Verify the *output*, not the pulse.
2. **Identity belongs to whoever creates it.** cAdvisor could never label by app because it never knew the mapping. The component that *deploys* the container is the only honest source of "which app is this," so the app-labelled series must come from *it*, not from bolting a label onto a downstream agent that would have to guess.
3. **A patch hides the symptom; a root-cause fix moves to the real source.** Zero series wasn't a metrics bug — it was a platform migration (Docker 29 → containerd store). Repointing at containerd fixed the *class*, not the instance.

---
**Teaches:** [[observe-what-matters]] · [[verify-the-artifact-under-test]] · [[single-source-of-truth]] · [[root-cause-over-patch]]
**Sources:** `controld/internal/dashboard/observe.go`, `stack/observability/compose.yml`, commit — cadvisor containerd repoint + controld `/metrics`
