// deploy.go — the deploy state machine. One linear path:
//
//	validate → lock app → record → pull → start+wait-ready → route → record live → retire old
//
// The old container keeps serving until the new one is ready AND routed, so a
// failed deploy never takes the app down: any step failure removes the new
// container (everything except the previous live one), marks the deploy
// failed, and leaves the previous state intact. Cleanup and failure recording
// run on a context detached from the deploy's, because the deploy context's
// death is itself a common failure cause.
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
	// would each read the same previous container and sweep "everything
	// except" it — the loser removing the winner's routed container. Fail
	// fast; the operator retries once the other invocation finishes.
	release, err := d.locker.AcquireAppLock(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("lock app %s: %w", spec.Name, err)
	}
	defer release()

	// The previous live container (if any) is both the fallback that keeps
	// serving during this deploy and the thing we retire once routed.
	prevID := ""
	if prev, err := d.store.GetApp(ctx, spec.Name); err != nil {
		return fmt.Errorf("read app: %w", err)
	} else if prev != nil {
		prevID = prev.ContainerID
	}

	if err := d.store.UpsertApp(ctx, spec); err != nil {
		return fmt.Errorf("record app: %w", err)
	}
	id, err := d.store.CreateDeploy(ctx, spec.Name, spec.Image)
	if err != nil {
		return fmt.Errorf("record deploy: %w", err)
	}

	if err := d.step(ctx, id, StatusPulling, func() error {
		return d.docker.Pull(ctx, spec.Image)
	}); err != nil {
		return err
	}

	var newID string
	if err := d.step(ctx, id, StatusStarting, func() error {
		cid, err := d.docker.StartApp(ctx, spec, id)
		if err != nil {
			return err
		}
		newID = cid
		if err := d.docker.WaitReady(ctx, cid, ReadyTimeout); err != nil {
			// Not ready in time: retire it; the old container is still routed.
			d.cleanupKeeping(ctx, spec.Name, prevID)
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	if err := d.step(ctx, id, StatusRouting, func() error {
		dial := fmt.Sprintf("%s:%d", ContainerName(spec.Name, id), spec.ContainerPort)
		if err := d.router.UpsertRoute(ctx, spec.Name, spec.Host, dial); err != nil {
			// Route not switched: retire the new container, old keeps serving.
			d.cleanupKeeping(ctx, spec.Name, prevID)
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	// The new container is routed; the old one is now unreachable. Record the
	// new container as live BEFORE retiring the old one: dying between the
	// two then leaves an extra unrouted container (swept by the next
	// successful deploy) — the reverse order could leave container_id
	// pointing at an already-removed container, and a LATER failed deploy's
	// cleanup would keep that stale ID and remove the routed container
	// instead: hard downtime.
	if err := d.store.SetLiveContainer(ctx, spec.Name, newID); err != nil {
		// The app IS serving on the new container, but the record now lies
		// about which container is live — the exact fact cleanupKeeping
		// trusts. Fail loud (not warn-and-continue) and remove nothing: both
		// containers stay up, and the next successful deploy re-records and
		// sweeps.
		return d.failDeploy(ctx, id, fmt.Errorf("record live container: %w", err))
	}
	// Retire the old container — best-effort: the app is live and recorded.
	d.cleanupKeeping(ctx, spec.Name, newID)
	return d.store.SetDeployStatus(ctx, id, StatusLive, "")
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

// cleanupKeeping removes every container of the app except keepID — the
// mid-deploy failure paths retire the new container with it (keep = previous
// live), the happy path retires the old one (keep = new). Best-effort: the
// deploy outcome is the thing worth surfacing, not a cleanup hiccup. Like
// failDeploy it detaches from the deploy context: when cleanup is triggered
// by that context's death, running on it would fail every docker call and
// leak a running restart=unless-stopped container (up to 512MB) until some
// later deploy of the same app happens to sweep it.
func (d *Deployer) cleanupKeeping(ctx context.Context, app, keepID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := d.docker.RemoveAppExcept(ctx, app, keepID); err != nil {
		fmt.Printf("warn: cleanup %s containers: %v\n", app, err)
	}
}
