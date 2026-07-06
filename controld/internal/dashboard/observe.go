// observe.go — the dashboard's live-container observability: the polled
// stats/logs fragments on the app detail page, and GET /metrics, the
// Prometheus exposition that gives every app first-class app="<name>" series
// (cAdvisor can't: under Docker's containerd image store its series carry
// only the raw container ID — see stack/observability/compose.yml).
package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"qincloud/controld/internal/deploy"
)

const logTailLines = 100

// appStats renders one polled stats line for the app detail page.
func (s *Server) appStats(w http.ResponseWriter, r *http.Request) {
	app, done := s.fragmentApp(w, r)
	if done {
		return
	}
	stats, err := s.runtime.Stats(r.Context(), app.ContainerID)
	if err != nil {
		render(w, r, MutedLine("stats unavailable — is the container running?"))
		return
	}
	render(w, r, StatsLine(statsText(stats)))
}

// appLogs renders the polled log tail for the app detail page.
func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	app, done := s.fragmentApp(w, r)
	if done {
		return
	}
	logs, err := s.runtime.Logs(r.Context(), app.ContainerID, logTailLines)
	if err != nil {
		render(w, r, MutedLine("logs unavailable — is the container running?"))
		return
	}
	render(w, r, LogsPane(logs))
}

// fragmentApp resolves the app behind a polled fragment. done=true means a
// response was already written: 286 stop-poll when the app is gone (same
// treatment as appHistory), a muted line when it has never deployed.
func (s *Server) fragmentApp(w http.ResponseWriter, r *http.Request) (*deploy.App, bool) {
	name := r.PathValue("name")
	app, err := s.store.GetApp(r.Context(), name)
	if err != nil {
		serverError(w, err)
		return nil, true
	}
	if app == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(htmxStopPolling)
		render(w, r, FlashError("app "+name+" no longer exists — destroyed"))
		return nil, true
	}
	if app.ContainerID == "" {
		render(w, r, MutedLine("not deployed yet"))
		return nil, true
	}
	return app, false
}

// statsText formats one snapshot the way an operator reads it.
func statsText(s deploy.ContainerStats) string {
	return fmt.Sprintf("cpu %.1f%% · mem %s / %s", s.CPUPercent, mib(s.MemBytes), mib(s.MemLimit))
}

func mib(b int64) string {
	return strconv.FormatInt(b>>20, 10) + " MiB"
}

// metricsConcurrency bounds the parallel Stats calls in the exporter. Each
// call blocks ~1-1.5s (the daemon double-samples for a CPU rate), so serial
// sampling flips the whole scrape past the 10s timeout at ~6 apps. Sampling
// concurrently keeps the scrape near one call's latency regardless of app
// count; the daemon handles concurrent stats fine.
const metricsConcurrency = 8

// metrics is a Prometheus exposition of per-app resource gauges.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type sample struct {
		app string
		up  bool
		st  deploy.ContainerStats
	}
	samples := make([]sample, len(apps))
	sem := make(chan struct{}, metricsConcurrency)
	var wg sync.WaitGroup
	for i, a := range apps {
		samples[i] = sample{app: a.Name}
		if a.ContainerID == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, cid string) {
			defer wg.Done()
			defer func() { <-sem }()
			if st, err := s.runtime.Stats(r.Context(), cid); err == nil {
				samples[i].up, samples[i].st = true, st
			}
		}(i, a.ContainerID)
	}
	wg.Wait()

	// One HELP/TYPE block per metric — a repeated TYPE line for the same
	// metric is a parse error to Prometheus, so emit grouped, not per-app.
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP qincloud_app_up Whether the app's live container was running and statable.")
	fmt.Fprintln(w, "# TYPE qincloud_app_up gauge")
	for _, sm := range samples {
		fmt.Fprintf(w, "qincloud_app_up{app=%q} %d\n", sm.app, boolToInt(sm.up))
	}
	fmt.Fprintln(w, "# HELP qincloud_app_cpu_percent CPU usage of the app's live container, percent of one core.")
	fmt.Fprintln(w, "# TYPE qincloud_app_cpu_percent gauge")
	for _, sm := range samples {
		if sm.up {
			fmt.Fprintf(w, "qincloud_app_cpu_percent{app=%q} %s\n", sm.app, strconv.FormatFloat(sm.st.CPUPercent, 'f', -1, 64))
		}
	}
	fmt.Fprintln(w, "# HELP qincloud_app_memory_bytes Working-set memory of the app's live container.")
	fmt.Fprintln(w, "# TYPE qincloud_app_memory_bytes gauge")
	for _, sm := range samples {
		if sm.up {
			fmt.Fprintf(w, "qincloud_app_memory_bytes{app=%q} %d\n", sm.app, sm.st.MemBytes)
		}
	}
	fmt.Fprintln(w, "# HELP qincloud_app_memory_limit_bytes Memory cap of the app's live container.")
	fmt.Fprintln(w, "# TYPE qincloud_app_memory_limit_bytes gauge")
	for _, sm := range samples {
		if sm.up {
			fmt.Fprintf(w, "qincloud_app_memory_limit_bytes{app=%q} %d\n", sm.app, sm.st.MemLimit)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
