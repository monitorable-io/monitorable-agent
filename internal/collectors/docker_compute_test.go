package collectors

import (
	"testing"
	"time"

	ctypes "github.com/moby/moby/api/types/container"
)

func TestCalculateMemUsageNoCache(t *testing.T) {
	cases := []struct {
		name  string
		stats ctypes.MemoryStats
		want  uint64
	}{
		{"cgroup_v1", ctypes.MemoryStats{Usage: 500, Stats: map[string]uint64{"total_inactive_file": 100}}, 400},
		{"cgroup_v2", ctypes.MemoryStats{Usage: 500, Stats: map[string]uint64{"inactive_file": 50}}, 450},
		{"no_stats", ctypes.MemoryStats{Usage: 500}, 500},
		{"cache_exceeds_usage", ctypes.MemoryStats{Usage: 100, Stats: map[string]uint64{"total_inactive_file": 200}}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calculateMemUsageNoCache(&tc.stats); got != tc.want {
				t.Errorf("calculateMemUsageNoCache = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCalculateCPUPercent(t *testing.T) {
	cur := ctypes.CPUStats{
		CPUUsage:    ctypes.CPUUsage{TotalUsage: 200},
		SystemUsage: 2000,
		OnlineCPUs:  2,
	}
	// No previous sample → 0.
	if got := calculateCPUPercent(&cur, cpuSample{}); got != 0 {
		t.Errorf("no-prev = %v, want 0", got)
	}
	// cpuDelta=100, systemDelta=1000, onlineCPUs=2 → (100/1000)*2*100 = 20.
	if got := calculateCPUPercent(&cur, cpuSample{Total: 100, System: 1000}); got != 20 {
		t.Errorf("= %v, want 20", got)
	}
}

func TestContainerName(t *testing.T) {
	if got := containerName([]string{"/web"}); got != "web" {
		t.Errorf("= %q, want web", got)
	}
	if got := containerName(nil); got != "" {
		t.Errorf("= %q, want empty", got)
	}
}

func TestDockerTimeMs(t *testing.T) {
	if got := dockerTimeMs("0001-01-01T00:00:00Z"); got != 0 {
		t.Errorf("zero time → %d, want 0", got)
	}
	if got := dockerTimeMs("not-a-time"); got != 0 {
		t.Errorf("garbage → %d, want 0", got)
	}
	want := time.Date(2026, 7, 7, 10, 0, 0, 500_000_000, time.UTC).UnixMilli()
	if got := dockerTimeMs("2026-07-07T10:00:00.5Z"); got != want {
		t.Errorf("RFC3339Nano → %d, want %d", got, want)
	}
}

func TestCalculateNetRate(t *testing.T) {
	if got := calculateNetRate(2000, 1000, 10); got != 100 {
		t.Errorf("rate = %v, want 100", got)
	}
	if got := calculateNetRate(500, 1000, 10); got != 0 { // counter reset
		t.Errorf("reset rate = %v, want 0", got)
	}
	if got := calculateNetRate(2000, 1000, 0); got != 0 { // zero elapsed
		t.Errorf("zero-elapsed rate = %v, want 0", got)
	}
}
