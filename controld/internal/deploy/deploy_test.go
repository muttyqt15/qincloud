// deploy_test.go — state machine tests against in-memory fakes. Asserts
// observable end state (which containers exist, what's routed, what the
// store recorded), not call counts. The property under test: a failed
// deploy never takes down the running app.
package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- fakes -----------------------------------------------------------------

type fakeDocker struct {
	pullErr     error
	startErr    error
	readyErr    error
	onWaitReady func()            // runs inside WaitReady — lets a test kill the deploy ctx mid-wait
	containers  map[string]string // containerID → app
	nextID      int
}

func newFakeDocker() *fakeDocker { return &fakeDocker{containers: map[string]string{}} }

func (f *fakeDocker) Pull(ctx context.Context, image string) error { return f.pullErr }

func (f *fakeDocker) StartApp(ctx context.Context, spec AppSpec, deployID int64) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	f.nextID++
	cid := ContainerName(spec.Name, deployID) // fake IDs = names, close enough
	f.containers[cid] = spec.Name
	return cid, nil
}

func (f *fakeDocker) WaitReady(ctx context.Context, cid string, timeout time.Duration) error {
	if f.onWaitReady != nil {
		f.onWaitReady()
	}
	return f.readyErr
}

func (f *fakeDocker) RemoveAppExcept(ctx context.Context, app, keepID string) error {
	// Honor the context like the real SDK adapter would: cleanup on a dead
	// deploy context must be detected by these tests, not silently absorbed.
	if err := ctx.Err(); err != nil {
		return err
	}
	for cid, a := range f.containers {
		if a == app && cid != keepID {
			delete(f.containers, cid)
		}
	}
	return nil
}

func (f *fakeDocker) RemoveContainer(ctx context.Context, nameOrID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(f.containers, nameOrID) // fake IDs = names; absent is not an error
	return nil
}

type fakeRouter struct {
	upsertErr error
	onUpsert  func()            // runs after a successful upsert — lets a test kill the deploy ctx right after the route switch
	routes    map[string]string // app → dial
}

func newFakeRouter() *fakeRouter { return &fakeRouter{routes: map[string]string{}} }

func (f *fakeRouter) UpsertRoute(ctx context.Context, app, host, dial string) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.routes[app] = dial
	if f.onUpsert != nil {
		f.onUpsert()
	}
	return nil
}

func (f *fakeRouter) DeleteRoute(ctx context.Context, app string) error {
	delete(f.routes, app)
	return nil
}

type fakeStore struct {
	apps        map[string]*App
	transitions []Status
	lastErrMsg  string
	nextDeploy  int64
	liveErr     error // returned by SetLiveContainer when set
}

func newFakeStore() *fakeStore { return &fakeStore{apps: map[string]*App{}} }

func (f *fakeStore) Init(ctx context.Context) error { return nil }

func (f *fakeStore) UpsertApp(ctx context.Context, spec AppSpec) error {
	prev := f.apps[spec.Name]
	app := &App{AppSpec: spec}
	if prev != nil {
		app.ContainerID = prev.ContainerID
	}
	f.apps[spec.Name] = app
	return nil
}

func (f *fakeStore) CreateDeploy(ctx context.Context, app, image string) (int64, error) {
	f.nextDeploy++
	f.transitions = append(f.transitions, StatusPending)
	return f.nextDeploy, nil
}

func (f *fakeStore) SetDeployStatus(ctx context.Context, id int64, s Status, errMsg string) error {
	// Honor the context like the real pgx adapter would — recording a failure
	// on a dead deploy context must fail here, so the tests can prove the
	// state machine detaches for it.
	if err := ctx.Err(); err != nil {
		return err
	}
	f.transitions = append(f.transitions, s)
	if s == StatusFailed {
		f.lastErrMsg = errMsg
	}
	return nil
}

func (f *fakeStore) SetLiveContainer(ctx context.Context, app, cid string) error {
	// Honor the context like the real pgx adapter would: this write runs
	// after the route switch, and the tests must prove it survives the deploy
	// context dying right there.
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.liveErr != nil {
		return f.liveErr
	}
	f.apps[app].ContainerID = cid
	return nil
}

func (f *fakeStore) GetApp(ctx context.Context, app string) (*App, error) {
	return f.apps[app], nil // nil, nil when absent — matches store contract
}

func (f *fakeStore) ListApps(ctx context.Context) ([]App, error) { return nil, nil }

func (f *fakeStore) DeleteApp(ctx context.Context, app string) error {
	delete(f.apps, app)
	return nil
}

// fakeLocker mimics the try-lock semantics of the pg advisory lock: acquiring
// a held app fails fast, releasing clears it. Pre-populate held to simulate
// another process mid-deploy.
type fakeLocker struct {
	held map[string]bool
}

func newFakeLocker() *fakeLocker { return &fakeLocker{held: map[string]bool{}} }

func (f *fakeLocker) AcquireAppLock(ctx context.Context, app string) (func(), error) {
	if f.held[app] {
		return nil, errors.New("another deploy or destroy of " + app + " is already running")
	}
	f.held[app] = true
	return func() { delete(f.held, app) }, nil
}

// --- helpers ---------------------------------------------------------------

func spec() AppSpec {
	return AppSpec{Name: "whoami", Image: "traefik/whoami:v1.10", ContainerPort: 80, Host: "whoami.example.com"}
}

func setup() (*fakeDocker, *fakeRouter, *fakeStore, *fakeLocker, *Deployer) {
	dk, rt, st, lk := newFakeDocker(), newFakeRouter(), newFakeStore(), newFakeLocker()
	return dk, rt, st, lk, New(dk, rt, st, lk)
}

func wantTransitions(t *testing.T, st *fakeStore, want ...Status) {
	t.Helper()
	if len(st.transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", st.transitions, want)
	}
	for i := range want {
		if st.transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", st.transitions, want)
		}
	}
}

// --- tests -----------------------------------------------------------------

func TestDeployHappyPath(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	wantTransitions(t, st, StatusPending, StatusPulling, StatusStarting, StatusRouting, StatusLive)
	if dial := rt.routes["whoami"]; dial != "qc-whoami-1:80" {
		t.Fatalf("routed dial = %q, want qc-whoami-1:80", dial)
	}
	if len(dk.containers) != 1 {
		t.Fatalf("containers = %v, want exactly the new one", dk.containers)
	}
	if st.apps["whoami"].ContainerID == "" {
		t.Fatal("live container not recorded")
	}
}

func TestRedeployRetiresOldContainer(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	first := st.apps["whoami"].ContainerID
	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	second := st.apps["whoami"].ContainerID
	if first == second {
		t.Fatal("second deploy did not produce a new container")
	}
	if _, oldAlive := dk.containers[first]; oldAlive {
		t.Fatal("old container was not retired")
	}
	if rt.routes["whoami"] != second+":80" {
		t.Fatalf("route %q does not point at new container %q", rt.routes["whoami"], second)
	}
}

func TestNotReadyKeepsOldContainerServing(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	old := st.apps["whoami"].ContainerID
	oldDial := rt.routes["whoami"]

	dk.readyErr = errors.New("healthcheck never went green")
	err := d.Deploy(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("err = %v, want starting-step failure", err)
	}

	if _, alive := dk.containers[old]; !alive {
		t.Fatal("old container was removed on failed deploy")
	}
	if len(dk.containers) != 1 {
		t.Fatalf("containers = %v, want only the old one (new retired)", dk.containers)
	}
	if rt.routes["whoami"] != oldDial {
		t.Fatalf("route changed to %q on failed deploy, want %q", rt.routes["whoami"], oldDial)
	}
	if st.transitions[len(st.transitions)-1] != StatusFailed {
		t.Fatalf("last transition = %v, want failed", st.transitions[len(st.transitions)-1])
	}
	if !strings.Contains(st.lastErrMsg, "healthcheck") {
		t.Fatalf("failure cause not recorded, got %q", st.lastErrMsg)
	}
}

func TestRouteFailureKeepsOldContainerServing(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	old := st.apps["whoami"].ContainerID

	rt.upsertErr = errors.New("caddy admin socket gone")
	err := d.Deploy(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "routing") {
		t.Fatalf("err = %v, want routing-step failure", err)
	}
	if _, alive := dk.containers[old]; !alive {
		t.Fatal("old container was removed on failed routing")
	}
	if len(dk.containers) != 1 {
		t.Fatalf("containers = %v, want only the old one", dk.containers)
	}
	if st.apps["whoami"].ContainerID != old {
		t.Fatal("live container record changed on failed deploy")
	}
}

func TestFirstDeployFailureLeavesNothingBehind(t *testing.T) {
	dk, _, st, _, d := setup()

	dk.readyErr = errors.New("crashloop")
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("want error")
	}
	if len(dk.containers) != 0 {
		t.Fatalf("containers = %v, want none after first-deploy failure", dk.containers)
	}
	if st.apps["whoami"].ContainerID != "" {
		t.Fatal("no container should be recorded live")
	}
}

func TestPullFailureRecordsFailedBeforeStartingAnything(t *testing.T) {
	dk, rt, st, _, d := setup()

	dk.pullErr = errors.New("no such image")
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("want error")
	}
	wantTransitions(t, st, StatusPending, StatusPulling, StatusFailed)
	if len(dk.containers) != 0 || len(rt.routes) != 0 {
		t.Fatal("nothing should have started or routed")
	}
}

func TestDestroyRemovesRouteContainersRecord(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if err := d.Destroy(context.Background(), "whoami"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if len(rt.routes) != 0 || len(dk.containers) != 0 {
		t.Fatalf("routes=%v containers=%v, want both empty", rt.routes, dk.containers)
	}
	if st.apps["whoami"] != nil {
		t.Fatal("app record not deleted")
	}
}

// Two deploys of the same app must not interleave: the loser's cleanup would
// remove the winner's routed container. The second invocation fails fast,
// before it creates a deploy row or starts anything.
func TestConcurrentDeploySameAppFailsFast(t *testing.T) {
	dk, rt, st, lk, d := setup()

	lk.held["whoami"] = true // another process is mid-deploy
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("want error while another deploy holds the app lock")
	}
	if len(st.transitions) != 0 {
		t.Fatalf("transitions = %v, want none — no deploy row while locked out", st.transitions)
	}
	if len(dk.containers) != 0 || len(rt.routes) != 0 {
		t.Fatalf("containers=%v routes=%v, want nothing touched", dk.containers, rt.routes)
	}
}

func TestDestroyFailsFastWhileAppLocked(t *testing.T) {
	dk, rt, st, lk, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	lk.held["whoami"] = true // simulate a deploy in flight elsewhere
	if err := d.Destroy(context.Background(), "whoami"); err == nil {
		t.Fatal("want error while another operation holds the app lock")
	}
	if len(rt.routes) != 1 || len(dk.containers) != 1 || st.apps["whoami"] == nil {
		t.Fatalf("routes=%v containers=%v, want the live app untouched", rt.routes, dk.containers)
	}
}

// The lock must release when the operation ends — success or failure — or the
// app stays undeployable until the process exits.
func TestAppLockReleasedAfterSuccessAndFailure(t *testing.T) {
	dk, _, _, lk, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(lk.held) != 0 {
		t.Fatalf("locks still held after successful deploy: %v", lk.held)
	}

	dk.readyErr = errors.New("crashloop")
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("want error")
	}
	if len(lk.held) != 0 {
		t.Fatalf("locks still held after failed deploy: %v", lk.held)
	}
}

// SetLiveContainer failing after the route switch must fail the deploy loud
// (the record now lies about which container is live — the exact fact failure
// cleanup trusts) and must remove NOTHING: the new container is routed, the
// old one is the safety net, and the next successful deploy re-records and
// sweeps both into shape.
func TestLiveRecordFailureFailsLoudKeepingBothContainers(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	old := st.apps["whoami"].ContainerID

	st.liveErr = errors.New("pg blip")
	err := d.Deploy(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "record live container") {
		t.Fatalf("err = %v, want loud record-live failure", err)
	}
	if len(dk.containers) != 2 {
		t.Fatalf("containers = %v, want both kept (new is routed, old is the fallback)", dk.containers)
	}
	if rt.routes["whoami"] != "qc-whoami-2:80" {
		t.Fatalf("route = %q, want the already-switched new dial", rt.routes["whoami"])
	}
	if st.apps["whoami"].ContainerID != old {
		t.Fatalf("record = %q — must still hold the old container when the write failed", st.apps["whoami"].ContainerID)
	}
	if st.transitions[len(st.transitions)-1] != StatusFailed {
		t.Fatalf("last transition = %v, want failed", st.transitions[len(st.transitions)-1])
	}
}

// A step often fails BECAUSE the deploy context died (CLI budget, Ctrl-C).
// Cleanup of the new container and the StatusFailed write must survive that
// death, or the container leaks (restart=unless-stopped) and the deploys row
// sticks mid-state.
func TestFailureCleanupAndRecordSurviveDeadDeployContext(t *testing.T) {
	dk, _, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	old := st.apps["whoami"].ContainerID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dk.onWaitReady = cancel // the deploy budget expires mid-wait…
	dk.readyErr = context.DeadlineExceeded

	err := d.Deploy(ctx, spec())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "recording failure also failed") {
		t.Fatalf("StatusFailed write died with the deploy context: %v", err)
	}
	if len(dk.containers) != 1 {
		t.Fatalf("containers = %v, want the new one cleaned up despite the dead context", dk.containers)
	}
	if _, alive := dk.containers[old]; !alive {
		t.Fatal("old container was removed")
	}
	if last := st.transitions[len(st.transitions)-1]; last != StatusFailed {
		t.Fatalf("last transition = %v, want failed recorded on a detached context", last)
	}
}

// A SetLiveContainer failure leaves the record stale: the routed container is
// deploy 2's, but container_id still names deploy 1's. A LATER failed deploy
// must not trust that stale record — its cleanup may remove only what IT
// created, never the routed container. (Found by the M5 review: the old
// "remove all except recorded-live" sweep caused hard downtime here.)
func TestFailedDeployAfterStaleLiveRecordKeepsRoutedContainer(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("deploy 1: %v", err)
	}
	st.liveErr = errors.New("pg blip")
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("deploy 2: want record-live failure")
	}
	st.liveErr = nil
	// State now: route → qc-whoami-2, record still says qc-whoami-1. Deploy 3
	// fails at readiness.
	dk.readyErr = errors.New("crashloop")
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("deploy 3: want readiness failure")
	}

	if rt.routes["whoami"] != "qc-whoami-2:80" {
		t.Fatalf("route = %q, want the serving container untouched", rt.routes["whoami"])
	}
	if _, alive := dk.containers["qc-whoami-2"]; !alive {
		t.Fatal("the ROUTED container was removed by a failed deploy's cleanup — hard downtime")
	}
	if _, gone := dk.containers["qc-whoami-3"]; gone {
		t.Fatal("deploy 3's own failed container was not retired")
	}
}

// The deploy context dying immediately after the route switch must not fail
// the deploy: the app is serving, so recording the live container, retiring
// the old one, and stamping StatusLive must all run detached.
func TestPostRouteBookkeepingSurvivesDeadDeployContext(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.onUpsert = cancel // the deploy budget expires right after the route switch…

	if err := d.Deploy(ctx, spec()); err != nil {
		t.Fatalf("deploy failed although the app is routed and serving: %v", err)
	}
	if st.apps["whoami"].ContainerID != "qc-whoami-2" {
		t.Fatalf("record = %q, want the routed container recorded live", st.apps["whoami"].ContainerID)
	}
	if len(dk.containers) != 1 {
		t.Fatalf("containers = %v, want the old one retired despite the dead context", dk.containers)
	}
	if last := st.transitions[len(st.transitions)-1]; last != StatusLive {
		t.Fatalf("last transition = %v, want live recorded on a detached context", last)
	}
}

// Redeploy reads the spec under the app lock. Its contract: absent app is an
// error, and the spec deployed is whatever the store holds at lock time.
func TestRedeployUsesCurrentStoredSpec(t *testing.T) {
	dk, rt, st, _, d := setup()

	if err := d.Deploy(context.Background(), spec()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	st.apps["whoami"].Image = "traefik/whoami:v1.11" // spec changed since (e.g. CLI deploy elsewhere)

	if err := d.Redeploy(context.Background(), "whoami"); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if got := st.apps["whoami"].Image; got != "traefik/whoami:v1.11" {
		t.Fatalf("stored image = %q, want the current spec preserved", got)
	}
	if rt.routes["whoami"] != "qc-whoami-2:80" {
		t.Fatalf("route = %q, want the redeployed container", rt.routes["whoami"])
	}
	if _, oldAlive := dk.containers["qc-whoami-1"]; oldAlive {
		t.Fatal("old container was not retired by redeploy")
	}
}

func TestRedeployAbsentAppFailsWithoutSideEffects(t *testing.T) {
	dk, rt, st, lk, d := setup()

	err := d.Redeploy(context.Background(), "ghost")
	if err == nil || !strings.Contains(err.Error(), "no such app") {
		t.Fatalf("err = %v, want no-such-app", err)
	}
	if len(st.transitions) != 0 || len(dk.containers) != 0 || len(rt.routes) != 0 {
		t.Fatal("redeploy of an absent app must touch nothing")
	}
	if len(lk.held) != 0 {
		t.Fatalf("lock still held: %v", lk.held)
	}
}

func TestRedeployFailsFastWhileAppLocked(t *testing.T) {
	_, _, st, lk, d := setup()

	lk.held["whoami"] = true // a deploy or destroy is in flight elsewhere
	if err := d.Redeploy(context.Background(), "whoami"); err == nil {
		t.Fatal("want error while another operation holds the app lock")
	}
	if len(st.transitions) != 0 {
		t.Fatalf("transitions = %v, want none", st.transitions)
	}
}

func TestValidateRejectsBrokenSpecs(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AppSpec)
	}{
		{"empty name", func(s *AppSpec) { s.Name = "" }},
		{"uppercase name", func(s *AppSpec) { s.Name = "Whoami" }},
		{"name too long", func(s *AppSpec) { s.Name = strings.Repeat("a", 33) }},
		{"empty image", func(s *AppSpec) { s.Image = "" }},
		{"port zero", func(s *AppSpec) { s.ContainerPort = 0 }},
		{"port too big", func(s *AppSpec) { s.ContainerPort = 70000 }},
		{"empty host", func(s *AppSpec) { s.Host = "" }},
		{"env key with dash", func(s *AppSpec) { s.Env = map[string]string{"BAD-KEY": "x"} }},
		{"env key starting with digit", func(s *AppSpec) { s.Env = map[string]string{"1KEY": "x"} }},
		{"empty env key", func(s *AppSpec) { s.Env = map[string]string{"": "x"} }},
	}
	for _, tc := range cases {
		s := spec()
		tc.mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: validated, want error", tc.name)
		}
	}
	if err := spec().Validate(); err != nil {
		t.Errorf("valid spec rejected: %v", err)
	}
	withEnv := spec()
	withEnv.Env = map[string]string{"DATABASE_URL": "postgresql://x", "APP_SECRET": "s3cr3t=with=equals"}
	withEnv.UseDB = true
	if err := withEnv.Validate(); err != nil {
		t.Errorf("valid env spec rejected: %v", err)
	}
}
