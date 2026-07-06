// store_test.go — real-Postgres integration test for the SQL adapter, gated
// behind CONTROLD_TEST_DSN (skipped when unset so plain `go test ./...` stays
// green without a DB). Point it at a THROWAWAY database — the test drops and
// recreates the controld tables. No pgx fakes: faking the driver this
// adapter exists to wrap would test nothing.
package store

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"qincloud/controld/internal/deploy"
)

// testStore connects, wipes the controld tables, and applies the schema
// twice (Init must be idempotent) so every run starts from a known state.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("CONTROLD_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLD_TEST_DSN not set; skipping real-DB integration test")
	}
	ctx := context.Background()
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS deploys, apps`); err != nil {
		t.Fatalf("drop tables: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("second init (must be idempotent): %v", err)
	}
	return s
}

func testSpec() deploy.AppSpec {
	return deploy.AppSpec{Name: "whoami", Image: "traefik/whoami:v1.10", ContainerPort: 80, Host: "whoami.example.com"}
}

// TestConnectRejectsBadDSN needs no database: a malformed DSN must fail at
// Connect (startup), never at the first query. Parse-only, no dialing.
func TestConnectRejectsBadDSN(t *testing.T) {
	if _, err := Connect(context.Background(), "definitely-not-a-dsn"); err == nil {
		t.Fatal("Connect accepted a malformed DSN, want error at startup")
	}
}

func TestStoreLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Absent app is (nil, nil) — the contract the state machine relies on to
	// tell "first deploy" apart from a store failure.
	if app, err := s.GetApp(ctx, "whoami"); err != nil || app != nil {
		t.Fatalf("GetApp(absent) = %+v, %v; want nil, nil", app, err)
	}

	spec := testSpec()
	if err := s.UpsertApp(ctx, spec); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	app, err := s.GetApp(ctx, "whoami")
	if err != nil || app == nil {
		t.Fatalf("GetApp after upsert = %+v, %v", app, err)
	}
	if !reflect.DeepEqual(app.AppSpec, spec) {
		t.Fatalf("stored spec %+v, want %+v", app.AppSpec, spec)
	}
	if app.ContainerID != "" {
		t.Fatalf("fresh app has container_id %q, want \"\"", app.ContainerID)
	}

	id, err := s.CreateDeploy(ctx, "whoami", spec.Image)
	if err != nil {
		t.Fatalf("create deploy: %v", err)
	}
	assertDeploy(t, s, id, "pending", "", false)

	// Mid-flight transition must not stamp finished_at.
	if err := s.SetDeployStatus(ctx, id, deploy.StatusPulling, ""); err != nil {
		t.Fatalf("set status pulling: %v", err)
	}
	assertDeploy(t, s, id, "pulling", "", false)

	if err := s.SetLiveContainer(ctx, "whoami", "cid-1"); err != nil {
		t.Fatalf("set live container: %v", err)
	}

	// THE invariant this adapter must hold: re-upserting the spec (start of
	// every redeploy) updates image/port/host but must NOT clobber the live
	// container_id — the state machine reads it back mid-deploy to know
	// which container to keep serving on failure.
	respec := spec
	respec.Image = "traefik/whoami:v1.11"
	if err := s.UpsertApp(ctx, respec); err != nil {
		t.Fatalf("re-upsert app: %v", err)
	}
	app, err = s.GetApp(ctx, "whoami")
	if err != nil || app == nil {
		t.Fatalf("GetApp after re-upsert = %+v, %v", app, err)
	}
	if app.Image != respec.Image {
		t.Fatalf("image = %q, want %q", app.Image, respec.Image)
	}
	if app.ContainerID != "cid-1" {
		t.Fatalf("re-upsert clobbered container_id: %q, want cid-1", app.ContainerID)
	}

	// Terminal states stamp finished_at; failed also records the cause.
	if err := s.SetDeployStatus(ctx, id, deploy.StatusLive, ""); err != nil {
		t.Fatalf("set status live: %v", err)
	}
	assertDeploy(t, s, id, "live", "", true)

	failedID, err := s.CreateDeploy(ctx, "whoami", respec.Image)
	if err != nil {
		t.Fatalf("create second deploy: %v", err)
	}
	if err := s.SetDeployStatus(ctx, failedID, deploy.StatusFailed, "pulling: no such image"); err != nil {
		t.Fatalf("set status failed: %v", err)
	}
	assertDeploy(t, s, failedID, "failed", "pulling: no such image", true)

	// ListApps orders by name.
	if err := s.UpsertApp(ctx, deploy.AppSpec{Name: "abacus", Image: "abacus:1", ContainerPort: 8080, Host: "abacus.example.com"}); err != nil {
		t.Fatalf("upsert second app: %v", err)
	}
	apps, err := s.ListApps(ctx)
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "abacus" || apps[1].Name != "whoami" {
		t.Fatalf("ListApps = %+v, want [abacus whoami]", apps)
	}

	// DeleteApp removes the record and cascades its deploy history.
	if err := s.DeleteApp(ctx, "whoami"); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	if app, err := s.GetApp(ctx, "whoami"); err != nil || app != nil {
		t.Fatalf("GetApp after delete = %+v, %v; want nil, nil", app, err)
	}
	var deploys int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deploys WHERE app_name = 'whoami'`).Scan(&deploys); err != nil {
		t.Fatalf("count deploys: %v", err)
	}
	if deploys != 0 {
		t.Fatalf("%d deploy rows survived DeleteApp, want cascade to 0", deploys)
	}
}

// TestHostUniqueness — one host routes to exactly one app. A second app
// claiming a routed host must fail loud and name the holder (never silently
// shadow it at the edge), a redeploy of the holder itself must not
// false-positive, and Init must retrofit the unique index onto a database
// created before the index existed (the IF NOT EXISTS migration path).
func TestHostUniqueness(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	spec := testSpec()
	if err := s.UpsertApp(ctx, spec); err != nil {
		t.Fatalf("upsert app: %v", err)
	}

	// Another app claiming the same host: rejected, holder named.
	blog := deploy.AppSpec{Name: "blog", Image: "blog:1", ContainerPort: 8080, Host: spec.Host}
	err := s.UpsertApp(ctx, blog)
	if err == nil {
		t.Fatal("UpsertApp with an already-routed host returned nil, want error")
	}
	for _, want := range []string{spec.Host, spec.Name} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("host-conflict error %q does not name %q", err, want)
		}
	}
	// The rejected upsert must leave no row behind.
	if app, err := s.GetApp(ctx, "blog"); err != nil || app != nil {
		t.Fatalf("GetApp(blog) after rejected upsert = %+v, %v; want nil, nil", app, err)
	}

	// Redeploying the holder with its own host is not a conflict.
	if err := s.UpsertApp(ctx, spec); err != nil {
		t.Fatalf("re-upsert of the holding app: %v", err)
	}

	// Moving the holder off the host frees it for another app.
	moved := spec
	moved.Host = "moved.example.com"
	if err := s.UpsertApp(ctx, moved); err != nil {
		t.Fatalf("move holder to a new host: %v", err)
	}
	if err := s.UpsertApp(ctx, blog); err != nil {
		t.Fatalf("upsert blog after host was freed: %v", err)
	}

	// Migration path: a table that predates the index gets it on the next
	// Init (every startup re-applies the schema), not only on fresh installs.
	if _, err := s.pool.Exec(ctx, `DROP INDEX apps_host_key`); err != nil {
		t.Fatalf("drop index to simulate a pre-constraint table: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-init on a table without the index: %v", err)
	}
	shadow := deploy.AppSpec{Name: "shadow", Image: "shadow:1", ContainerPort: 80, Host: blog.Host}
	if err := s.UpsertApp(ctx, shadow); err == nil {
		t.Fatal("Init did not restore the host unique index on an existing table")
	}
}

func TestWritesToMissingRowsFailLoud(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.SetDeployStatus(ctx, 999999, deploy.StatusLive, ""); err == nil {
		t.Fatal("SetDeployStatus on missing deploy returned nil, want error")
	}
	if err := s.SetLiveContainer(ctx, "ghost", "cid-1"); err == nil {
		t.Fatal("SetLiveContainer on missing app returned nil, want error")
	}
	if err := s.DeleteApp(ctx, "ghost"); err == nil {
		t.Fatal("DeleteApp on missing app returned nil, want error")
	}
}

// assertDeploy checks a deploy row's status/error and whether finished_at is
// set, straight from the table — the adapter's observable output is the rows
// it writes.
func assertDeploy(t *testing.T, s *Store, id int64, status, errMsg string, finished bool) {
	t.Helper()
	var gotStatus, gotErr string
	var finishedAt *time.Time
	row := s.pool.QueryRow(context.Background(),
		`SELECT status, error, finished_at FROM deploys WHERE id = $1`, id)
	if err := row.Scan(&gotStatus, &gotErr, &finishedAt); err != nil {
		t.Fatalf("read deploy %d: %v", id, err)
	}
	if gotStatus != status || gotErr != errMsg {
		t.Fatalf("deploy %d = (%q, %q), want (%q, %q)", id, gotStatus, gotErr, status, errMsg)
	}
	if finished && finishedAt == nil {
		t.Fatalf("deploy %d reached %s but finished_at is NULL", id, status)
	}
	if !finished && finishedAt != nil {
		t.Fatalf("deploy %d is mid-flight (%s) but finished_at = %v", id, status, *finishedAt)
	}
}
