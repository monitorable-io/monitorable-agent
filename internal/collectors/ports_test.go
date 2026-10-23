package collectors

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// procNetLine renders one row in the kernel's /proc/net/tcp{,6} format: each
// 4-byte address word printed as a native (little-endian) integer.
func procNetLine(i int, r tcpRow, v6 bool) string {
	var raw []byte
	if v6 {
		a := r.Local.As16()
		raw = a[:]
	} else {
		a := r.Local.As4()
		raw = a[:]
	}
	// Each 4-byte word is printed as a little-endian integer.
	hexAddr := make([]byte, 0, len(raw)*2)
	for j := 0; j < len(raw); j += 4 {
		hexAddr = fmt.Appendf(hexAddr, "%08X", binary.LittleEndian.Uint32(raw[j:]))
	}
	zero := strings.Repeat("0", len(hexAddr))
	return fmt.Sprintf("%4d: %s:%04X %s:0000 %02X 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 100 0 0 10 0\n",
		i, hexAddr, r.LocalPort, zero, r.State, 1000+i)
}

// writeProcNet writes <root>/net/tcp (and net/tcp6 when v6 != nil) from rows.
func writeProcNet(t *testing.T, root string, v4, v6 []tcpRow) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, rows []tcpRow, isV6 bool) {
		var sb strings.Builder
		sb.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
		for i, r := range rows {
			sb.WriteString(procNetLine(i, r, isV6))
		}
		if err := os.WriteFile(filepath.Join(root, "net", name), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tcp", v4, false)
	if v6 != nil {
		write("tcp6", v6, true)
	}
}

func listenRow(addr string, port uint16) tcpRow { return row(addr, port, tcpStateListen) }

func estRows(addr string, port uint16, n int) []tcpRow {
	rows := make([]tcpRow, n)
	for i := range rows {
		rows[i] = row(addr, port, tcpStateEstablished)
	}
	return rows
}

func newTestPorts(t *testing.T) (*PortsCollector, string) {
	t.Helper()
	root := t.TempDir()
	return NewPortsCollector(root, "testdata/services.txt", nil), root
}

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 123_000_000, time.UTC)

func sampleAt(t *testing.T, c *PortsCollector, i int) {
	t.Helper()
	if err := c.Sample(context.Background(), t0.Add(time.Duration(i)*10*time.Second)); err != nil {
		t.Fatalf("Sample %d: %v", i, err)
	}
}

func TestPortsCollector_SampleAndSnapshot(t *testing.T) {
	c, root := newTestPorts(t)
	for i, n := range []int{1, 1, 2, 2, 1, 1} {
		v4 := append([]tcpRow{listenRow("0.0.0.0", 22)}, estRows("10.0.0.5", 22, n)...)
		writeProcNet(t, root, v4, []tcpRow{listenRow("::", 22)})
		sampleAt(t, c, i)
	}
	res := c.Snapshot(30)
	if res.Status != PortsOK || res.Snapshot == nil {
		t.Fatalf("status = %v", res.Status)
	}
	if len(res.Snapshot.Ports) != 1 {
		t.Fatalf("ports = %+v", res.Snapshot.Ports)
	}
	p := res.Snapshot.Ports[0]
	if p.Port != 22 || p.Service != "ssh" || p.Scope != "exposed" || p.Families != "dual" {
		t.Errorf("detail = %+v", p)
	}
	if p.ConnAvg < 1.3333 || p.ConnAvg > 1.3334 || p.ConnPeak != 2 || p.Samples != 6 {
		t.Errorf("stats = avg %v peak %d samples %d", p.ConnAvg, p.ConnPeak, p.Samples)
	}
}

// runWindow writes and samples one /proc/net/tcp per entry of perSample.
func runWindow(t *testing.T, c *PortsCollector, root string, perSample [][]tcpRow) {
	t.Helper()
	for i, rows := range perSample {
		writeProcNet(t, root, rows, []tcpRow{})
		sampleAt(t, c, i)
	}
}

func port8080Rows(listening bool, conns int) []tcpRow {
	rows := []tcpRow{listenRow("0.0.0.0", 22)}
	if listening {
		rows = append(rows, listenRow("0.0.0.0", 8080))
	}
	return append(rows, estRows("10.0.0.5", 8080, conns)...)
}

func TestPortsCollector_PortListeningInSomeSamples(t *testing.T) {
	c, root := newTestPorts(t)
	// 8080 listens in samples 2,3,4 (counts 0,5,7) and in the last (count 0 too).
	runWindow(t, c, root, [][]tcpRow{
		port8080Rows(false, 0),
		port8080Rows(true, 0),
		port8080Rows(true, 5),
		port8080Rows(true, 7),
		port8080Rows(false, 0),
		port8080Rows(true, 0),
	})
	snap := c.Snapshot(30).Snapshot
	if len(snap.AllPorts) != 2 || snap.AllPorts[1] != 8080 || len(snap.Ports) != 2 {
		t.Fatalf("AllPorts = %v Ports = %+v", snap.AllPorts, snap.Ports)
	}
	p := snap.Ports[1]
	if p.Port != 8080 || p.ConnAvg != 3 || p.ConnPeak != 7 || p.Samples != 4 {
		t.Errorf("8080 = %+v, want avg 3 peak 7 samples 4", p)
	}
}

func TestPortsCollector_ListeningOnlyInMiddleSamples(t *testing.T) {
	c, root := newTestPorts(t)
	// Samples 2,3,4 only: counts 0,5,7 -> avg 4, peak 7, samples 3.
	runWindow(t, c, root, [][]tcpRow{
		port8080Rows(false, 0),
		port8080Rows(true, 0),
		port8080Rows(true, 5),
		port8080Rows(true, 7),
	})
	p := c.Snapshot(30).Snapshot.Ports[1]
	if p.Port != 8080 || p.ConnAvg != 4 || p.ConnPeak != 7 || p.Samples != 3 {
		t.Errorf("8080 = %+v, want avg 4 peak 7 samples 3", p)
	}
}

func TestPortsCollector_LastSampleDecidesPresence(t *testing.T) {
	c, root := newTestPorts(t)
	// 8080 listens in samples 1-5 but not in the 6th; its stale peers don't matter.
	runWindow(t, c, root, [][]tcpRow{
		port8080Rows(true, 2), port8080Rows(true, 2), port8080Rows(true, 2),
		port8080Rows(true, 2), port8080Rows(true, 2), port8080Rows(false, 3),
	})
	snap := c.Snapshot(30).Snapshot
	if len(snap.AllPorts) != 1 || snap.AllPorts[0] != 22 || len(snap.Ports) != 1 || snap.Ports[0].Port != 22 {
		t.Fatalf("AllPorts = %v Ports = %+v, want only 22", snap.AllPorts, snap.Ports)
	}
}

func manyListeners(n int) []tcpRow {
	rows := make([]tcpRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, listenRow("0.0.0.0", uint16(1000+i)))
	}
	return rows
}

func TestPortsCollector_CapKeepsLowestPorts(t *testing.T) {
	c, root := newTestPorts(t)
	rows := manyListeners(41)
	// Shuffle-ish: reverse so file order is not ascending.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	writeProcNet(t, root, rows, []tcpRow{})
	sampleAt(t, c, 0)
	s := c.Snapshot(30).Snapshot
	if len(s.AllPorts) != 41 || s.AllPortsTruncated {
		t.Fatalf("AllPorts len %d truncated %v", len(s.AllPorts), s.AllPortsTruncated)
	}
	for i, p := range s.AllPorts {
		if p != 1000+i {
			t.Fatalf("AllPorts[%d] = %d, want ascending", i, p)
		}
	}
	if len(s.Ports) != 30 {
		t.Fatalf("len(Ports) = %d", len(s.Ports))
	}
	for i, d := range s.Ports {
		if d.Port != 1000+i {
			t.Fatalf("Ports[%d] = %d, want the 30 lowest", i, d.Port)
		}
	}
}

func TestPortsCollector_AllPortsTruncatedAt1000(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, manyListeners(1001), []tcpRow{})
	sampleAt(t, c, 0)
	s := c.Snapshot(30).Snapshot
	if len(s.AllPorts) != MaxAllPorts || !s.AllPortsTruncated {
		t.Fatalf("AllPorts len %d truncated %v", len(s.AllPorts), s.AllPortsTruncated)
	}
	if len(s.Ports) != 30 || s.Ports[0].Port != 1000 || s.Ports[29].Port != 1029 {
		t.Fatalf("Ports = %d rows, first %d", len(s.Ports), s.Ports[0].Port)
	}
}

func TestPortsCollector_SnapshotResetsWindow(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("0.0.0.0", 22)}, []tcpRow{})
	sampleAt(t, c, 0)
	if res := c.Snapshot(30); res.Status != PortsOK {
		t.Fatalf("first status = %v", res.Status)
	}
	res := c.Snapshot(30)
	if res.Status != PortsNoSamples || res.Snapshot != nil {
		t.Errorf("second = %+v, want PortsNoSamples", res)
	}
}

func TestPortsCollector_UnreadableTable(t *testing.T) {
	c := NewPortsCollector(t.TempDir(), "testdata/services.txt", nil)
	if err := c.Sample(context.Background(), t0); err == nil {
		t.Fatal("Sample without net/tcp returned nil")
	}
	res := c.Snapshot(30)
	if res.Status != PortsUnreadable || res.Err == nil {
		t.Errorf("result = %+v, want PortsUnreadable with Err", res)
	}
}

func TestPortsCollector_EmptyTableIsAnOKEmptySnapshot(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{}, []tcpRow{}) // headers only: a host with no listeners
	sampleAt(t, c, 0)
	res := c.Snapshot(30)
	if res.Status != PortsOK || res.Snapshot == nil || len(res.Snapshot.Ports) != 0 {
		t.Fatalf("result = %+v, want OK with no ports", res)
	}
	b, err := json.Marshal(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"all_ports":[]`) {
		t.Errorf("json = %s, want all_ports: []", b)
	}
}

func TestPortsCollector_AllMalformedTableSkipsSample(t *testing.T) {
	c, root := newTestPorts(t)
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Unknown format: must not read as "no listeners" (the backend would drop every port).
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte("hdr\nnot a socket row\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Sample(context.Background(), t0); err == nil {
		t.Fatal("Sample of an unparseable table returned nil")
	}
	if res := c.Snapshot(30); res.Status != PortsUnreadable || res.Err == nil {
		t.Errorf("result = %+v, want PortsUnreadable", res)
	}
}

func TestPortsCollector_FailedSampleNotInWindow(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("0.0.0.0", 22)}, []tcpRow{})
	sampleAt(t, c, 0)
	if err := os.Remove(filepath.Join(root, "net", "tcp")); err != nil {
		t.Fatal(err)
	}
	if err := c.Sample(context.Background(), t0.Add(10*time.Second)); err == nil {
		t.Fatal("expected error")
	}
	res := c.Snapshot(30)
	if res.Status != PortsOK || res.Snapshot.Ports[0].Samples != 1 || res.Snapshot.CollectedAtUnixMs != t0.UnixMilli() {
		t.Errorf("result = %+v", res)
	}
}

func TestPortsCollector_NoSamplesYet(t *testing.T) {
	c, _ := newTestPorts(t)
	if res := c.Snapshot(30); res.Status != PortsNoSamples {
		t.Errorf("status = %v", res.Status)
	}
}

func TestPortsCollector_CancelledContext(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("0.0.0.0", 22)}, []tcpRow{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sample(ctx, t0); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// expiresAfterFirstCheck reports a live context on the first Err call (the
// pre-read check) and DeadlineExceeded afterwards, i.e. the deadline passes
// while /proc is being read.
type expiresAfterFirstCheck struct {
	context.Context
	calls int
}

func (c *expiresAfterFirstCheck) Err() error {
	c.calls++
	if c.calls == 1 {
		return nil
	}
	return context.DeadlineExceeded
}

func TestPortsCollector_OverDeadlineSampleSkipped(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("0.0.0.0", 22)}, []tcpRow{})
	ctx := &expiresAfterFirstCheck{Context: context.Background()}
	if err := c.Sample(ctx, t0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	res := c.Snapshot(30)
	if res.Status != PortsUnreadable || !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("result = %+v, want PortsUnreadable (late sample not counted)", res)
	}
}

func TestPortsCollector_MissingTCP6IsFine(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("127.0.0.1", 5432)}, nil)
	if err := c.Sample(context.Background(), t0); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	p := c.Snapshot(30).Snapshot.Ports[0]
	if p.Scope != "local" || p.Families != "v4" {
		t.Errorf("detail = %+v", p)
	}
}

func TestPortsCollector_CollectedAtIsLastSample(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("0.0.0.0", 22)}, []tcpRow{})
	for i := 0; i < 3; i++ {
		sampleAt(t, c, i)
	}
	got := c.Snapshot(30).Snapshot.CollectedAtUnixMs
	if want := t0.Add(20 * time.Second).UnixMilli(); got != want {
		t.Errorf("CollectedAtUnixMs = %d, want %d", got, want)
	}
}

func TestPortsCollector_V6OnlyAndNoPrivateData(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{}, []tcpRow{listenRow("::1", 631), row("::ffff:10.1.2.3", 631, tcpStateEstablished)})
	sampleAt(t, c, 0)
	p := c.Snapshot(30).Snapshot.Ports[0]
	if p.Scope != "local" || p.Families != "v6" || p.ConnPeak != 1 {
		t.Errorf("detail = %+v", p)
	}
}

func TestPortsCollector_Concurrent(t *testing.T) {
	c, root := newTestPorts(t)
	writeProcNet(t, root, []tcpRow{listenRow("0.0.0.0", 22)}, []tcpRow{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = c.Sample(context.Background(), t0.Add(time.Duration(i)*time.Second))
		}
	}()
	for i := 0; i < 20; i++ {
		_ = c.Snapshot(30)
	}
	<-done
}

func TestLoadServices(t *testing.T) {
	m := loadServices("testdata/services.txt")
	if m[22] != "ssh" || m[80] != "http" || m[53] != "domain" || m[1] != "tcpmux" {
		t.Errorf("services = %v", m)
	}
	if len(m) != 4 {
		t.Errorf("len = %d, want 4 (udp-only, bad lines skipped): %v", len(m), m)
	}
	if m := loadServices(filepath.Join(t.TempDir(), "nope")); m == nil || len(m) != 0 {
		t.Errorf("missing file = %v, want empty non-nil map", m)
	}
}
