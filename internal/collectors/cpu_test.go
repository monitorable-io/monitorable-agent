package collectors

import (
	"os"
	"path/filepath"
	"testing"
)

// writeStat writes content to a temp file and returns its path.
func writeStat(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "stat")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// LXC guest /proc/stat: sparse, non-contiguous host CPU IDs, aggregate line first.
const lxcStat = `cpu  100 0 200 300 40 0 8 0 0 0
cpu8 10 0 20 30 4 0 1 0 0 0
cpu44 90 0 180 270 36 0 7 0 0 0
intr 123 0 0
ctxt 456
btime 1777625254
processes 789
procs_running 2
procs_blocked 0
`

func TestCPUTimeCollect_ParsesPerCoreSparseLabels(t *testing.T) {
	c := &CPUTimeCollector{statPath: writeStat(t, lxcStat)}
	got, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got.PerCPU) != 2 {
		t.Fatalf("want 2 per-cpu entries (aggregate skipped), got %d", len(got.PerCPU))
	}
	if got.PerCPU[0].CPU != "cpu8" || got.PerCPU[1].CPU != "cpu44" {
		t.Fatalf("want labels [cpu8 cpu44], got [%s %s]", got.PerCPU[0].CPU, got.PerCPU[1].CPU)
	}
	// cpu8 user = 10 jiffies / 100 = 0.10s ; wait(iowait)=4/100=0.04 ; steal=0
	if got.PerCPU[0].User != 0.10 || got.PerCPU[0].Iowait != 0.04 || got.PerCPU[0].Steal != 0 {
		t.Fatalf("cpu8 seconds wrong: %+v", got.PerCPU[0])
	}
	if got.BootTime != 1777625254 {
		t.Fatalf("btime want 1777625254, got %d", got.BootTime)
	}
}

// Two snapshots with DIFFERENT cpu sets (cpuset re-pinned: cpu0 disappears, cpu53
// appears). The collector must succeed both times — proving it cannot wedge the way
// the upstream ucal scraper does.
func TestCPUTimeCollect_SurvivesCPUSetChange(t *testing.T) {
	before := "cpu  1 0 1 1 0 0 0 0 0 0\ncpu0 1 0 1 1 0 0 0 0 0 0\ncpu8 1 0 1 1 0 0 0 0 0 0\nbtime 1\n"
	after := "cpu  2 0 2 2 0 0 0 0 0 0\ncpu8 2 0 2 2 0 0 0 0 0 0\ncpu53 2 0 2 2 0 0 0 0 0 0\nbtime 1\n"

	c1 := &CPUTimeCollector{statPath: writeStat(t, before)}
	if _, err := c1.Collect(); err != nil {
		t.Fatalf("first collect: %v", err)
	}
	c2 := &CPUTimeCollector{statPath: writeStat(t, after)}
	got, err := c2.Collect()
	if err != nil {
		t.Fatalf("second collect after cpu-set change must not error, got: %v", err)
	}
	if len(got.PerCPU) != 2 || got.PerCPU[0].CPU != "cpu8" || got.PerCPU[1].CPU != "cpu53" {
		t.Fatalf("want [cpu8 cpu53], got %+v", got.PerCPU)
	}
}

func TestCPUTimeCollect_SkipsMalformedLines(t *testing.T) {
	stat := "cpu  1 0 1 1 0 0 0 0\ncpu0 1 0 1\ncpu1 5 0 5 5 1 0 1\nbtime 1\n"
	c := &CPUTimeCollector{statPath: writeStat(t, stat)}
	got, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// cpu0 has only 3 columns -> skipped; cpu1 has 7 value columns -> kept (steal=0).
	if len(got.PerCPU) != 1 || got.PerCPU[0].CPU != "cpu1" {
		t.Fatalf("want only cpu1, got %+v", got.PerCPU)
	}
	if got.PerCPU[0].System != 0.05 || got.PerCPU[0].Steal != 0 {
		t.Fatalf("cpu1 values wrong: %+v", got.PerCPU[0])
	}
}

func TestCPUTimeCollect_NoPerCPUIsError(t *testing.T) {
	c := &CPUTimeCollector{statPath: writeStat(t, "cpu  1 0 1 1 0 0 0 0 0 0\nbtime 1\n")}
	if _, err := c.Collect(); err == nil {
		t.Fatal("want error when only the aggregate line is present")
	}
}
