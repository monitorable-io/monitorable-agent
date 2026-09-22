package collectors

import (
	"strings"
	"time"

	ctypes "github.com/moby/moby/api/types/container"
)

// netSample is the previous cycle's summed network byte counters for a container.
type netSample struct {
	Rx uint64
	Tx uint64
	At time.Time
}

// cpuSample is the previous cycle's cumulative CPU counters for a container. One-shot
// stats carry no prior sample, so we retain this between cycles to compute utilization.
type cpuSample struct {
	Total  uint64 // CPUUsage.TotalUsage
	System uint64 // SystemUsage (system_cpu_usage)
}

// ContainerStats is one container's wire snapshot for the `docker.containers`
// resource attribute. Stats pointers are set only when stats were read this cycle
// (running containers); absent fields mean "not applicable" (backend stores NULL).
type ContainerStats struct {
	ID               string   `json:"container_id"`
	Name             string   `json:"name"`
	Image            string   `json:"image"`
	State            string   `json:"state"`               // running|paused|restarting|exited|dead
	Health           string   `json:"health,omitempty"`    // healthy|unhealthy|starting
	ExitCode         *int     `json:"exit_code,omitempty"` // exited/dead only
	RestartCount     int      `json:"restart_count"`
	StartedAtUnixMs  int64    `json:"started_at_unix_ms,omitempty"`
	FinishedAtUnixMs int64    `json:"finished_at_unix_ms,omitempty"` // exited/dead only
	ServiceName      string   `json:"service_name,omitempty"`        // com.docker.compose.service
	ProjectName      string   `json:"project_name,omitempty"`        // com.docker.compose.project
	CPUPercent       *float64 `json:"cpu_percent,omitempty"`
	MemUsage         *int64   `json:"memory_usage_bytes,omitempty"`
	MemLimit         *int64   `json:"memory_limit_bytes,omitempty"`
	NetRxRate        *float64 `json:"net_rx_bytes_per_sec,omitempty"`
	NetTxRate        *float64 `json:"net_tx_bytes_per_sec,omitempty"`
}

// applyRunningStats fills the stats pointer fields on cs from one stats read and
// returns the samples to retain for next cycle's deltas. First sight (zero prev)
// yields 0 CPU% and 0 rates, matching the previous first-scrape behavior.
func applyRunningStats(cs *ContainerStats, raw *ctypes.StatsResponse, prevCPU cpuSample, prevNet netSample, now time.Time) (cpuSample, netSample) {
	cpu := calculateCPUPercent(&raw.CPUStats, prevCPU)
	cs.CPUPercent = &cpu

	mem := int64(calculateMemUsageNoCache(&raw.MemoryStats))
	limit := int64(raw.MemoryStats.Limit)
	cs.MemUsage = &mem
	cs.MemLimit = &limit

	rx, tx := sumNetworks(raw)
	var rxRate, txRate float64
	if !prevNet.At.IsZero() {
		elapsed := now.Sub(prevNet.At).Seconds()
		rxRate = calculateNetRate(rx, prevNet.Rx, elapsed)
		txRate = calculateNetRate(tx, prevNet.Tx, elapsed)
	}
	cs.NetRxRate = &rxRate
	cs.NetTxRate = &txRate

	return cpuSample{Total: raw.CPUStats.CPUUsage.TotalUsage, System: raw.CPUStats.SystemUsage},
		netSample{Rx: rx, Tx: tx, At: now}
}

// sumNetworks totals the per-interface counters — the panel shows one rate pair per
// container, so per-interface resolution is not carried over the wire.
func sumNetworks(raw *ctypes.StatsResponse) (rx, tx uint64) {
	for _, n := range raw.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	return rx, tx
}

// calculateNetRate returns bytes/sec for one counter delta; counter resets and
// non-positive elapsed clamp to 0.
func calculateNetRate(cur, prev uint64, elapsed float64) float64 {
	if elapsed <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / elapsed
}

// dockerTimeMs parses Docker's RFC3339Nano timestamps; the zero sentinel
// ("0001-01-01T00:00:00Z") and unparseable values map to 0.
func dockerTimeMs(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return 0
	}
	return t.UnixMilli()
}

// calculateCPUPercent mirrors dockerstatsreceiver's formula, but uses the previous cycle's
// sample we retain (one-shot stats don't populate PreCPUStats). Returns 0 when there is no
// previous sample yet (prev.System == 0 — SystemUsage is never 0 on a live host), matching
// the upstream first-scrape behavior.
func calculateCPUPercent(cur *ctypes.CPUStats, prev cpuSample) float64 {
	if prev.System == 0 {
		return 0.0
	}
	cpuDelta := float64(cur.CPUUsage.TotalUsage) - float64(prev.Total)
	systemDelta := float64(cur.SystemUsage) - float64(prev.System)
	onlineCPUs := float64(cur.OnlineCPUs)
	if onlineCPUs == 0.0 {
		onlineCPUs = float64(len(cur.CPUUsage.PercpuUsage))
	}
	if systemDelta > 0.0 && cpuDelta > 0.0 {
		return (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}
	return 0.0
}

// calculateMemUsageNoCache mirrors dockerstatsreceiver: subtract the inactive-file cache
// (cgroup v1 "total_inactive_file", else cgroup v2 "inactive_file") from total usage.
func calculateMemUsageNoCache(m *ctypes.MemoryStats) uint64 {
	if v, ok := m.Stats["total_inactive_file"]; ok && v < m.Usage {
		return m.Usage - v
	}
	if v := m.Stats["inactive_file"]; v < m.Usage {
		return m.Usage - v
	}
	return m.Usage
}

// containerName returns the first container name with the leading "/" stripped.
func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
