package capabilities

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SystemCapabilities tracks what the system can collect based on permissions. Fields
// that gated no collector (disk health, RAID, ZFS, network stats) and fields duplicated
// by a collector's own graceful degradation (SMART, systemd units) were removed —
// temperature detection is the only capability an actual collector consults.
type SystemCapabilities struct {
	CanReadTemperatures bool      `json:"temperatures"`
	DetectedAt          time.Time `json:"detected_at"`
}

// Detector handles capability detection.
type Detector struct {
	sysRoot string // overridable in tests; production always uses "/sys" (NewDetector)
}

// NewDetector creates a new capability detector.
func NewDetector() *Detector {
	return &Detector{sysRoot: "/sys"}
}

// DetectAll detects all system capabilities. It has no fallible calls, so it returns
// the result directly rather than a dead error.
func (d *Detector) DetectAll() *SystemCapabilities {
	return &SystemCapabilities{
		DetectedAt:          time.Now(),
		CanReadTemperatures: d.testTemperatures(),
	}
}

// testTemperatures tests access to temperature sensors, mirroring the real consumer
// (internal/sensors, via gopsutil v4): hwmon is probed first (the common case —
// coretemp/k10temp), falling back to thermal_zone only when hwmon has no entries. A
// host with working hwmon but no thermal_zone entries must still be detected.
func (d *Detector) testTemperatures() bool {
	if d.hwmonReadable() {
		return true
	}
	return d.thermalZoneReadable()
}

func (d *Detector) hwmonReadable() bool {
	hwmonDirs, err := os.ReadDir(filepath.Join(d.sysRoot, "class/hwmon"))
	if err != nil {
		return false
	}
	for _, entry := range hwmonDirs {
		matches, err := filepath.Glob(filepath.Join(d.sysRoot, "class/hwmon", entry.Name(), "temp*_input"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if data, err := os.ReadFile(m); err == nil && len(data) > 0 {
				return true
			}
		}
	}
	return false
}

func (d *Detector) thermalZoneReadable() bool {
	thermalDirs, err := os.ReadDir(filepath.Join(d.sysRoot, "class/thermal"))
	if err != nil {
		return false
	}
	for _, entry := range thermalDirs {
		// Note: thermal_zone entries are symlinks, not directories.
		if !strings.HasPrefix(entry.Name(), "thermal_zone") {
			continue
		}
		tempPath := filepath.Join(d.sysRoot, "class/thermal", entry.Name(), "temp")
		if data, err := os.ReadFile(tempPath); err == nil && len(data) > 0 {
			return true
		}
	}
	return false
}

// GetErrorFlags returns error flags for missing capabilities. Only capabilities an
// actual collector consumes get a flag — the disk_health/raid_status/zfs_status/
// network_stats/smart_data/systemd_units flags all advertised unbuilt or
// self-degrading features and were removed.
//
// temperature_monitoring_requires_root was removed too (2026-10-13): it fired whenever
// no hwmon/thermal_zone sensor was READABLE, which on every VM means no sensor EXISTS.
// Root never helped (sysfs temp*_input is world-readable where it exists) and the unit
// never runs as root. So today no capability maps to a code, and the backend/dashboard
// catalog that gates what the Permission limitations card shows is empty to match. A
// code goes back in here only for a real EACCES case, together with the backend
// (pkg/models/permission_errors.go) and dashboard catalog entries in the same release.
func (d *Detector) GetErrorFlags(_ *SystemCapabilities) []string {
	return nil
}
