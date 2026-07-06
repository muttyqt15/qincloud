// contract.go — the shared contract of the control plane: the app spec, the
// deploy status machine, and the four capabilities a deploy needs (container
// runtime, edge router, state store, cross-process app lock). Interfaces live
// here, consumer-side; implementations are in internal/dockerx,
// internal/caddyapi, internal/store, and (for Locker) cmd/controld.
package deploy

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// AppSpec is everything controld needs to run and route one app.
type AppSpec struct {
	Name          string            // [a-z0-9-], <=32 chars; container is named qc-<name>-<deployID>
	Image         string            // full image ref, e.g. traefik/whoami:v1.10
	ContainerPort int               // port the app listens on inside the container
	Host          string            // hostname Caddy routes to this app, e.g. whoami.sparboard.com
	Env           map[string]string // container environment; values may be secrets — render KEYS only
	UseDB         bool              // attach tenant_db_net (shared Postgres reachable; redis is NOT on it)
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate rejects a spec that would produce a broken deploy. Fail here,
// loud, not three layers down in the Docker API.
func (s AppSpec) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("app name %q: must match %s", s.Name, nameRe)
	}
	if s.Image == "" {
		return fmt.Errorf("app %s: image is required", s.Name)
	}
	if s.ContainerPort < 1 || s.ContainerPort > 65535 {
		return fmt.Errorf("app %s: container port %d out of range", s.Name, s.ContainerPort)
	}
	if s.Host == "" {
		return fmt.Errorf("app %s: host is required", s.Name)
	}
	for k := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("app %s: env key %q: must match %s", s.Name, k, envKeyRe)
		}
	}
	return nil
}

// Status is the deploy state machine. Linear happy path, one terminal
// failure state; every transition is persisted before the step runs.
//
//	pending → pulling → starting → routing → live
//	   └────────┴──────────┴─────────┴──→ failed (with error)
type Status string

const (
	StatusPending  Status = "pending"
	StatusPulling  Status = "pulling"
	StatusStarting Status = "starting"
	StatusRouting  Status = "routing"
	StatusLive     Status = "live"
	StatusFailed   Status = "failed"
)

// Docker runs app containers on app_net.
type Docker interface {
	// Pull pulls the image via the host daemon.
	Pull(ctx context.Context, image string) error
	// StartApp creates and starts container qc-<app>-<deployID> on app_net,
	// labeled qincloud.app=<app>, and returns its container ID.
	StartApp(ctx context.Context, spec AppSpec, deployID int64) (containerID string, err error)
	// WaitReady blocks until the container reports healthy — or, for images
	// without a healthcheck, until it has stayed running for a grace period.
	WaitReady(ctx context.Context, containerID string, timeout time.Duration) error
	// RemoveAppExcept stops and removes every qincloud.app=<app> container
	// except keepID (pass "" to remove all). Idempotent; absent is not an error.
	RemoveAppExcept(ctx context.Context, app, keepID string) error
	// RemoveContainer stops and removes one container by name or ID.
	// Idempotent; absent is not an error. Failure paths use this to retire
	// exactly the container their own deploy created — a sweep keyed on a
	// remembered "previous live" ID would remove the routed container when
	// that memory is stale (see deploy.go).
	RemoveContainer(ctx context.Context, nameOrID string) error
}

// ContainerStats is one resource snapshot, shaped for display and export.
// Produced by dockerx, consumed by the dashboard (its Runtime interface) —
// it lives here as the shared domain type, deliberately NOT as methods on
// Docker: the deploy state machine never reads stats, and its contract
// should not grow observability the fakes would have to stub.
type ContainerStats struct {
	CPUPercent float64 // of one core; can exceed 100 on multi-core usage
	MemBytes   int64   // working set (usage minus inactive file cache)
	MemLimit   int64   // the container's memory cap
}

// ImageInfo is what pulling+inspecting an image tells the dashboard so the
// operator does not have to hand-supply a port: the ports the image declares
// it listens on (EXPOSE). Same rationale as ContainerStats for living here
// and off the Docker interface — the deploy state machine is handed a
// concrete port and never inspects.
type ImageInfo struct {
	Ref          string // the resolved reference (as requested)
	ExposedPorts []int  // TCP ports the image EXPOSEs, ascending; empty if none declared
	SizeBytes    int64  // on-disk size of the pulled image
}

// Router programs the edge (Caddy admin API over its unix socket).
type Router interface {
	// UpsertRoute makes requests for host reverse-proxy to dial
	// (e.g. "qc-whoami-3:80"), replacing any existing route for app.
	UpsertRoute(ctx context.Context, app, host, dial string) error
	// DeleteRoute removes app's route. Idempotent; absent is not an error.
	DeleteRoute(ctx context.Context, app string) error
}

// Store persists apps and deploy history (Postgres via pgx).
type Store interface {
	// Init applies the embedded schema. Idempotent.
	Init(ctx context.Context) error
	UpsertApp(ctx context.Context, spec AppSpec) error
	// CreateDeploy inserts a deploy row in StatusPending and returns its id.
	CreateDeploy(ctx context.Context, app, image string) (int64, error)
	// SetDeployStatus records a transition; errMsg only for StatusFailed.
	SetDeployStatus(ctx context.Context, id int64, status Status, errMsg string) error
	// SetLiveContainer records which container currently serves the app.
	SetLiveContainer(ctx context.Context, app, containerID string) error
	GetApp(ctx context.Context, app string) (*App, error)
	ListApps(ctx context.Context) ([]App, error)
	DeleteApp(ctx context.Context, app string) error
}

// Locker serializes deploy/destroy per app across processes. Every CLI
// invocation is its own `docker exec` process, so an in-process mutex cannot
// stop two concurrent deploys of the same app from interleaving — each would
// treat the other's container as garbage, and the loser's cleanup removes the
// winner's routed container (502s while both deploy rows read live). The lock
// must live in shared state; the Postgres advisory-lock implementation is in
// cmd/controld, next to the rest of the wiring.
type Locker interface {
	// AcquireAppLock takes an exclusive cross-process lock on app, failing
	// fast (not blocking) when another deploy/destroy already holds it. The
	// returned release func must be called when the operation ends; the lock
	// must also release on its own if the holding process dies.
	AcquireAppLock(ctx context.Context, app string) (release func(), err error)
}

// ContainerName is the single naming convention for app containers:
// dockerx.StartApp names containers with it, and the router dials by it
// (docker DNS resolves container names on app_net).
func ContainerName(app string, deployID int64) string {
	return fmt.Sprintf("qc-%s-%d", app, deployID)
}

// App is an AppSpec plus its runtime state, as read back from the store.
type App struct {
	AppSpec
	ContainerID string // current live container; "" if never deployed
	UpdatedAt   time.Time
}

// DeployRecord is one row of deploy history, as read back from the store.
// The Deployer only writes these (via Store); readers (CLI, dashboard) get
// them through store methods outside the deploy.Store interface.
type DeployRecord struct {
	ID         int64
	AppName    string
	Image      string
	Status     Status
	Error      string // non-empty only for StatusFailed
	StartedAt  time.Time
	FinishedAt *time.Time // nil until the deploy reaches live/failed
}
