package collectors

import (
	"context"
	"errors"
	"io/fs"
	"syscall"
	"testing"
	"time"
)

func i(v int) *int { return &v }

func boolp(b bool) *bool { return &b }

func TestDiskType(t *testing.T) {
	if got := diskType(rawSmart{IsNVMe: true}); got != "NVMe" {
		t.Errorf("NVMe: got %q", got)
	}
	if got := diskType(rawSmart{RotationRate: 0}); got != "SSD" {
		t.Errorf("SSD: got %q", got)
	}
	if got := diskType(rawSmart{RotationRate: 7200}); got != "HDD" {
		t.Errorf("HDD: got %q", got)
	}
}

func TestWearoutNVMe(t *testing.T) {
	w := wearoutUsedPct(rawSmart{IsNVMe: true, NvmePercentageUsed: i(13)})
	if w == nil || *w != 13 {
		t.Errorf("nvme wearout: got %v want 13", w)
	}
}

func TestWearoutSataSSD(t *testing.T) {
	// remaining-life normalized value 100 → 0% used; 91 → 9% used
	w := wearoutUsedPct(rawSmart{RotationRate: 0, AtaWearRemaining: i(91)})
	if w == nil || *w != 9 {
		t.Errorf("sata wearout: got %v want 9", w)
	}
}

func TestWearoutHDDIsNil(t *testing.T) {
	if w := wearoutUsedPct(rawSmart{RotationRate: 7200}); w != nil {
		t.Errorf("hdd wearout should be nil, got %v", w)
	}
}

func TestErrorCount(t *testing.T) {
	if got := errorCount(rawSmart{IsNVMe: true, NvmeMediaErrors: 4}); got != 4 {
		t.Errorf("nvme errors: got %d want 4", got)
	}
	if got := errorCount(rawSmart{RotationRate: 0, AtaErrorCount: 2}); got != 2 {
		t.Errorf("sata errors: got %d want 2", got)
	}
}

func TestSmartPassed(t *testing.T) {
	pass := true
	if !smartPassed(rawSmart{AtaOverallPassed: &pass}) {
		t.Error("ata passed should be true")
	}
	if smartPassed(rawSmart{AtaOverallPassed: boolp(false)}) {
		t.Error("ata overall-failed should return false")
	}
	if smartPassed(rawSmart{IsNVMe: true, NvmeCriticalWarning: 0x01}) {
		t.Error("nvme with critical warning should fail")
	}
	if !smartPassed(rawSmart{IsNVMe: true, NvmeCriticalWarning: 0x00}) {
		t.Error("nvme clean should pass")
	}
}

func TestDeriveDiskSnapshot_NVMe(t *testing.T) {
	r := rawSmart{
		Device: "nvme0n1", IsNVMe: true, Model: "Samsung 980", Serial: "S1",
		CapacityBytes: 512 << 30, PowerOnHours: 2339, NvmePercentageUsed: i(13),
		NvmeMediaErrors: 0, NvmeCriticalWarning: 0, TemperatureC: i(40),
	}
	s := deriveDiskSnapshot(r)
	if s.Type != "NVMe" || s.PowerOnHours != 2339 || !s.SmartPassed || *s.WearoutPct != 13 || s.ErrorCount != 0 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
}

func TestDeriveDiskSnapshot_SataSSD(t *testing.T) {
	r := rawSmart{
		Device: "sda", RotationRate: 0, Model: "Crucial MX500", Serial: "S2",
		CapacityBytes: 1 << 40, PowerOnHours: 11050,
		AtaWearRemaining: i(91), AtaErrorCount: 2, AtaOverallPassed: boolp(true),
	}
	s := deriveDiskSnapshot(r)
	if s.Type != "SSD" || s.PowerOnHours != 11050 || !s.SmartPassed || s.WearoutPct == nil || *s.WearoutPct != 9 || s.ErrorCount != 2 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
}

func TestDeriveDiskSnapshot_HDD(t *testing.T) {
	r := rawSmart{Device: "sdb", RotationRate: 7200, AtaOverallPassed: boolp(true), PowerOnHours: 40000}
	s := deriveDiskSnapshot(r)
	if s.Type != "HDD" || s.WearoutPct != nil || !s.SmartPassed {
		t.Fatalf("unexpected hdd snapshot: %+v", s)
	}
}

type fakeReader struct {
	devices []string
	reads   map[string]rawSmart
	errs    map[string]error
	enumErr error
}

func (f *fakeReader) Enumerate() ([]string, error) {
	if f.enumErr != nil {
		return nil, f.enumErr
	}
	return f.devices, nil
}
func (f *fakeReader) Read(dev string) (rawSmart, error) {
	if e, ok := f.errs[dev]; ok {
		return rawSmart{}, e
	}
	return f.reads[dev], nil
}

func TestCollect_MixedDevices(t *testing.T) {
	fr := &fakeReader{
		devices: []string{"nvme0n1", "sda", "vdb"},
		reads: map[string]rawSmart{
			"nvme0n1": {Device: "nvme0n1", IsNVMe: true, PowerOnHours: 2339, NvmePercentageUsed: i(13)},
			"sda":     {Device: "sda", RotationRate: 0, PowerOnHours: 11050, AtaWearRemaining: i(100), AtaOverallPassed: boolp(true)},
		},
		errs: map[string]error{"vdb": errors.New("no SMART (virtio)")}, // must be skipped silently
	}
	c := &SmartCollector{reader: fr}
	res := c.Collect(context.Background())
	if res.Status != SmartPresent {
		t.Fatalf("status = %v, want present", res.Status)
	}
	if len(res.Disks) != 2 {
		t.Fatalf("got %d disks, want 2 (vdb skipped)", len(res.Disks))
	}
}

func TestCollect_NoReadableDisks(t *testing.T) {
	fr := &fakeReader{devices: []string{"vda"}, errs: map[string]error{"vda": errors.New("no SMART")}}
	c := &SmartCollector{reader: fr}
	if res := c.Collect(context.Background()); res.Status != SmartAbsent {
		t.Fatalf("status = %v, want absent", res.Status)
	}
}

func TestCollect_EnumerateError(t *testing.T) {
	c := &SmartCollector{reader: &fakeReader{enumErr: errors.New("denied")}}
	if res := c.Collect(context.Background()); res.Status != SmartNoPermission {
		t.Fatalf("status = %v, want no-permission", res.Status)
	}
}

func TestCollect_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fr := &fakeReader{
		devices: []string{"sda"},
		reads:   map[string]rawSmart{"sda": {Device: "sda", RotationRate: 0, AtaOverallPassed: boolp(true)}},
	}
	res := (&SmartCollector{reader: fr}).Collect(ctx)
	if res.Status != SmartAbsent { // cancelled before any read → nothing collected
		t.Fatalf("cancelled collect: status = %v, want absent", res.Status)
	}
}

// blockingReader's Read never returns on its own — it simulates a wedged/failing disk
// ioctl (the exact failure class SMART exists to catch).
type blockingReader struct {
	devices []string
	block   chan struct{} // closed to unblock; left open to simulate a permanent hang
}

func (b *blockingReader) Enumerate() ([]string, error) { return b.devices, nil }
func (b *blockingReader) Read(dev string) (rawSmart, error) {
	<-b.block
	return rawSmart{Device: dev}, nil
}

// TestCollect_ReadTimeoutSkipsDevice pins a2: a device Read that blocks past the
// caller's deadline must not block Collect itself — the device is skipped and Collect
// returns as soon as ctx is done, not when (or if) the read eventually completes.
func TestCollect_ReadTimeoutSkipsDevice(t *testing.T) {
	br := &blockingReader{devices: []string{"sda"}, block: make(chan struct{})}
	c := &SmartCollector{reader: br}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan SmartResult, 1)
	go func() { done <- c.Collect(ctx) }()

	select {
	case res := <-done:
		if res.Status != SmartAbsent {
			t.Fatalf("status = %v, want SmartAbsent (device skipped on timeout)", res.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Collect did not return within 2s of a 20ms ctx timeout — read is not bounded")
	}
}

// TestCollect_AllPermissionDeniedYieldsNoPermission pins a3: when every attempted
// device fails with a permission error (EACCES/EPERM — the realistic misconfiguration:
// missing disk-group/capabilities), Collect must report SmartNoPermission, not the
// generic SmartAbsent that produces no diagnostic trail under mode=auto.
func TestCollect_AllPermissionDeniedYieldsNoPermission(t *testing.T) {
	fr := &fakeReader{
		devices: []string{"sda", "sdb"},
		errs: map[string]error{
			"sda": &fs.PathError{Op: "open", Path: "/dev/sda", Err: syscall.EACCES},
			"sdb": &fs.PathError{Op: "open", Path: "/dev/sdb", Err: syscall.EPERM},
		},
	}
	res := (&SmartCollector{reader: fr}).Collect(context.Background())
	if res.Status != SmartNoPermission {
		t.Fatalf("status = %v, want SmartNoPermission", res.Status)
	}
}

// A mix of permission and non-permission errors (e.g. a genuinely SMART-less virtio
// device alongside a permission-denied one) is not a clean "all denied" signal, so it
// stays SmartAbsent rather than claiming a permission wall.
func TestCollect_MixedErrorsYieldAbsent(t *testing.T) {
	fr := &fakeReader{
		devices: []string{"sda", "vdb"},
		errs: map[string]error{
			"sda": &fs.PathError{Op: "open", Path: "/dev/sda", Err: syscall.EACCES},
			"vdb": errors.New("no SMART (virtio)"),
		},
	}
	res := (&SmartCollector{reader: fr}).Collect(context.Background())
	if res.Status != SmartAbsent {
		t.Fatalf("status = %v, want SmartAbsent (mixed failure reasons)", res.Status)
	}
}
