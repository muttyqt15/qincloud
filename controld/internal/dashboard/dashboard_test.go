// dashboard_test.go — handler tests against in-memory fakes (same style as
// deploy_test.go: assert observable end state, never call counts) plus
// table-driven tests for the status projection.
package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"qincloud/controld/internal/deploy"
)

var (
	now      = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	testSpec = deploy.AppSpec{Name: "whoami", Image: "traefik/whoami:v1.10", ContainerPort: 80, Host: "whoami.sparboard.com"}
)

type fakeStore struct {
	apps    []deploy.App
	latest  map[string]deploy.DeployRecord
	deploys []deploy.DeployRecord
}

func (f *fakeStore) GetApp(_ context.Context, name string) (*deploy.App, error) {
	for _, a := range f.apps {
		if a.Name == name {
			return &a, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) ListApps(context.Context) ([]deploy.App, error) { return f.apps, nil }
func (f *fakeStore) ListDeploys(_ context.Context, app string, limit int) ([]deploy.DeployRecord, error) {
	out := []deploy.DeployRecord{}
	for _, d := range f.deploys {
		if d.AppName == app && len(out) < limit {
			out = append(out, d)
		}
	}
	return out, nil
}
func (f *fakeStore) LatestDeploys(context.Context) (map[string]deploy.DeployRecord, error) {
	return f.latest, nil
}

// fakeDeployer reports outcomes through channels so tests synchronize on the
// action itself, not on sleeps.
type fakeDeployer struct {
	deployErr  error
	block      chan struct{}       // non-nil: Deploy/Redeploy waits for close (slow deploy)
	deployed   chan deploy.AppSpec // buffered; receives every completed Deploy
	redeployed chan string         // buffered; receives every completed Redeploy
	destroyed  chan string         // buffered; receives every Destroy
}

func newFakeDeployer() *fakeDeployer {
	return &fakeDeployer{
		deployed:   make(chan deploy.AppSpec, 8),
		redeployed: make(chan string, 8),
		destroyed:  make(chan string, 8),
	}
}

func (f *fakeDeployer) Deploy(_ context.Context, spec deploy.AppSpec) error {
	if f.block != nil {
		<-f.block
	}
	f.deployed <- spec
	return f.deployErr
}

func (f *fakeDeployer) Redeploy(_ context.Context, app string) error {
	if f.block != nil {
		<-f.block
	}
	f.redeployed <- app
	return f.deployErr
}

func (f *fakeDeployer) Destroy(_ context.Context, app string) error {
	if f.deployErr != nil {
		return f.deployErr
	}
	f.destroyed <- app
	return nil
}

// fakeRuntime serves canned stats/logs; err poisons both. resolveInfo/
// resolveErr drive the image-resolve endpoint independently.
type fakeRuntime struct {
	stats       deploy.ContainerStats
	logs        string
	err         error
	resolveInfo deploy.ImageInfo
	resolveErr  error
}

func (f *fakeRuntime) Stats(context.Context, string) (deploy.ContainerStats, error) {
	return f.stats, f.err
}
func (f *fakeRuntime) Logs(context.Context, string, int) (string, error) { return f.logs, f.err }
func (f *fakeRuntime) ResolveImage(context.Context, string) (deploy.ImageInfo, error) {
	return f.resolveInfo, f.resolveErr
}

func newTestServer(st *fakeStore, d *fakeDeployer) *http.ServeMux {
	return newTestServerRT(st, d, &fakeRuntime{})
}

func newTestServerRT(st *fakeStore, d *fakeDeployer, rt *fakeRuntime) *http.ServeMux {
	s := New(st, d, rt)
	s.fastFail = 20 * time.Millisecond // keep the slow-path handoff test fast
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func post(t *testing.T, mux *http.ServeMux, path string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func wantContains(t *testing.T, rec *httptest.ResponseRecorder, substr string) {
	t.Helper()
	if !strings.Contains(rec.Body.String(), substr) {
		t.Fatalf("response does not contain %q:\n%s", substr, rec.Body.String())
	}
}

func TestIndexRendersAppsWithStatus(t *testing.T) {
	st := &fakeStore{
		apps: []deploy.App{{AppSpec: testSpec, ContainerID: "abc123", UpdatedAt: now}},
		latest: map[string]deploy.DeployRecord{
			"whoami": {ID: 3, AppName: "whoami", Status: deploy.StatusLive, StartedAt: now},
		},
	}
	rec := get(t, newTestServer(st, newFakeDeployer()), "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	wantContains(t, rec, "whoami")
	wantContains(t, rec, ">live<")
	wantContains(t, rec, "https://whoami.sparboard.com")
}

func TestUnknownPathIs404(t *testing.T) {
	rec := get(t, newTestServer(&fakeStore{}, newFakeDeployer()), "/nonsense")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAppDetailRendersHistory(t *testing.T) {
	finished := now.Add(8 * time.Second)
	st := &fakeStore{
		apps: []deploy.App{{AppSpec: testSpec, ContainerID: "abc", UpdatedAt: now}},
		deploys: []deploy.DeployRecord{
			{ID: 2, AppName: "whoami", Image: testSpec.Image, Status: deploy.StatusFailed, Error: "pulling: no such image", StartedAt: now, FinishedAt: &finished},
		},
	}
	rec := get(t, newTestServer(st, newFakeDeployer()), "/apps/whoami")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	wantContains(t, rec, "pulling: no such image")
	wantContains(t, rec, ">failed<")
}

func TestAppDetailUnknownAppIs404(t *testing.T) {
	rec := get(t, newTestServer(&fakeStore{}, newFakeDeployer()), "/apps/ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPostWithoutHtmxHeaderIsForbidden(t *testing.T) {
	d := newFakeDeployer()
	st := &fakeStore{apps: []deploy.App{{AppSpec: testSpec}}}
	mux := newTestServer(st, d)

	for _, path := range []string{"/deploy", "/apps/whoami/redeploy", "/apps/whoami/destroy"} {
		rec := post(t, mux, path, url.Values{}, false)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s without HX-Request: status = %d, want 403", path, rec.Code)
		}
	}
	select {
	case spec := <-d.deployed:
		t.Fatalf("deploy ran despite 403: %+v", spec)
	case app := <-d.destroyed:
		t.Fatalf("destroy ran despite 403: %s", app)
	default: // nothing happened — correct
	}
}

func TestDeployFastFailureRendersError(t *testing.T) {
	d := newFakeDeployer()
	d.deployErr = deploy.AppSpec{}.Validate() // a real validation error
	mux := newTestServer(&fakeStore{}, d)

	form := url.Values{"name": {"whoami"}, "image": {testSpec.Image}, "port": {"80"}, "host": {testSpec.Host}}
	rec := post(t, mux, "/deploy", form, true)

	wantContains(t, rec, "flash error")
	<-d.deployed // the deployer did run; the error came from it
}

func TestDeployBadPortFailsBeforeDeployer(t *testing.T) {
	d := newFakeDeployer()
	mux := newTestServer(&fakeStore{}, d)

	form := url.Values{"name": {"x"}, "image": {"img"}, "port": {"eighty"}, "host": {"h"}}
	rec := post(t, mux, "/deploy", form, true)

	wantContains(t, rec, "port must be a number")
	select {
	case spec := <-d.deployed:
		t.Fatalf("deployer ran with unparseable port: %+v", spec)
	default:
	}
}

func TestSlowDeployHandsOffToPolling(t *testing.T) {
	d := newFakeDeployer()
	d.block = make(chan struct{})
	mux := newTestServer(&fakeStore{}, d)

	form := url.Values{"name": {"whoami"}, "image": {testSpec.Image}, "port": {"80"}, "host": {testSpec.Host}}
	rec := post(t, mux, "/deploy", form, true)

	// The response must come back BEFORE the deploy finishes...
	wantContains(t, rec, "started")

	// ...and the deploy must still run to completion in the background.
	close(d.block)
	select {
	case spec := <-d.deployed:
		if !reflect.DeepEqual(spec, testSpec) {
			t.Fatalf("deployed spec = %+v, want %+v", spec, testSpec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background deploy never completed after handoff")
	}
}

// The handler must delegate to Deployer.Redeploy (which reads the spec under
// the app lock) — never read the spec itself and call Deploy.
func TestRedeployDelegatesByName(t *testing.T) {
	d := newFakeDeployer()
	st := &fakeStore{apps: []deploy.App{{AppSpec: testSpec, ContainerID: "abc"}}}
	mux := newTestServer(st, d)

	rec := post(t, mux, "/apps/whoami/redeploy", url.Values{}, true)

	wantContains(t, rec, "redeploy of whoami")
	select {
	case app := <-d.redeployed:
		if app != "whoami" {
			t.Fatalf("redeployed %q, want whoami", app)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("redeploy never reached the deployer")
	}
	select {
	case spec := <-d.deployed:
		t.Fatalf("handler bypassed Redeploy and called Deploy with %+v", spec)
	default:
	}
}

func TestRedeployUnknownAppRendersError(t *testing.T) {
	d := newFakeDeployer()
	d.deployErr = deploy.AppSpec{}.Validate() // any fast error from Redeploy
	rec := post(t, newTestServer(&fakeStore{}, d), "/apps/ghost/redeploy", url.Values{}, true)
	wantContains(t, rec, "flash error")
}

// GET history for a destroyed app must stop the poll (htmx status 286) and
// say so — a 200 with an empty table would render the stale detail page as
// if the app still existed, forever.
func TestHistoryForMissingAppStopsPolling(t *testing.T) {
	rec := get(t, newTestServer(&fakeStore{}, newFakeDeployer()), "/apps/ghost/history")
	if rec.Code != htmxStopPolling {
		t.Fatalf("status = %d, want %d (htmx stop-polling)", rec.Code, htmxStopPolling)
	}
	wantContains(t, rec, "no longer exists")
}

func TestDestroyReachesDeployer(t *testing.T) {
	d := newFakeDeployer()
	st := &fakeStore{apps: []deploy.App{{AppSpec: testSpec}}}
	rec := post(t, newTestServer(st, d), "/apps/whoami/destroy", url.Values{}, true)

	wantContains(t, rec, "whoami destroyed")
	select {
	case app := <-d.destroyed:
		if app != "whoami" {
			t.Fatalf("destroyed %q, want whoami", app)
		}
	default:
		t.Fatal("destroy never reached the deployer")
	}
}

func TestEnvFromLines(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    map[string]string
		wantErr bool
	}{
		{"empty", "", map[string]string{}, false},
		{"blank lines ignored", "\n  \nA=1\n\n", map[string]string{"A": "1"}, false},
		{"value keeps equals", "DSN=postgres://u:p=x@h/db", map[string]string{"DSN": "postgres://u:p=x@h/db"}, false},
		{"crlf tolerated", "A=1\r\nB=2", map[string]string{"A": "1", "B": "2"}, false},
		{"no equals is an error", "JUSTAKEY", nil, true},
		{"empty key is an error", "=value", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := envFromLines(tt.text)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// Env VALUES are secrets: the detail page shows keys only, never values.
func TestAppDetailNeverEchoesEnvValues(t *testing.T) {
	app := deploy.App{AppSpec: testSpec, UpdatedAt: now}
	app.Env = map[string]string{"APP_SECRET": "hunter2-do-not-render"}
	app.UseDB = true
	st := &fakeStore{apps: []deploy.App{app}}

	rec := get(t, newTestServer(st, newFakeDeployer()), "/apps/whoami")

	wantContains(t, rec, "APP_SECRET")
	wantContains(t, rec, "tenant_db_net")
	if strings.Contains(rec.Body.String(), "hunter2-do-not-render") {
		t.Fatal("detail page rendered an env VALUE — secrets leak")
	}
}

func TestStatsFragment(t *testing.T) {
	deployed := deploy.App{AppSpec: testSpec, ContainerID: "abc", UpdatedAt: now}
	never := deploy.App{AppSpec: deploy.AppSpec{Name: "fresh", Image: "img", ContainerPort: 80, Host: "f.x"}}
	st := &fakeStore{apps: []deploy.App{deployed, never}}
	rt := &fakeRuntime{stats: deploy.ContainerStats{CPUPercent: 3.14, MemBytes: 259 << 20, MemLimit: 512 << 20}}
	mux := newTestServerRT(st, newFakeDeployer(), rt)

	rec := get(t, mux, "/apps/whoami/stats")
	wantContains(t, rec, "cpu 3.1%")
	wantContains(t, rec, "259 MiB / 512 MiB")

	rec = get(t, mux, "/apps/fresh/stats")
	wantContains(t, rec, "not deployed yet")

	rec = get(t, mux, "/apps/ghost/stats")
	if rec.Code != htmxStopPolling {
		t.Fatalf("ghost stats status = %d, want %d", rec.Code, htmxStopPolling)
	}

	rt.err = deploy.AppSpec{}.Validate()
	rec = get(t, mux, "/apps/whoami/stats")
	wantContains(t, rec, "stats unavailable")
}

// Log content is untrusted app output — it must render as text, never HTML.
func TestLogsAreEscaped(t *testing.T) {
	st := &fakeStore{apps: []deploy.App{{AppSpec: testSpec, ContainerID: "abc"}}}
	rt := &fakeRuntime{logs: "hello <script>alert(1)</script>"}
	rec := get(t, newTestServerRT(st, newFakeDeployer(), rt), "/apps/whoami/logs")

	wantContains(t, rec, "&lt;script&gt;")
	if strings.Contains(rec.Body.String(), "<script>alert") {
		t.Fatal("log output rendered as raw HTML — XSS via app logs")
	}
}

func TestMetricsExposition(t *testing.T) {
	deployed := deploy.App{AppSpec: testSpec, ContainerID: "abc"}
	never := deploy.App{AppSpec: deploy.AppSpec{Name: "fresh", Image: "img", ContainerPort: 80, Host: "f.x"}}
	st := &fakeStore{apps: []deploy.App{deployed, never}}
	rt := &fakeRuntime{stats: deploy.ContainerStats{CPUPercent: 2.5, MemBytes: 100, MemLimit: 200}}
	rec := get(t, newTestServerRT(st, newFakeDeployer(), rt), "/metrics")

	wantContains(t, rec, `qincloud_app_up{app="whoami"} 1`)
	wantContains(t, rec, `qincloud_app_up{app="fresh"} 0`)
	wantContains(t, rec, `qincloud_app_cpu_percent{app="whoami"} 2.5`)
	wantContains(t, rec, `qincloud_app_memory_bytes{app="whoami"} 100`)
	if strings.Contains(rec.Body.String(), `qincloud_app_memory_bytes{app="fresh"}`) {
		t.Fatal("gauges emitted for an app with no running container")
	}
	if n := strings.Count(rec.Body.String(), "# TYPE qincloud_app_up"); n != 1 {
		t.Fatalf("TYPE header for qincloud_app_up appears %d times, want 1 (repeats are a Prometheus parse error)", n)
	}
}

func TestStatusProjection(t *testing.T) {
	finished := now.Add(9 * time.Second)
	tests := []struct {
		name      string
		rec       deploy.DeployRecord
		inLatest  bool
		wantLabel string
		wantClass string
	}{
		{
			name:      "never deployed",
			inLatest:  false,
			wantLabel: "never deployed",
			wantClass: "muted",
		},
		{
			name:      "live",
			rec:       deploy.DeployRecord{Status: deploy.StatusLive, StartedAt: now, FinishedAt: &finished},
			inLatest:  true,
			wantLabel: "live",
			wantClass: "live",
		},
		{
			name:      "failed",
			rec:       deploy.DeployRecord{Status: deploy.StatusFailed, StartedAt: now, FinishedAt: &finished},
			inLatest:  true,
			wantLabel: "failed",
			wantClass: "failed",
		},
		{
			name:      "in flight",
			rec:       deploy.DeployRecord{Status: deploy.StatusPulling, StartedAt: now.Add(-30 * time.Second)},
			inLatest:  true,
			wantLabel: "pulling…",
			wantClass: "progress",
		},
		{
			name:      "abandoned: non-terminal but far older than any deploy budget",
			rec:       deploy.DeployRecord{Status: deploy.StatusStarting, StartedAt: now.Add(-time.Hour)},
			inLatest:  true,
			wantLabel: "abandoned (starting)",
			wantClass: "failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := deploy.App{AppSpec: testSpec}
			latest := map[string]deploy.DeployRecord{}
			if tt.inLatest {
				tt.rec.AppName = app.Name
				latest[app.Name] = tt.rec
			}
			got := statusFor(app, latest, now)
			if got.Label != tt.wantLabel || got.Class != tt.wantClass {
				t.Fatalf("statusFor = %+v, want {%s %s}", got, tt.wantLabel, tt.wantClass)
			}
		})
	}
}

func TestTook(t *testing.T) {
	finished := now.Add(83 * time.Second)
	tests := []struct {
		name string
		rec  deploy.DeployRecord
		want string
	}{
		{"finished", deploy.DeployRecord{StartedAt: now, FinishedAt: &finished}, "1m23s"},
		{"in flight ticks", deploy.DeployRecord{StartedAt: now.Add(-5 * time.Second)}, "5s…"},
		{"abandoned shows dash", deploy.DeployRecord{StartedAt: now.Add(-time.Hour)}, "—"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := took(tt.rec, now); got != tt.want {
				t.Fatalf("took = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveImageSinglePort(t *testing.T) {
	rt := &fakeRuntime{resolveInfo: deploy.ImageInfo{Ref: "nginx", ExposedPorts: []int{80}, SizeBytes: 50 << 20}}
	rec := post(t, newTestServerRT(&fakeStore{}, newFakeDeployer(), rt), "/images/resolve",
		url.Values{"image": {"nginx"}}, true)
	wantContains(t, rec, "listens on port 80")
	wantContains(t, rec, `name="port"`)
	wantContains(t, rec, `value="80"`)
}

func TestResolveImageMultiPortOffersPicker(t *testing.T) {
	rt := &fakeRuntime{resolveInfo: deploy.ImageInfo{ExposedPorts: []int{80, 3000}}}
	rec := post(t, newTestServerRT(&fakeStore{}, newFakeDeployer(), rt), "/images/resolve",
		url.Values{"image": {"x"}}, true)
	wantContains(t, rec, "<select")
	wantContains(t, rec, ">3000<")
}

func TestResolveImageNoPortAsksForOne(t *testing.T) {
	rt := &fakeRuntime{resolveInfo: deploy.ImageInfo{ExposedPorts: []int{}}}
	rec := post(t, newTestServerRT(&fakeStore{}, newFakeDeployer(), rt), "/images/resolve",
		url.Values{"image": {"x"}}, true)
	wantContains(t, rec, "declares no port")
	wantContains(t, rec, `name="port"`)
}

func TestResolveImageErrorEmitsNoPortField(t *testing.T) {
	rt := &fakeRuntime{resolveErr: deploy.AppSpec{}.Validate()}
	rec := post(t, newTestServerRT(&fakeStore{}, newFakeDeployer(), rt), "/images/resolve",
		url.Values{"image": {"ghcr.io/no/such:v9"}}, true)
	wantContains(t, rec, "couldn't pull")
	if strings.Contains(rec.Body.String(), `name="port"`) {
		t.Fatal("a failed resolve must not emit a port field (submit stays disabled)")
	}
}

func TestResolveImageEmptyRefIsResting(t *testing.T) {
	rec := post(t, newTestServer(&fakeStore{}, newFakeDeployer()), "/images/resolve",
		url.Values{"image": {"  "}}, true)
	wantContains(t, rec, "detected from the image")
}

// A successful deploy must fire the trigger app.js listens on to close the
// modal, and OOB-swap the confirmation onto the page's #flash.
func TestDeploySuccessTriggersModalClose(t *testing.T) {
	d := newFakeDeployer()
	mux := newTestServer(&fakeStore{}, d)
	form := url.Values{"name": {"whoami"}, "image": {testSpec.Image}, "port": {"80"}, "host": {testSpec.Host}}
	rec := post(t, mux, "/deploy", form, true)

	if got := rec.Header().Get("HX-Trigger"); got != "deploy-started" {
		t.Fatalf("HX-Trigger = %q, want deploy-started", got)
	}
	wantContains(t, rec, `id="flash"`)
	wantContains(t, rec, "hx-swap-oob")
	<-d.deployed
}
