package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

type fakeConn struct {
	units     []sd.UnitStatus
	unitProps map[string]map[string]any
	svcProps  map[string]map[string]any
	listErr   error
	closed    int
}

func (f *fakeConn) ListUnitsByPatternsContext(_ context.Context, _ []string, _ []string) ([]sd.UnitStatus, error) {
	return f.units, f.listErr
}
func (f *fakeConn) GetUnitPropertiesContext(_ context.Context, unit string) (map[string]any, error) {
	return f.unitProps[unit], nil
}
func (f *fakeConn) GetUnitTypePropertiesContext(_ context.Context, unit, _ string) (map[string]any, error) {
	return f.svcProps[unit], nil
}
func (f *fakeConn) Close() { f.closed++ }

// discardLogger (defined in docker_test.go, same package) keeps test output pristine.

func newCollectorWithConn(c systemdConn) *SystemdCollector {
	col := NewSystemdCollector(discardLogger())
	col.newConn = func(context.Context) (systemdConn, error) { return c, nil }
	return col
}

func baseFake() *fakeConn {
	return &fakeConn{
		units: []sd.UnitStatus{{Name: "nginx.service", ActiveState: "active", SubState: "running", Description: "web"}},
		unitProps: map[string]map[string]any{
			"nginx.service": {
				"Description":          "web",
				"FragmentPath":         "/lib/systemd/system/nginx.service",
				"UnitFileState":        "enabled",
				"ActiveEnterTimestamp": uint64(1_719_300_000_000_000), // µs since epoch
				"After":                []string{"network.target"},
			},
		},
		svcProps: map[string]map[string]any{
			"nginx.service": {
				"CPUUsageNSec":  uint64(1_000_000_000),
				"MemoryCurrent": uint64(12_582_912),
				"MemoryPeak":    uint64(20_971_520),
				"NRestarts":     uint32(0),
				"TasksCurrent":  uint64(3),
				"MainPID":       uint32(1234),
			},
		},
	}
}

func TestCollectMapsFields(t *testing.T) {
	c := newCollectorWithConn(baseFake())
	res := c.Collect(context.Background())
	if res.Status != SystemdPresent {
		t.Fatalf("Status = %v, want SystemdPresent", res.Status)
	}
	if len(res.Services) != 1 {
		t.Fatalf("len(Services) = %d, want 1", len(res.Services))
	}
	s := res.Services[0]
	if s.Name != "nginx.service" || s.State != "active" || s.SubState != "running" {
		t.Errorf("identity = %+v", s)
	}
	if s.MemoryBytes == nil || *s.MemoryBytes != 12_582_912 {
		t.Errorf("MemoryBytes = %v", s.MemoryBytes)
	}
	if s.CPUPercent != nil {
		t.Errorf("CPUPercent on first sample = %v, want nil", s.CPUPercent)
	}
	if s.EnabledOnBoot != true || s.Details.MainPID != 1234 {
		t.Errorf("details = %+v enabled=%v", s.Details, s.EnabledOnBoot)
	}
}

func TestCollectCPUPercentDelta(t *testing.T) {
	f := baseFake()
	c := newCollectorWithConn(f)
	c.Collect(context.Background()) // first sample → no prev
	// advance cumulative CPU by 2s of CPU time
	f.svcProps["nginx.service"]["CPUUsageNSec"] = uint64(3_000_000_000)
	res := c.Collect(context.Background())
	if res.Services[0].CPUPercent == nil {
		t.Fatal("CPUPercent = nil on second sample, want a value")
	}
	if *res.Services[0].CPUPercent <= 0 {
		t.Errorf("CPUPercent = %v, want > 0", *res.Services[0].CPUPercent)
	}
}

func TestCollectMemorySentinel(t *testing.T) {
	f := baseFake()
	f.svcProps["nginx.service"]["MemoryCurrent"] = uint64(math.MaxUint64) // accounting off
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if res.Services[0].MemoryBytes != nil {
		t.Errorf("MemoryBytes = %v, want nil for UINT64_MAX sentinel", res.Services[0].MemoryBytes)
	}
}

func TestCollectAbsentWhenNoConn(t *testing.T) {
	c := NewSystemdCollector(discardLogger())
	c.newConn = func(context.Context) (systemdConn, error) { return nil, errConnFailed }
	res := c.Collect(context.Background())
	if res.Status != SystemdAbsent {
		t.Errorf("Status = %v, want SystemdAbsent", res.Status)
	}
}

// A genuine permission wall surfaces as a godbus dbus.Error named AccessDenied.
// That — and only that — must be classified as SystemdNoPermission, with the
// underlying error carried out so the receiver can log it.
func TestCollectAccessDeniedIsNoPermission(t *testing.T) {
	f := baseFake()
	f.listErr = dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"Rejected send message"}}
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if res.Status != SystemdNoPermission {
		t.Errorf("Status = %v, want SystemdNoPermission", res.Status)
	}
	if res.Err == nil {
		t.Fatal("Err = nil, want the underlying AccessDenied error")
	}
	var dbusErr dbus.Error
	if !errors.As(res.Err, &dbusErr) || dbusErr.Name != "org.freedesktop.DBus.Error.AccessDenied" {
		t.Errorf("Err = %v, want the injected AccessDenied dbus.Error", res.Err)
	}
	if f.closed == 0 {
		t.Error("conn not closed on error path; re-dial-next-cycle behavior lost")
	}
}

// AccessDenied wrapped by an intermediate layer must still be detected via errors.As.
func TestCollectWrappedAccessDeniedIsNoPermission(t *testing.T) {
	f := baseFake()
	f.listErr = fmt.Errorf("list units: %w", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"})
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if res.Status != SystemdNoPermission {
		t.Errorf("Status = %v, want SystemdNoPermission for wrapped AccessDenied", res.Status)
	}
}

// A plain (non-AccessDenied) error is transient: distinct status, error carried out.
func TestCollectTransientEnumError(t *testing.T) {
	f := baseFake()
	f.listErr = errors.New("dbus: connection closed by user")
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if res.Status != SystemdEnumError {
		t.Errorf("Status = %v, want SystemdEnumError", res.Status)
	}
	if res.Err == nil || res.Err.Error() != "dbus: connection closed by user" {
		t.Errorf("Err = %v, want the injected transient error", res.Err)
	}
	if f.closed == 0 {
		t.Error("conn not closed on error path")
	}
}

// A context deadline (systemd.timeout) is transient, not a permission problem.
func TestCollectDeadlineIsTransient(t *testing.T) {
	f := baseFake()
	f.listErr = context.DeadlineExceeded
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if res.Status != SystemdEnumError {
		t.Errorf("Status = %v, want SystemdEnumError for DeadlineExceeded", res.Status)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want context.DeadlineExceeded", res.Err)
	}
}

// A non-AccessDenied dbus.Error (e.g. NoReply) is transient, not a permission wall.
func TestCollectOtherDbusErrorIsTransient(t *testing.T) {
	f := baseFake()
	f.listErr = dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"}
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if res.Status != SystemdEnumError {
		t.Errorf("Status = %v, want SystemdEnumError for non-AccessDenied dbus.Error", res.Status)
	}
}

// dial-per-snapshot: each Collect dials a fresh connection and closes it before
// returning, so no connection lingers across the multi-minute idle interval. A
// lingering conn gets torn down when the receiver cancels the per-call context
// (go-systemd binds the conn lifetime to that context), then fails the next
// enumeration with ErrClosed — the fleet-wide ~6-min flap this fix removes.
func TestCollectDialsFreshAndClosesEachCycle(t *testing.T) {
	f := baseFake()
	var dials int
	c := NewSystemdCollector(discardLogger())
	c.newConn = func(context.Context) (systemdConn, error) {
		dials++
		return f, nil
	}
	c.Collect(context.Background())
	c.Collect(context.Background())
	if dials != 2 {
		t.Errorf("newConn called %d times, want 2 (fresh dial per cycle)", dials)
	}
	if f.closed != 2 {
		t.Errorf("conn closed %d times, want 2 (closed after every cycle)", f.closed)
	}
}

// Success path carries no error.
func TestCollectSuccessHasNoErr(t *testing.T) {
	c := newCollectorWithConn(baseFake())
	res := c.Collect(context.Background())
	if res.Status != SystemdPresent {
		t.Fatalf("Status = %v, want SystemdPresent", res.Status)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil on success", res.Err)
	}
}

func TestCollectTasksMaxSentinel(t *testing.T) {
	t.Run("UINT64_MAX yields nil", func(t *testing.T) {
		f := baseFake()
		f.svcProps["nginx.service"]["TasksMax"] = uint64(math.MaxUint64)
		c := newCollectorWithConn(f)
		res := c.Collect(context.Background())
		if res.Services[0].Details.TasksMax != nil {
			t.Errorf("TasksMax = %v, want nil for UINT64_MAX sentinel", res.Services[0].Details.TasksMax)
		}
	})

	t.Run("normal value yields pointer", func(t *testing.T) {
		f := baseFake()
		f.svcProps["nginx.service"]["TasksMax"] = uint64(4915)
		c := newCollectorWithConn(f)
		res := c.Collect(context.Background())
		got := res.Services[0].Details.TasksMax
		if got == nil {
			t.Fatal("TasksMax = nil, want pointer to 4915")
		}
		if *got != 4915 {
			t.Errorf("TasksMax = %d, want 4915", *got)
		}
	})
}

// manyUnits builds n minimal active units, all resolvable in unitProps/svcProps so
// Collect doesn't hit the "missing key" defaults.
func manyUnits(n int) ([]sd.UnitStatus, map[string]map[string]any) {
	units := make([]sd.UnitStatus, n)
	props := make(map[string]map[string]any, n)
	for i := range n {
		name := fmt.Sprintf("svc%d.service", i)
		units[i] = sd.UnitStatus{Name: name, ActiveState: "active", SubState: "running"}
		props[name] = map[string]any{"Description": "d"}
	}
	return units, props
}

// TestCollectCapsUnitsAt500 pins a27: an unbounded unit list produces an unbounded,
// ever-repeating OTLP payload — Collect must cap it.
func TestCollectCapsUnitsAt500(t *testing.T) {
	units, props := manyUnits(600)
	f := &fakeConn{units: units, unitProps: props, svcProps: props}
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if len(res.Services) != maxUnits {
		t.Fatalf("len(Services) = %d, want %d", len(res.Services), maxUnits)
	}
}

// TestCollectOrdersFailedUnitsFirstBeforeCap pins b16: failed units must be ordered
// first (stable — ties keep D-Bus order), before the newest/oldest-agnostic cap runs,
// so a cap on a host with many active units never hides a failed one. 599 active units
// precede one failed unit in D-Bus order; without the reorder, the failed unit (at
// index 599, past the 500 cap) would be dropped.
func TestCollectOrdersFailedUnitsFirstBeforeCap(t *testing.T) {
	units, props := manyUnits(599)
	failed := sd.UnitStatus{Name: "broken.service", ActiveState: "failed", SubState: "failed"}
	units = append(units, failed)
	props["broken.service"] = map[string]any{"Description": "d"}

	f := &fakeConn{units: units, unitProps: props, svcProps: props}
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())

	if len(res.Services) != maxUnits {
		t.Fatalf("len(Services) = %d, want %d", len(res.Services), maxUnits)
	}
	found := false
	for _, s := range res.Services {
		if s.Name == "broken.service" {
			found = true
			break
		}
	}
	if !found {
		t.Error("broken.service (the only failed unit, positioned past the cap in D-Bus order) was dropped by truncation")
	}
}

// TestCollectWarnsOnceOnUnitTruncation pins the "WARN once" half of a27.
func TestCollectWarnsOnceOnUnitTruncation(t *testing.T) {
	units, props := manyUnits(600)
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	f := &fakeConn{units: units, unitProps: props, svcProps: props}
	c := NewSystemdCollector(logger)
	c.newConn = func(context.Context) (systemdConn, error) { return f, nil }

	c.Collect(context.Background())
	c.Collect(context.Background())

	if got := strings.Count(buf.String(), "truncated"); got != 1 {
		t.Errorf("truncation WARN logged %d times, want 1 (once, not per cycle)", got)
	}
}

// TestCollectTruncatesDescription pins the 256-rune Description bound.
func TestCollectTruncatesDescription(t *testing.T) {
	f := baseFake()
	long := strings.Repeat("d", 300)
	f.unitProps["nginx.service"]["Description"] = long
	c := newCollectorWithConn(f)
	res := c.Collect(context.Background())
	if got := len([]rune(res.Services[0].Description)); got != maxDescriptionRunes {
		t.Errorf("Description length = %d, want %d (truncated)", got, maxDescriptionRunes)
	}
}

// TestCollectLogsPropertyFetchFailuresOncePerCycle pins a28: per-unit property errors
// must not be silently discarded, but must also not spam one log line per unit —
// a single Debug line with the failure count, once per Collect call.
func TestCollectLogsPropertyFetchFailuresOncePerCycle(t *testing.T) {
	units := []sd.UnitStatus{
		{Name: "ok.service", ActiveState: "active", SubState: "running"},
		{Name: "bad1.service", ActiveState: "active", SubState: "running"},
		{Name: "bad2.service", ActiveState: "active", SubState: "running"},
	}
	f := &erroringPropsConn{
		fakeConn: fakeConn{units: units},
		failFor:  map[string]bool{"bad1.service": true, "bad2.service": true},
	}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewSystemdCollector(logger)
	c.newConn = func(context.Context) (systemdConn, error) { return f, nil }
	c.Collect(context.Background())

	out := buf.String()
	if got := strings.Count(out, "property fetch"); got != 1 {
		t.Fatalf("property-fetch-failure log lines = %d, want 1 (once per cycle): %s", got, out)
	}
	if !strings.Contains(out, "count=4") { // 2 units × (uProps+sProps) each erroring = 4
		t.Errorf("log line missing the failure count: %s", out)
	}
}

// erroringPropsConn wraps fakeConn so GetUnitPropertiesContext/
// GetUnitTypePropertiesContext fail for the named units.
type erroringPropsConn struct {
	fakeConn
	failFor map[string]bool
}

func (f *erroringPropsConn) GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error) {
	if f.failFor[unit] {
		return nil, errors.New("boom")
	}
	return f.fakeConn.GetUnitPropertiesContext(ctx, unit)
}

func (f *erroringPropsConn) GetUnitTypePropertiesContext(ctx context.Context, unit, t string) (map[string]any, error) {
	if f.failFor[unit] {
		return nil, errors.New("boom")
	}
	return f.fakeConn.GetUnitTypePropertiesContext(ctx, unit, t)
}
