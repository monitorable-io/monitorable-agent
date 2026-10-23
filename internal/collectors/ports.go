package collectors

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxAllPorts caps PortsSnapshot.AllPorts; beyond it AllPortsTruncated is set.
const MaxAllPorts = 1000

// PortDetail is one listening TCP port aggregated over the sampling window.
// It carries no addresses, peers, PIDs or command lines.
type PortDetail struct {
	Port     int     `json:"port"`
	Service  string  `json:"service"`  // /etc/services name, "" when unknown
	Scope    string  `json:"scope"`    // "local" (loopback only) | "exposed"
	Families string  `json:"families"` // "v4" | "v6" | "dual"
	ConnAvg  float64 `json:"conn_avg"` // mean ESTABLISHED count over the samples where the port listened
	ConnPeak int     `json:"conn_peak"`
	Samples  int     `json:"samples"` // samples in which the port was listening
}

// PortsSnapshot is the per-cycle listening-ports report.
type PortsSnapshot struct {
	CollectedAtUnixMs int64        `json:"collected_at_unix_ms"`
	AllPorts          []int        `json:"all_ports"` // every listening port of the last sample, ascending, capped at MaxAllPorts
	AllPortsTruncated bool         `json:"all_ports_truncated"`
	Ports             []PortDetail `json:"ports"` // detail rows for the lowest-numbered ports only
}

// PortsStatus is the outcome of a single Snapshot.
type PortsStatus int

const (
	PortsOK         PortsStatus = iota // snapshot built from at least one sample
	PortsNoSamples                     // window empty, nothing failed (e.g. agent just started)
	PortsUnreadable                    // window empty because the last Sample could not read /proc/net/tcp
)

// PortsResult is the outcome of PortsCollector.Snapshot.
type PortsResult struct {
	Status   PortsStatus
	Err      error // last Sample error when Status is PortsUnreadable
	Snapshot *PortsSnapshot
}

// portSample is one port's state in a single sample.
type portSample struct {
	facts listenerFacts
	conns int
}

type portsWindow struct {
	samples []map[uint16]portSample
	lastAt  time.Time
}

// PortsCollector samples /proc/net/tcp{,6} and aggregates listening ports and
// ESTABLISHED connection counts over a window. Sample (every ~10s) and Snapshot
// (once per collection cycle) run on different goroutines.
type PortsCollector struct {
	procRoot string
	services map[uint16]string
	logger   *slog.Logger // may be nil (tests)

	mu      sync.Mutex
	window  portsWindow
	lastErr error // error of the most recent Sample, nil after a good one
}

// NewPortsCollector reads the service-name table once; production passes
// "/proc" and "/etc/services".
func NewPortsCollector(procRoot, servicesPath string, logger *slog.Logger) *PortsCollector {
	services := loadServices(servicesPath)
	if len(services) == 0 && logger != nil {
		logger.Debug("ports: no service names loaded", "path", servicesPath)
	}
	return &PortsCollector{procRoot: procRoot, services: services, logger: logger}
}

// Sample reads the socket tables and appends one sample to the window. A failed
// read is not appended and its error is returned. A missing tcp6 file (IPv6
// disabled) is not an error; a missing tcp is.
func (c *PortsCollector) Sample(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listeners, established, err := c.readTables()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err
	if err != nil {
		return err
	}
	s := make(map[uint16]portSample, len(listeners))
	for port, f := range listeners {
		s[port] = portSample{facts: *f, conns: established[port]}
	}
	c.window.samples = append(c.window.samples, s)
	c.window.lastAt = now
	return nil
}

func (c *PortsCollector) readTables() (map[uint16]*listenerFacts, map[uint16]int, error) {
	v4, err := c.readTable("tcp", false)
	if err != nil {
		return nil, nil, err
	}
	v6, err := c.readTable("tcp6", true)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	listeners, established := summarizeSockets(v4, v6)
	return listeners, established, nil
}

func (c *PortsCollector) readTable(name string, v6 bool) ([]tcpRow, error) {
	f, err := os.Open(filepath.Join(c.procRoot, "net", name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rows, err := parseProcNetTCP(f, v6)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return rows, nil
}

// Snapshot aggregates the window and resets it. maxPorts (>= 1, validated by the
// caller) bounds the detail rows; AllPorts is independent of it.
func (c *PortsCollector) Snapshot(maxPorts int) PortsResult {
	c.mu.Lock()
	w := c.window
	c.window = portsWindow{}
	lastErr := c.lastErr
	c.mu.Unlock()

	if len(w.samples) == 0 {
		if lastErr != nil {
			return PortsResult{Status: PortsUnreadable, Err: lastErr}
		}
		return PortsResult{Status: PortsNoSamples}
	}

	// The last sample decides which ports exist; earlier samples only feed stats.
	last := w.samples[len(w.samples)-1]
	ports := make([]int, 0, len(last))
	for p := range last {
		ports = append(ports, int(p))
	}
	sort.Ints(ports)

	snap := &PortsSnapshot{
		CollectedAtUnixMs: w.lastAt.UnixMilli(),
		AllPorts:          ports,
		Ports:             []PortDetail{},
	}
	if len(snap.AllPorts) > MaxAllPorts {
		snap.AllPorts = snap.AllPorts[:MaxAllPorts]
		snap.AllPortsTruncated = true
	}
	for _, p := range ports[:min(maxPorts, len(ports))] {
		snap.Ports = append(snap.Ports, c.detail(uint16(p), last[uint16(p)].facts, w.samples))
	}
	return PortsResult{Status: PortsOK, Snapshot: snap}
}

func (c *PortsCollector) detail(port uint16, facts listenerFacts, samples []map[uint16]portSample) PortDetail {
	d := PortDetail{Port: int(port), Service: c.services[port], Scope: "exposed"}
	if facts.Loopback {
		d.Scope = "local"
	}
	switch {
	case facts.V4 && facts.V6:
		d.Families = "dual"
	case facts.V6:
		d.Families = "v6"
	default:
		d.Families = "v4"
	}
	sum := 0
	for _, s := range samples {
		ps, ok := s[port]
		if !ok {
			continue
		}
		d.Samples++
		sum += ps.conns
		d.ConnPeak = max(d.ConnPeak, ps.conns)
	}
	d.ConnAvg = float64(sum) / float64(d.Samples) // Samples >= 1: the port is in the last sample
	return d
}

// loadServices maps port to service name from an /etc/services-format file,
// keeping the first tcp entry per port. A missing or unreadable file yields an
// empty map: names are cosmetic.
func loadServices(path string) map[uint16]string {
	out := make(map[uint16]string)
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		portStr, proto, ok := strings.Cut(fields[1], "/")
		if !ok || proto != "tcp" {
			continue
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			continue
		}
		if _, dup := out[uint16(port)]; !dup {
			out[uint16(port)] = fields[0]
		}
	}
	return out
}
