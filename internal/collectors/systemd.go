package collectors

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

var errConnFailed = errors.New("systemd: dbus connection failed")

// maxUnits caps the unit list emitted in one snapshot; a host with many transient/
// templated units must not inflate agent memory or produce an unbounded, ever-
// repeating OTLP payload.
const maxUnits = 500

// maxDescriptionRunes bounds Description — sourced from the unit file with no
// length guarantee.
const maxDescriptionRunes = 256

// dbusAccessDenied is the godbus error name for a genuine permission wall. A
// real denial (D-Bus/polkit policy blocking the collector user) surfaces as a
// dbus.Error with this name; everything else (timeouts, a stale/closed
// connection, other D-Bus faults) is transient.
const dbusAccessDenied = "org.freedesktop.DBus.Error.AccessDenied"

// SystemdStatus is the outcome of one systemd collection attempt.
type SystemdStatus int

const (
	SystemdAbsent       SystemdStatus = iota // dbus unreachable / no systemd
	SystemdNoPermission                      // connected but enumeration truly denied (AccessDenied)
	SystemdPresent                           // snapshot collected (possibly empty)
	SystemdEnumError                         // connected but enumeration failed transiently (timeout, stale conn, ...)
)

// activeFailedStates is the D-Bus state filter: active + failed only.
var activeFailedStates = []string{"active", "activating", "deactivating", "reloading", "failed"}

// ServiceDetails is the drill-down blob (stored as JSONB, returned only by the detail RPC).
type ServiceDetails struct {
	MainPID      int64    `json:"main_pid,omitempty"`
	TasksCurrent int64    `json:"tasks_current,omitempty"`
	TasksMax     *int64   `json:"tasks_max,omitempty"`
	FragmentPath string   `json:"fragment_path,omitempty"`
	Wants        []string `json:"wants,omitempty"`
	Requires     []string `json:"requires,omitempty"`
	Before       []string `json:"before,omitempty"`
	After        []string `json:"after,omitempty"`
}

// ServiceSnapshot is one service's current values. nil pointers mean "unavailable".
type ServiceSnapshot struct {
	Name              string         `json:"name"`
	State             string         `json:"state"`
	SubState          string         `json:"sub_state"`
	CPUPercent        *float64       `json:"cpu_percent,omitempty"`
	MemoryBytes       *int64         `json:"memory_bytes,omitempty"`
	MemoryPeakBytes   *int64         `json:"memory_peak_bytes,omitempty"`
	RestartCount      int            `json:"restart_count"`
	ActiveSinceUnixMs int64          `json:"active_since_unix_ms,omitempty"`
	EnabledOnBoot     bool           `json:"enabled_on_boot"`
	Description       string         `json:"description,omitempty"`
	Details           ServiceDetails `json:"details"`
}

// SystemdResult is the outcome of SystemdCollector.Collect.
type SystemdResult struct {
	Status   SystemdStatus
	Services []ServiceSnapshot
	// Err carries the underlying failure on the NoPermission/EnumError paths so
	// the receiver can log the real D-Bus error. It is nil on the Present and
	// Absent paths.
	Err error
}

// classifyEnumErr maps a unit-enumeration failure to a status. A genuine
// permission wall surfaces as a godbus dbus.Error named AccessDenied; only that
// is SystemdNoPermission. Every other failure — a context deadline/cancel from
// systemd.timeout, a stale/closed connection, or any other D-Bus fault — is
// transient (SystemdEnumError) and retried on the next cycle.
func classifyEnumErr(err error) SystemdStatus {
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) && dbusErr.Name == dbusAccessDenied {
		return SystemdNoPermission
	}
	return SystemdEnumError
}

// systemdConn is the subset of *dbus.Conn we use; lets tests inject a fake.
type systemdConn interface {
	ListUnitsByPatternsContext(ctx context.Context, states, patterns []string) ([]sd.UnitStatus, error)
	GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error)
	Close()
}

type cpuTimeSample struct {
	nsec uint64
	at   time.Time
}

// SystemdCollector snapshots active/failed *.service units over the system D-Bus.
// It dials a fresh connection per cycle (see Collect), so it is safe to call every
// cycle regardless of whether systemd is present.
type SystemdCollector struct {
	newConn func(ctx context.Context) (systemdConn, error) // overridable in tests
	prevCPU map[string]cpuTimeSample
	logger  *slog.Logger // may be nil (tests); Debug/Warn logs are then no-ops

	warnedTruncated bool // WARN once per process lifetime, not once per cycle
}

func NewSystemdCollector(logger *slog.Logger) *SystemdCollector {
	return &SystemdCollector{
		newConn: defaultNewConn,
		prevCPU: map[string]cpuTimeSample{},
		logger:  logger,
	}
}

func defaultNewConn(ctx context.Context) (systemdConn, error) {
	c, err := sd.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Close is retained for the receiver's shutdown contract. Connections are dialed
// and closed within each Collect, so there is nothing to tear down here.
func (c *SystemdCollector) Close() error { return nil }

// Collect performs one detect-and-snapshot cycle. It never returns an error.
//
// A fresh D-Bus connection is dialed per cycle and closed before returning, so no
// connection lingers across the multi-minute idle interval between snapshots. This
// is deliberate: go-systemd binds a connection's lifetime to the context passed at
// dial time (godbus closes the conn when that context is Done), and the receiver
// cancels its per-call timeout context as soon as the cycle returns. A retained
// connection would therefore be torn down moments after each snapshot and fail the
// next enumeration with ErrClosed ("connection closed by user") — the source of the
// fleet-wide ~6-min flap. Dialing per cycle keeps the connection's lifetime inside
// the context that owns it.
func (c *SystemdCollector) Collect(ctx context.Context) SystemdResult {
	conn, err := c.newConn(ctx)
	if err != nil {
		return SystemdResult{Status: SystemdAbsent}
	}
	defer conn.Close()

	units, err := conn.ListUnitsByPatternsContext(ctx, activeFailedStates, []string{"*.service"})
	if err != nil {
		// classifyEnumErr tells a true permission wall (AccessDenied) apart from a
		// transient fault; Err carries the real error out for the receiver to log.
		return SystemdResult{Status: classifyEnumErr(err), Err: err}
	}

	if len(units) > maxUnits {
		// Order failed units first (stable — ties keep D-Bus order) so the cap can
		// never hide a failed unit behind a large pile of active ones.
		slices.SortStableFunc(units, func(a, b sd.UnitStatus) int {
			aFailed, bFailed := a.ActiveState == "failed", b.ActiveState == "failed"
			switch {
			case aFailed == bFailed:
				return 0
			case aFailed:
				return -1
			default:
				return 1
			}
		})
		total := len(units)
		units = units[:maxUnits]
		if !c.warnedTruncated {
			c.warnedTruncated = true
			if c.logger != nil {
				c.logger.Warn("systemd: unit list truncated", "limit", maxUnits, "total", total)
			}
		}
	}

	now := time.Now()
	numCPU := float64(runtime.NumCPU())
	newPrev := make(map[string]cpuTimeSample, len(units))
	out := make([]ServiceSnapshot, 0, len(units))
	propErrs := 0

	for _, u := range units {
		uProps, uErr := conn.GetUnitPropertiesContext(ctx, u.Name)
		sProps, sErr := conn.GetUnitTypePropertiesContext(ctx, u.Name, "Service")
		if uErr != nil {
			propErrs++
		}
		if sErr != nil {
			propErrs++
		}

		snap := ServiceSnapshot{
			Name:          u.Name,
			State:         u.ActiveState,
			SubState:      u.SubState,
			Description:   truncateRunes(asString(uProps, "Description"), maxDescriptionRunes),
			EnabledOnBoot: asString(uProps, "UnitFileState") == "enabled",
			RestartCount:  int(asUint64(sProps, "NRestarts")),
		}
		if v, ok := availUint64(sProps, "MemoryCurrent"); ok {
			n := int64(v)
			snap.MemoryBytes = &n
		}
		if v, ok := availUint64(sProps, "MemoryPeak"); ok {
			n := int64(v)
			snap.MemoryPeakBytes = &n
		}
		if ts := asUint64(uProps, "ActiveEnterTimestamp"); ts > 0 && ts != math.MaxUint64 {
			snap.ActiveSinceUnixMs = int64(ts / 1000) // µs → ms
		}
		details := ServiceDetails{
			MainPID:      int64(asUint64(sProps, "MainPID")),
			TasksCurrent: int64(asUint64(sProps, "TasksCurrent")),
			FragmentPath: asString(uProps, "FragmentPath"),
			Wants:        asStrings(uProps, "Wants"),
			Requires:     asStrings(uProps, "Requires"),
			Before:       asStrings(uProps, "Before"),
			After:        asStrings(uProps, "After"),
		}
		if v, ok := availUint64(sProps, "TasksMax"); ok {
			n := int64(v)
			details.TasksMax = &n
		}
		snap.Details = details

		// CPU%: delta of cumulative CPUUsageNSec over wall time, normalized by core count.
		if cur, ok := availUint64(sProps, "CPUUsageNSec"); ok {
			if prev, had := c.prevCPU[u.Name]; had && cur >= prev.nsec {
				elapsed := now.Sub(prev.at).Seconds()
				if elapsed > 0 {
					pct := (float64(cur-prev.nsec) / (elapsed * 1e9 * numCPU)) * 100.0
					snap.CPUPercent = &pct
				}
			}
			newPrev[u.Name] = cpuTimeSample{nsec: cur, at: now}
		}

		out = append(out, snap)
	}
	c.prevCPU = newPrev
	if propErrs > 0 && c.logger != nil {
		// One line per cycle, not one per unit: a partial harvest under a tight
		// systemd.timeout can hit every remaining unit, and per-unit logging would
		// just spam the journal without adding information.
		c.logger.Debug("systemd: per-unit property fetch failures", "count", propErrs)
	}
	return SystemdResult{Status: SystemdPresent, Services: out}
}

// availUint64 returns (value, false) when the property is missing or is the
// systemd "unavailable/unlimited" sentinel (UINT64_MAX → accounting off).
func availUint64(m map[string]any, key string) (uint64, bool) {
	v := asUint64(m, key)
	if v == math.MaxUint64 {
		return 0, false
	}
	if _, present := m[key]; !present {
		return 0, false
	}
	return v, true
}

func asUint64(m map[string]any, key string) uint64 {
	switch v := m[key].(type) {
	case uint64:
		return v
	case uint32:
		return uint64(v)
	case int64:
		if v < 0 {
			return 0
		}
		return uint64(v)
	default:
		return 0
	}
}

func asString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func asStrings(m map[string]any, key string) []string {
	if s, ok := m[key].([]string); ok {
		return s
	}
	return nil
}
