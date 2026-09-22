package collectors

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// userHZ is the kernel clock tick (USER_HZ). 100 on all supported Linux targets;
// gopsutil uses the same default, so emitted second-values stay continuous with the
// metrics previously produced by the hostmetrics cpu scraper.
const userHZ = 100.0

// CPUStateTime holds cumulative seconds a single logical CPU spent in each state
// since boot. Field meanings match /proc/stat columns (iowait == OTel "wait",
// irq == "interrupt").
type CPUStateTime struct {
	CPU     string
	User    float64
	Nice    float64
	System  float64
	Idle    float64
	Iowait  float64
	Irq     float64
	Softirq float64
	Steal   float64
}

// CPUTimes is a single snapshot of /proc/stat: cumulative per-core counters plus the
// system boot time (epoch seconds, from the "btime" line) used as the counter start.
type CPUTimes struct {
	BootTime int64
	PerCPU   []CPUStateTime
}

// CPUTimeCollector reads /proc/stat. It is STATELESS: each Collect returns the current
// cumulative counters and retains nothing between calls. That is what makes it immune
// to CPU-set changes (cpuset re-pin, hotplug, core parking) — there is no previous set
// to fall out of sync with.
type CPUTimeCollector struct {
	statPath string
}

// NewCPUTimeCollector returns a collector reading the real /proc/stat.
func NewCPUTimeCollector() *CPUTimeCollector {
	return &CPUTimeCollector{statPath: "/proc/stat"}
}

// Collect parses /proc/stat. Per-core lines ("cpu0", "cpu8", ...) are returned with
// their natural kernel labels; the aggregate "cpu" line and any line with fewer than
// 8 value columns are skipped. Returns an error only if the file cannot be read or
// contains no usable per-core line.
func (c *CPUTimeCollector) Collect() (*CPUTimes, error) {
	f, err := os.Open(c.statPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	res := &CPUTimes{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "cpu"):
			fields := strings.Fields(line)
			// Skip the aggregate "cpu" line (label has no digit); emit per-core only.
			if fields[0] == "cpu" {
				continue
			}
			// Need label + user,nice,system,idle,iowait,irq,softirq (>= 8 columns).
			if len(fields) < 8 {
				continue
			}
			v, ok := parseJiffies(fields[1:])
			if !ok {
				continue
			}
			res.PerCPU = append(res.PerCPU, CPUStateTime{
				CPU:     fields[0],
				User:    v[0] / userHZ,
				Nice:    v[1] / userHZ,
				System:  v[2] / userHZ,
				Idle:    v[3] / userHZ,
				Iowait:  v[4] / userHZ,
				Irq:     v[5] / userHZ,
				Softirq: v[6] / userHZ,
				Steal:   v[7] / userHZ, // 0 if column absent (very old kernels)
			})
		case strings.HasPrefix(line, "btime "):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[len("btime "):]), 10, 64); err == nil {
				res.BootTime = n
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(res.PerCPU) == 0 {
		return nil, errors.New("no per-cpu lines found in /proc/stat")
	}
	return res, nil
}

// parseJiffies parses up to 8 columns (user..steal) into float64. Columns beyond what
// is present default to 0 (steal is absent on pre-2.6.11 kernels). Returns false if a
// present column is non-numeric.
func parseJiffies(cols []string) ([8]float64, bool) {
	var out [8]float64
	for i := 0; i < 8 && i < len(cols); i++ {
		n, err := strconv.ParseFloat(cols[i], 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
