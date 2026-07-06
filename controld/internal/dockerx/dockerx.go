// dockerx.go — Docker SDK adapter implementing deploy.Docker: pulls images,
// runs app containers on app_net (named qc-<app>-<deployID>, labeled
// qincloud.app=<app>), waits for readiness, retires old containers.
//
// The one non-obvious part is readiness: images with a healthcheck are ready
// when Docker says "healthy"; images without one are ready once they have
// stayed running for a short grace period. That per-inspect decision is the
// pure function decideReadiness, unit-testable without a daemon.
package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"

	"qincloud/controld/internal/deploy"
)

const (
	// appNet is the external bridge shared with Caddy (stack/edge). Caddy
	// dials app containers by name on it, so every app must attach here.
	appNet = "app_net"

	// tenantDBNet is the external bridge apps join with AppSpec.UseDB: the
	// shared Postgres is on it, and NOTHING else — redis, the exporters, and
	// controld stay unreachable from tenant workloads (invariant #3 at the
	// network layer, not just per-service auth). Created by bootstrap.sh.
	tenantDBNet = "tenant_db_net"

	// appLabel marks every container controld owns. RemoveAppExcept finds
	// containers by it, so it is the source of truth for "which containers
	// belong to app X".
	appLabel = "qincloud.app"

	// pollInterval and readyGrace drive WaitReady: inspect about once a
	// second; a container without a healthcheck counts as ready after it has
	// stayed running this long — long enough to catch an immediate crash,
	// short enough not to drag out every deploy.
	pollInterval = time.Second
	readyGrace   = 3 * time.Second

	// stopGraceSeconds is how long a retiring container gets to shut down
	// cleanly (SIGTERM) before SIGKILL.
	stopGraceSeconds = 10

	// The resource fence every app container runs inside. A memory cap alone
	// does not contain a misbehaving image: Docker's default pids-limit is
	// unlimited, so a fork-bombing app would exhaust host PIDs and hang the
	// whole box (edge, postgres, controld itself), and an uncapped CPU
	// spinner would starve every other stack on the shared 4-vCPU host. One
	// buggy image must never take the box down, so pids and CPU are fenced
	// alongside memory.
	// ponytail: flat limits for all apps — plenty for a single-VPS PaaS
	// today; when a real app needs more, promote these to AppSpec fields
	// instead of raising the global caps.
	memoryLimitBytes = 512 << 20
	pidsLimit        = 256
	cpuNanoLimit     = 2_000_000_000 // NanoCPUs: 2 of the box's 4 vCPUs
)

// Client implements deploy.Docker on top of the host daemon's API.
type Client struct {
	api *client.Client
}

var _ deploy.Docker = (*Client)(nil)

// New connects to the host daemon via /var/run/docker.sock (DOCKER_HOST
// respected) with API version negotiation.
func New() (*Client, error) {
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect docker daemon: %w", err)
	}
	return &Client{api: api}, nil
}

func (c *Client) Close() error { return c.api.Close() }

// Pull pulls the image via the host daemon. The daemon performs the pull
// while the progress stream is consumed, and mid-pull failures arrive as
// JSON error messages *inside* the stream — so the stream must be both
// drained to EOF and parsed, not just discarded.
func (c *Client) Pull(ctx context.Context, imageRef string) error {
	progress, err := c.api.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	defer progress.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(progress, io.Discard, 0, false, nil); err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	return nil
}

// StartApp creates and starts container qc-<app>-<deployID> on app_net and
// returns its ID. NO published ports — invariant #2: only Caddy publishes;
// it reaches the app by container name over app_net (docker DNS).
func (c *Client) StartApp(ctx context.Context, spec deploy.AppSpec, deployID int64) (string, error) {
	name := deploy.ContainerName(spec.Name, deployID)
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cfg := &container.Config{
		Image:  spec.Image,
		Env:    env,
		Labels: map[string]string{appLabel: spec.Name},
	}
	// PidsLimit is a pointer in the SDK: nil means "leave unchanged", so the
	// limit must be an addressable value.
	appPids := int64(pidsLimit)
	hostCfg := &container.HostConfig{
		// unless-stopped: apps survive daemon/host restarts, but stay down
		// once controld deliberately stops them during retirement.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		Resources: container.Resources{
			Memory:    memoryLimitBytes,
			NanoCPUs:  cpuNanoLimit,
			PidsLimit: &appPids,
		},
		// controld runs images it did not build; no-new-privileges keeps a
		// container process from ever gaining privileges (setuid binaries
		// are inert) — free hardening with no effect on well-behaved apps.
		SecurityOpt: []string{"no-new-privileges:true"},
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{appNet: {}},
	}
	if spec.UseDB {
		netCfg.EndpointsConfig[tenantDBNet] = &network.EndpointSettings{}
	}

	created, err := c.api.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", name, err)
	}
	if err := c.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// Don't leak a created-but-never-started container: the deploy
		// machine only cleans up once StartApp has returned an ID.
		if rmErr := c.api.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			return "", fmt.Errorf("start %s: %w (and removing the created container failed: %v)", name, err, rmErr)
		}
		return "", fmt.Errorf("start %s: %w", name, err)
	}
	return created.ID, nil
}

// WaitReady polls the container about once a second until decideReadiness
// reports ready or fatal, or the timeout elapses. It tracks how long the
// container has been *continuously* running so the no-healthcheck grace
// clock resets if the container stops in between.
func (c *Client) WaitReady(ctx context.Context, containerID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var runningSince time.Time // zero until the container is first seen running
	for {
		info, err := c.api.ContainerInspect(ctx, containerID)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", containerID, err)
		}
		var state *container.State
		if info.ContainerJSONBase != nil {
			state = info.State
		}

		if state != nil && state.Running {
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
		} else {
			runningSince = time.Time{}
		}
		var runningFor time.Duration
		if !runningSince.IsZero() {
			runningFor = time.Since(runningSince)
		}

		ready, err := decideReadiness(state, runningFor, readyGrace)
		if err != nil {
			return fmt.Errorf("container %s: %w", containerID, err)
		}
		if ready {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s not ready after %s: %w", containerID, timeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// decideReadiness is WaitReady's per-poll decision as a pure function: one
// inspected state in, one verdict out. runningFor is how long the caller has
// observed the container continuously running (zero when it is not running
// right now); grace is the settle time for images without a healthcheck.
//
// Verdicts: (true, nil) ready · (false, nil) keep waiting · (false, err)
// fatal — the container will never become ready, stop polling.
func decideReadiness(state *container.State, runningFor, grace time.Duration) (bool, error) {
	// A missing state is a broken inspect response, not a retryable condition.
	if state == nil {
		return false, errors.New("inspect returned no container state")
	}

	// A finished container never comes back on its own.
	if state.Status == container.StateExited || state.Status == container.StateDead {
		return false, fmt.Errorf("%s (exit code %d)", state.Status, state.ExitCode)
	}
	// "restarting" means the process already crashed and the unless-stopped
	// restart policy is respawning it — a crashed container usually shows
	// this state, not "exited". The policy is a safety net for production;
	// during a deploy one crash means the deploy is bad, so fail now instead
	// of letting the crash loop eat the whole timeout.
	if state.Status == container.StateRestarting {
		return false, fmt.Errorf("crashed and is restarting (exit code %d)", state.ExitCode)
	}

	// Image has a healthcheck → Docker's verdict is the verdict.
	if hasHealthcheck(state.Health) {
		switch state.Health.Status {
		case container.Healthy:
			return true, nil
		case container.Unhealthy:
			return false, fmt.Errorf("unhealthy: %s", lastHealthLine(state.Health))
		default: // "starting" — the healthcheck has not concluded yet
			return false, nil
		}
	}

	// No healthcheck → running continuously for the grace period is the best
	// readiness signal available.
	if state.Running && runningFor >= grace {
		return true, nil
	}
	return false, nil
}

// hasHealthcheck reports whether the inspected state carries a real
// healthcheck verdict. Images without a HEALTHCHECK inspect with Health nil;
// HEALTHCHECK NONE inspects with status "none" — both mean "no healthcheck".
func hasHealthcheck(h *container.Health) bool {
	return h != nil && h.Status != container.NoHealthcheck
}

// lastHealthLine returns the trimmed output of the most recent health probe
// that produced any, so an unhealthy verdict carries the actual failure and
// not just the word "unhealthy".
func lastHealthLine(h *container.Health) string {
	for i := len(h.Log) - 1; i >= 0; i-- {
		if h.Log[i] == nil {
			continue
		}
		if out := strings.TrimSpace(h.Log[i].Output); out != "" {
			return out
		}
	}
	return "no health log output"
}

// RemoveContainer stops (10s grace) and removes one container by name or ID.
// Absent at any step is success: something else already did the job.
func (c *Client) RemoveContainer(ctx context.Context, nameOrID string) error {
	stopGrace := stopGraceSeconds
	err := c.api.ContainerStop(ctx, nameOrID, container.StopOptions{Timeout: &stopGrace})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("stop %s: %w", nameOrID, err)
	}
	// Force also covers the race where the container restarted between the
	// stop above and this remove.
	err = c.api.ContainerRemove(ctx, nameOrID, container.RemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove %s: %w", nameOrID, err)
	}
	return nil
}

// RemoveAppExcept stops (10s grace) and removes every qincloud.app=<app>
// container — running or not — except keepID. A container that vanishes
// between list and stop/remove is success, not error: something else already
// did the job.
func (c *Client) RemoveAppExcept(ctx context.Context, app, keepID string) error {
	byLabel := filters.NewArgs(filters.Arg("label", appLabel+"="+app))
	containers, err := c.api.ContainerList(ctx, container.ListOptions{All: true, Filters: byLabel})
	if err != nil {
		return fmt.Errorf("list %s containers: %w", app, err)
	}

	stopGrace := stopGraceSeconds
	for _, ctr := range containers {
		if ctr.ID == keepID {
			continue
		}
		err := c.api.ContainerStop(ctx, ctr.ID, container.StopOptions{Timeout: &stopGrace})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("stop %s: %w", ctr.ID, err)
		}
		// Force also covers the race where the container restarted between
		// the stop above and this remove.
		err = c.api.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove %s: %w", ctr.ID, err)
		}
	}
	return nil
}
