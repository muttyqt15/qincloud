// deploy.go — the deploy state machine. One linear path:
//
//	validate → lock app → record → pull → start+wait-ready → route → record live → retire old
//
// The old container keeps serving until the new one is ready AND routed, so a
// failed deploy never takes the app down: any step failure removes exactly
// the container this deploy created (by name), marks the deploy failed, and
// leaves the previous state intact. Cleanup, failure recording, and the
// post-route bookkeeping all run on contexts detached from the deploy's,
// because the deploy context's death is itself a common failure cause.
package deploy

import (
	"context"
	"fmt"
	"time"
)

const (
	// ReadyTimeout bounds how long a new container may take to become ready.
	ReadyTimeout = 60 * time.Second

	// cleanupTimeout is the fresh budget for best-effort work that runs after
	// a step has already failed (retiring the new container, recording
	// StatusFailed) — detached from the deploy context, which may be the very
	// thing that just expired. See failDeploy / cleanupKeeping.
	cleanupTimeout = 30 * time.Second
)

type Deployer struct {
	docker Docker
	router Router
	store  Store
	locker Locker
}

func New(docker Docker, router Router, store Store, locker Locker) *Deployer {
	return &Deployer{docker: docker, router: router, store: store, locker: locker}
}

// Deploy runs one deploy to completion. Returns nil only when the app is
// live behind the edge and the deploy is recorded as such.
func (d *Deployer) Deploy(ctx context.Context, spec AppSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	// One deploy per app at a time, across processes: two interleaved deploys
	// would race the route switch and the happy-path sweep — the loser's
	// "retire everything except my container" removing the winner's routed
	// one. Fail fast; the operator retries once the other invocation finishes.
	release, err := d.locker.AcquireAppLock(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("lock app %s: %w", spec.Name, err)
	}
	defer release()

	return d.deployLocked(ctx, spec)
}

// Redeploy re-runs the app's stored spec. The read happens INSIDE the app
// lock: read outside it, a concurrent destroy could win the lock, remove the
// app completely, and then the stale spec would resurrect it — an outcome no
// serial order of the two operations can produce.
func (d *Deployer) Redeploy(ctx context.Context, app string) error {
	release, err := d.locker.AcquireAppLock(ctx, app)
	if err != nil {
		return fmt.Errorf("lock app %s: %w", app, err)
	}
	defer release()

	prev, err := d.store.GetApp(ctx, app)
	if err != nil {
		return fmt.Errorf("read app: %w", err)
	}
	if prev == nil {
		return fmt.Errorf("no such app: %s", app)
	}
	return d.deployLocked(ctx, prev.AppSpec)
}

// deployLocked is the deploy body; the caller holds the app lock.
//
// Failure paths retire exactly the container THIS deploy created (by its
// deterministic name), never "everything except the recorded live one": the
// record can be stale — SetLiveContainer may have failed after an earlier
// route switch — and a sweep keyed on it would remove the routed container.
// Strays a failed cleanup leaves behind are swept by the next successful
// deploy's retirement, which keys on its own fresh container.
func (d *Deployer) deployLocked(ctx context.Context, spec AppSpec) error {
	if err := d.store.UpsertApp(ctx, spec); err != nil {
		return fmt.Errorf("record app: %w", err)
	}
	id, err := d.store.CreateDeploy(ctx, spec.Name, spec.Image)
	if err != nil {
		return fmt.Errorf("record deploy: %w", err)
	}
	newName := ContainerName(spec.Name, id)

	if err := d.step(ctx, id, StatusPulling, func() error {
		return d.docker.Pull(ctx, spec.Image)
	}); err != nil {
		return err
	}

	var newID string
	if err := d.step(ctx, id, StatusStarting, func() error {
		cid, err := d.docker.StartApp(ctx, spec, id)
		if err != nil {
			// StartApp can fail after creating the named container; remove by
			// name so a half-created one doesn't linger either.
			d.removeNew(ctx, newName)
			return err
		}
		newID = cid
		if err := d.docker.WaitReady(ctx, cid, ReadyTimeout); err != nil {
			// Not ready in time: retire it; the old container is still routed.
			d.removeNew(ctx, newName)
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	if err := d.step(ctx, id, StatusRouting, func() error {
		dial := fmt.Sprintf("%s:%d", newName, spec.ContainerPort)
		if err := d.router.UpsertRoute(ctx, spec.Name, spec.Host, dial); err != nil {
			// Route not switched: retire the new container, old keeps serving.
			d.removeNew(ctx, newName)
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	// Past this point the new container is routed and serving: the deploy has
	// succeeded in the world, and everything left is bookkeeping and
	// retirement. All of it runs detached from the deploy context — the
	// context's death here (budget expiry mid-route, Ctrl-C) must not be able
	// to leave the record lying about which container is live, or the deploys
	// row stuck mid-state.
	tail, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	// Record the new container as live BEFORE retiring the old one: dying
	// between the two leaves only an extra unrouted container (swept by the
	// next successful deploy) — the reverse order could leave container_id
	// pointing at an already-removed container.
	if err := d.store.SetLiveContainer(tail, spec.Name, newID); err != nil {
		// The app IS serving on the new container, but the record now lies
		// about which container is live. Fail loud (not warn-and-continue)
		// and remove nothing: both containers stay up, and the next
		// successful deploy re-records and sweeps.
		return d.failDeploy(tail, id, fmt.Errorf("record live container: %w", err))
	}
	// Retire everything except the new container — best-effort: the app is
	// live and recorded, and newID is fresh from this very deploy, so the
	// sweep cannot hit the routed container.
	d.cleanupKeeping(tail, spec.Name, newID)
	return d.store.SetDeployStatus(tail, id, StatusLive, "")
}

// removeNew retires the container this deploy created, on a detached context
// for the same reason as failDeploy: the cleanup often runs BECAUSE the
// deploy context died. Best-effort — the step's own error is the thing worth
// surfacing, and a stray container is swept by the next successful deploy.
func (d *Deployer) removeNew(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := d.docker.RemoveContainer(ctx, name); err != nil {
		fmt.Printf("warn: remove container %s: %v\n", name, err)
	}
}

// Destroy removes an app entirely: route first (stop traffic), then
// containers, then the record.
func (d *Deployer) Destroy(ctx context.Context, app string) error {
	// Same lock as Deploy: a destroy interleaving with a deploy would remove
	// route/containers while the deploy re-creates them, leaving halves.
	release, err := d.locker.AcquireAppLock(ctx, app)
	if err != nil {
		return fmt.Errorf("lock app %s: %w", app, err)
	}
	defer release()

	if err := d.router.DeleteRoute(ctx, app); err != nil {
		return fmt.Errorf("delete route: %w", err)
	}
	if err := d.docker.RemoveAppExcept(ctx, app, ""); err != nil {
		return fmt.Errorf("remove containers: %w", err)
	}
	if err := d.store.DeleteApp(ctx, app); err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	return nil
}

// step records the transition, runs the step, and on failure records
// StatusFailed with the cause — so the deploys table always tells you
// exactly which step a deploy died in.
func (d *Deployer) step(ctx context.Context, id int64, s Status, f func() error) error {
	if err := d.store.SetDeployStatus(ctx, id, s, ""); err != nil {
		return fmt.Errorf("record status %s: %w", s, err)
	}
	if err := f(); err != nil {
		return d.failDeploy(ctx, id, fmt.Errorf("%s: %w", s, err))
	}
	return nil
}

// failDeploy records StatusFailed with the cause and returns the cause. The
// write runs on a fresh context detached from the deploy's: the deploy
// context's death (the CLI's 5-minute budget, Ctrl-C) is a common failure
// cause, and recording the failure on the already-dead context would fail
// instantly — leaving the deploys row stuck mid-state exactly when the
// "history tells you which step it died in" promise matters most.
func (d *Deployer) failDeploy(ctx context.Context, id int64, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if serr := d.store.SetDeployStatus(ctx, id, StatusFailed, cause.Error()); serr != nil {
		return fmt.Errorf("%w (and recording failure also failed: %v)", cause, serr)
	}
	return cause
}

// cleanupKeeping removes every container of the app except keepID. Only the
// happy path calls it (keep = the container this very deploy routed), which
// is what makes the sweep safe: keepID is fresh, never a stale record.
// Best-effort: the deploy outcome is the thing worth surfacing, not a
// cleanup hiccup. Detached from the deploy context like failDeploy — a
// leaked restart=unless-stopped container costs up to 512MB until some
// later deploy sweeps it.
func (d *Deployer) cleanupKeeping(ctx context.Context, app, keepID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := d.docker.RemoveAppExcept(ctx, app, keepID); err != nil {
		fmt.Printf("warn: cleanup %s containers: %v\n", app, err)
	}
}
