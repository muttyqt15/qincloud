// dockerx_test.go — unit tests for decideReadiness, the pure per-poll
// readiness decision behind WaitReady. The SDK adapter itself is exercised
// against a real daemon, not faked here (never mock the unit under test).
package dockerx

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func TestDecideReadiness(t *testing.T) {
	tests := []struct {
		name       string
		state      *container.State
		runningFor time.Duration
		wantReady  bool
		wantErr    string // substring of the fatal error; "" means not fatal
	}{
		{
			name:    "nil state is fatal",
			state:   nil,
			wantErr: "no container state",
		},
		{
			name:    "exited is fatal with exit code",
			state:   &container.State{Status: container.StateExited, ExitCode: 137},
			wantErr: "exit code 137",
		},
		{
			name:    "dead is fatal",
			state:   &container.State{Status: container.StateDead, ExitCode: 1},
			wantErr: "dead",
		},
		{
			// Under restart policy unless-stopped a crashed container shows
			// "restarting", not "exited" — it must still fail early.
			name:    "restarting is fatal",
			state:   &container.State{Status: container.StateRestarting, ExitCode: 2},
			wantErr: "restarting",
		},
		{
			name: "healthcheck starting keeps waiting even past the grace period",
			state: &container.State{
				Status: container.StateRunning, Running: true,
				Health: &container.Health{Status: container.Starting},
			},
			runningFor: time.Minute,
		},
		{
			name: "healthcheck healthy is ready immediately",
			state: &container.State{
				Status: container.StateRunning, Running: true,
				Health: &container.Health{Status: container.Healthy},
			},
			wantReady: true,
		},
		{
			name: "unhealthy is fatal and carries the last probe output",
			state: &container.State{
				Status: container.StateRunning, Running: true,
				Health: &container.Health{
					Status: container.Unhealthy,
					Log: []*container.HealthcheckResult{
						{Output: "older probe"},
						{Output: "connection refused\n"},
					},
				},
			},
			wantErr: "connection refused",
		},
		{
			name: "unhealthy with an empty log still fails loud",
			state: &container.State{
				Status: container.StateRunning, Running: true,
				Health: &container.Health{Status: container.Unhealthy},
			},
			wantErr: "unhealthy",
		},
		{
			name:       "no healthcheck keeps waiting inside the grace period",
			state:      &container.State{Status: container.StateRunning, Running: true},
			runningFor: readyGrace - time.Millisecond,
		},
		{
			name:       "no healthcheck is ready once the grace period has elapsed",
			state:      &container.State{Status: container.StateRunning, Running: true},
			runningFor: readyGrace,
			wantReady:  true,
		},
		{
			name: "health status none counts as no healthcheck",
			state: &container.State{
				Status: container.StateRunning, Running: true,
				Health: &container.Health{Status: container.NoHealthcheck},
			},
			runningFor: readyGrace,
			wantReady:  true,
		},
		{
			name:  "created and not yet running keeps waiting",
			state: &container.State{Status: container.StateCreated},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := decideReadiness(tt.state, tt.runningFor, readyGrace)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want fatal error containing %q, got ready=%v err=nil", tt.wantErr, ready)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected fatal error: %v", err)
			}
			if ready != tt.wantReady {
				t.Fatalf("ready = %v, want %v", ready, tt.wantReady)
			}
		})
	}
}

func TestCalcStats(t *testing.T) {
	base := func() container.StatsResponse {
		var r container.StatsResponse
		r.CPUStats.CPUUsage.TotalUsage = 2_000_000_000
		r.PreCPUStats.CPUUsage.TotalUsage = 1_000_000_000
		r.CPUStats.SystemUsage = 20_000_000_000
		r.PreCPUStats.SystemUsage = 10_000_000_000
		r.CPUStats.OnlineCPUs = 4
		r.MemoryStats.Usage = 300 << 20
		r.MemoryStats.Stats = map[string]uint64{"inactive_file": 44 << 20}
		r.MemoryStats.Limit = 512 << 20
		return r
	}

	t.Run("normal sample", func(t *testing.T) {
		s := calcStats(base())
		if s.CPUPercent != 40 { // 1e9/10e9 * 4 cpus * 100
			t.Fatalf("cpu = %v, want 40", s.CPUPercent)
		}
		if s.MemBytes != 256<<20 {
			t.Fatalf("mem = %d, want %d (usage minus inactive_file)", s.MemBytes, 256<<20)
		}
		if s.MemLimit != 512<<20 {
			t.Fatalf("limit = %d, want %d", s.MemLimit, 512<<20)
		}
	})

	t.Run("zero system delta yields zero cpu, not NaN", func(t *testing.T) {
		r := base()
		r.CPUStats.SystemUsage = r.PreCPUStats.SystemUsage
		if s := calcStats(r); s.CPUPercent != 0 {
			t.Fatalf("cpu = %v, want 0", s.CPUPercent)
		}
	})

	t.Run("inactive_file larger than usage does not underflow", func(t *testing.T) {
		r := base()
		r.MemoryStats.Stats["inactive_file"] = r.MemoryStats.Usage + 1
		if s := calcStats(r); s.MemBytes != 0 {
			t.Fatalf("mem = %d, want 0", s.MemBytes)
		}
	})

	t.Run("no online cpus falls back to percpu list", func(t *testing.T) {
		r := base()
		r.CPUStats.OnlineCPUs = 0
		r.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 1}
		if s := calcStats(r); s.CPUPercent != 20 {
			t.Fatalf("cpu = %v, want 20 (2 cpus)", s.CPUPercent)
		}
	})
}
