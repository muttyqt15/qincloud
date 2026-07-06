// dashboard.go — the M5 web dashboard, mounted by `controld serve` on :8600.
// Two ingress paths: (1) TS_IP:8600, the unauthenticated tailnet path
// (reachability is the auth model there, same as Grafana/Prometheus,
// invariant #3); (2) https://dash.sparboard.com, public, behind Caddy
// basic_auth, proxied over admin_net (deliberate operator decision
// 2026-07-06). Every state-changing route additionally requires the
// HX-Request header (requireHtmx) so neither path is CSRF-forgeable.
// Server-rendered templ views + htmx; all deploy state lives in Postgres —
// the deploys table is the single status projection and the UI only ever
// re-reads it, so a dashboard restart loses nothing.
package dashboard

import (
	"context"
	"embed"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"qincloud/controld/internal/deploy"
)

const (
	// deployBudget/destroyBudget mirror the CLI's per-command timeouts.
	deployBudget  = 5 * time.Minute
	destroyBudget = 2 * time.Minute

	// fastFailWindow is how long POST /deploy waits before handing off to
	// polling. Pre-flight failures (validation, lock held, host conflict) are
	// pure DB work and surface within milliseconds; anything still running at
	// the window's end has already recorded a deploys row, so status and
	// errors reach the operator through the table instead.
	fastFailWindow = time.Second

	// staleAfter: a non-terminal deploy this old means the deploying process
	// died without recording an outcome (controld restart mid-deploy) — far
	// beyond the state machine's own 5-minute budget. Render it abandoned
	// rather than polling "starting…" forever.
	staleAfter = 10 * time.Minute

	// imageResolveTimeout bounds the pull-and-inspect behind the deploy form's
	// image check. A cold pull of a large image can take a while; the field
	// shows a spinner until this elapses.
	imageResolveTimeout = 3 * time.Minute

	historyLimit = 20
)

// Store is what the dashboard reads — the consumer-side subset of
// *store.Store, per the contract.go convention.
type Store interface {
	GetApp(ctx context.Context, app string) (*deploy.App, error)
	ListApps(ctx context.Context) ([]deploy.App, error)
	ListDeploys(ctx context.Context, app string, limit int) ([]deploy.DeployRecord, error)
	LatestDeploys(ctx context.Context) (map[string]deploy.DeployRecord, error)
}

// Runtime is the live-container observability surface — implemented by
// *dockerx.Client. Read-only from the deploy state machine's view: it inspects
// but never mutates app state (ResolveImage does pull an image, which only
// warms the local cache).
type Runtime interface {
	Stats(ctx context.Context, containerID string) (deploy.ContainerStats, error)
	Logs(ctx context.Context, containerID string, tail int) (string, error)
	// ResolveImage verifies an image is real/pullable and reports its exposed
	// ports, so the deploy form can auto-detect the container port instead of
	// making the operator guess.
	ResolveImage(ctx context.Context, ref string) (deploy.ImageInfo, error)
}

// Deployer is the action surface — implemented by *deploy.Deployer.
type Deployer interface {
	Deploy(ctx context.Context, spec deploy.AppSpec) error
	// Redeploy re-runs the app's stored spec, reading it under the app lock.
	// The dashboard must NOT GetApp-then-Deploy itself: between its read and
	// the deploy taking the lock, a destroy can win the lock and delete the
	// app — the stale spec would then resurrect it.
	Redeploy(ctx context.Context, app string) error
	Destroy(ctx context.Context, app string) error
}

type Server struct {
	store    Store
	deployer Deployer
	runtime  Runtime
	fastFail time.Duration // fastFailWindow; tests shrink it
}

func New(st Store, d Deployer, rt Runtime) *Server {
	return &Server{store: st, deployer: d, runtime: rt, fastFail: fastFailWindow}
}

//go:embed static
var staticFS embed.FS

// Register mounts the dashboard on mux. Method+path patterns (Go 1.22+);
// "GET /{$}" keeps unknown paths a 404 instead of serving the index.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /apps", s.appList)
	mux.HandleFunc("GET /apps/{name}", s.appDetail)
	mux.HandleFunc("GET /apps/{name}/history", s.appHistory)
	mux.HandleFunc("GET /apps/{name}/stats", s.appStats)
	mux.HandleFunc("GET /apps/{name}/logs", s.appLogs)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("POST /images/resolve", requireHtmx(s.resolveImage))
	mux.HandleFunc("POST /deploy", requireHtmx(s.deploy))
	mux.HandleFunc("POST /apps/{name}/redeploy", requireHtmx(s.redeploy))
	mux.HandleFunc("POST /apps/{name}/destroy", requireHtmx(s.destroy))
}

// requireHtmx gates every state-changing route on the HX-Request header.
// Browsers refuse to attach custom headers cross-origin without a CORS
// preflight (which this server never grants), so a drive-by page on the
// public internet cannot forge a destroy against the tailnet IP — CSRF
// protection without tokens, because there is no session to protect.
func requireHtmx(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HX-Request") != "true" {
			http.Error(w, "htmx requests only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// --- GET: pages and polled fragments -----------------------------------------

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	apps, latest, err := s.overview(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, IndexPage(apps, latest, time.Now()))
}

func (s *Server) appList(w http.ResponseWriter, r *http.Request) {
	apps, latest, err := s.overview(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, AppList(apps, latest, time.Now()))
}

func (s *Server) overview(ctx context.Context) ([]deploy.App, map[string]deploy.DeployRecord, error) {
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return nil, nil, err
	}
	latest, err := s.store.LatestDeploys(ctx)
	if err != nil {
		return nil, nil, err
	}
	return apps, latest, nil
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetApp(r.Context(), name)
	if err != nil {
		serverError(w, err)
		return
	}
	if app == nil {
		http.NotFound(w, r)
		return
	}
	deploys, err := s.store.ListDeploys(r.Context(), name, historyLimit)
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, AppPage(*app, deploys, time.Now()))
}

// htmxStopPolling: htmx cancels an `every Ns` poll when a response arrives
// with this status, after swapping the body in.
const htmxStopPolling = 286

func (s *Server) appHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetApp(r.Context(), name)
	if err != nil {
		serverError(w, err)
		return
	}
	if app == nil {
		// The app was destroyed while its detail page was open. A plain 200
		// with an empty table would masquerade as valid state and the page
		// would poll it forever — stop the poll and say what happened.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(htmxStopPolling)
		render(w, r, FlashError("app "+name+" no longer exists — destroyed"))
		return
	}
	deploys, err := s.store.ListDeploys(r.Context(), name, historyLimit)
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, HistoryTable(deploys, time.Now()))
}

// --- POST: image resolution (the deploy form's auto-detect) -------------------

// resolveImage verifies the typed image and reports its exposed ports so the
// operator does not pick a container port by hand. It renders the deploy
// form's #image-result fragment: the port field it emits is what the operator
// then submits, so a good deploy never reaches the daemon with a guessed port.
func (s *Server) resolveImage(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.FormValue("image"))
	if ref == "" {
		render(w, r, ImageUnchecked())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), imageResolveTimeout)
	defer cancel()
	info, err := s.runtime.ResolveImage(ctx, ref)
	if err != nil {
		render(w, r, ImageResolveError(ref, err.Error()))
		return
	}
	render(w, r, ImageResolved(info))
}

// --- POST: actions ------------------------------------------------------------

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	spec, err := specFromForm(r)
	if err != nil {
		flashError(w, r, err) // into the modal's #deploy-flash; modal stays open
		return
	}
	s.runDeploy(w, r, "deploy of "+spec.Name, func(ctx context.Context) error {
		return s.deployer.Deploy(ctx, spec)
	})
}

// runDeploy is runAsync for the modal: on success it fires the HX-Trigger the
// page listens on to close the dialog, and OOB-swaps the confirmation onto the
// main #flash (the modal — and its #deploy-flash — is gone by then); on error
// it renders into the modal's #deploy-flash so the operator can fix and retry
// without losing what they typed.
func (s *Server) runDeploy(w http.ResponseWriter, r *http.Request, what string, run func(context.Context) error) {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deployBudget)
		defer cancel()
		err := run(ctx)
		if err != nil {
			log.Printf("dashboard: %s failed: %v", what, err)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			flashError(w, r, err)
			return
		}
		s.deployStarted(w, r, what+" finished")
	case <-time.After(s.fastFail):
		s.deployStarted(w, r, what+" started — status appears in the list")
	}
}

func (s *Server) deployStarted(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("HX-Trigger", "deploy-started") // app.js closes + resets the modal
	render(w, r, DeployStarted(msg))               // OOB-swaps the flash onto the page
}

func (s *Server) redeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.runAsync(w, r, "redeploy of "+name, func(ctx context.Context) error {
		return s.deployer.Redeploy(ctx, name)
	})
}

func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Detached from the request: a closed tab mid-destroy must not leave the
	// route deleted but the containers running.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), destroyBudget)
	defer cancel()
	if err := s.deployer.Destroy(ctx, name); err != nil {
		flashError(w, r, err)
		return
	}
	render(w, r, Flash(name+" destroyed"))
}

// runAsync starts the deploy/redeploy in the background and waits fastFail
// for an early error, which renders inline; a still-running deploy hands off
// to the app list's polling (its deploys row already exists by then — a
// heuristic: a Postgres stall could push pre-flight past the window, in
// which case the failure is only in the container log, but during such a
// stall the whole dashboard is visibly degraded anyway). The goroutine runs
// on its own context, like the destroy handler and for the same reason.
func (s *Server) runAsync(w http.ResponseWriter, r *http.Request, what string, run func(context.Context) error) {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deployBudget)
		defer cancel()
		err := run(ctx)
		if err != nil {
			// Also in the deploys row; logged for the box-side paper trail.
			log.Printf("dashboard: %s failed: %v", what, err)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			flashError(w, r, err)
			return
		}
		render(w, r, Flash(what+" finished"))
	case <-time.After(s.fastFail):
		render(w, r, Flash(what+" started — status appears in the list"))
	}
}

// specFromForm only converts types; AppSpec.Validate (inside Deploy) is the
// single validation path for both the CLI and the dashboard.
func specFromForm(r *http.Request) (deploy.AppSpec, error) {
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if err != nil {
		return deploy.AppSpec{}, fmt.Errorf("port must be a number")
	}
	env, err := envFromLines(r.FormValue("env"))
	if err != nil {
		return deploy.AppSpec{}, err
	}
	return deploy.AppSpec{
		Name:          strings.TrimSpace(r.FormValue("name")),
		Image:         strings.TrimSpace(r.FormValue("image")),
		ContainerPort: port,
		Host:          strings.TrimSpace(r.FormValue("host")),
		Env:           env,
		UseDB:         r.FormValue("db") == "on",
	}, nil
}

// envFromLines parses the form's env textarea: one KEY=VALUE per line, blank
// lines ignored. Key validity is AppSpec.Validate's job, shape is ours.
func envFromLines(text string) (map[string]string, error) {
	env := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("env line %q: want KEY=VALUE", line)
		}
		env[k] = v
	}
	if len(env) == 0 {
		return nil, nil // no env = nil map, same as a CLI deploy without -env
	}
	return env, nil
}

// envKeys renders which env vars an app carries WITHOUT their values — env
// values are secrets and the detail page must never echo them.
func envKeys(env map[string]string) string {
	if len(env) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

// --- rendering helpers ---------------------------------------------------------

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		log.Printf("dashboard: render: %v", err)
	}
}

func flashError(w http.ResponseWriter, r *http.Request, err error) {
	render(w, r, FlashError(err.Error()))
}

// serverError is for GET pages, where there is no flash region to render
// into. Plain 500 + text; the cause goes to the box-side log.
func serverError(w http.ResponseWriter, err error) {
	log.Printf("dashboard: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// statusView is what a status badge renders: a label and a CSS class.
type statusView struct {
	Label string
	Class string // "live" | "failed" | "progress" | "muted"
}

// statusFor merges an app with its latest deploy into one display status —
// the app list's projection.
func statusFor(app deploy.App, latest map[string]deploy.DeployRecord, now time.Time) statusView {
	rec, ok := latest[app.Name]
	if !ok {
		return statusView{Label: "never deployed", Class: "muted"}
	}
	return deployStatusView(rec, now)
}

func deployStatusView(d deploy.DeployRecord, now time.Time) statusView {
	switch {
	case d.Status == deploy.StatusLive:
		return statusView{Label: "live", Class: "live"}
	case d.Status == deploy.StatusFailed:
		return statusView{Label: "failed", Class: "failed"}
	case now.Sub(d.StartedAt) > staleAfter:
		return statusView{Label: "abandoned (" + string(d.Status) + ")", Class: "failed"}
	default:
		return statusView{Label: string(d.Status) + "…", Class: "progress"}
	}
}

// took renders a deploy's duration: exact when finished, ticking while
// in-flight, "—" once abandoned.
func took(d deploy.DeployRecord, now time.Time) string {
	if d.FinishedAt != nil {
		return d.FinishedAt.Sub(d.StartedAt).Round(time.Second).String()
	}
	if now.Sub(d.StartedAt) > staleAfter {
		return "—"
	}
	return now.Sub(d.StartedAt).Round(time.Second).String() + "…"
}

func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// humanSize renders an image size for the resolve line — rough is fine, it's
// just to give the operator a sense of the pull.
func humanSize(b int64) string {
	const mb = 1 << 20
	const gb = 1 << 30
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	case b >= mb:
		return fmt.Sprintf("%d MB", b/mb)
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
}
