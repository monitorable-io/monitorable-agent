package collectors

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	smart "github.com/anatol/smart.go"
)

type sysfsSmartReader struct{}

func newSysfsSmartReader() diskReader { return &sysfsSmartReader{} }

var nvmeNsRe = regexp.MustCompile(`^(nvme\d+)n\d+`)

// nvmeControllerNode maps an NVMe namespace name to its controller node,
// e.g. "nvme0n1" -> "nvme0". NVMe admin commands (Identify Controller, SMART
// log) target the controller, not the namespace. Names that don't match
// (including all non-NVMe devices) are returned unchanged.
func nvmeControllerNode(device string) string {
	if m := nvmeNsRe.FindStringSubmatch(device); m != nil {
		return m[1]
	}
	return device
}

// Enumerate returns candidate physical block devices from /sys/block, excluding
// virtual and partition-less pseudo devices.
func (s *sysfsSmartReader) Enumerate() ([]string, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if isPhysicalBlockName(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// isPhysicalBlockName keeps sd*, nvme*, vd* and drops loop/dm-/ram/md/sr/zram/zd/fd/dax/nbd.
func isPhysicalBlockName(name string) bool {
	for _, p := range []string{"loop", "dm-", "ram", "md", "sr", "zram", "zd", "fd", "dax", "nbd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return strings.HasPrefix(name, "sd") ||
		strings.HasPrefix(name, "nvme") ||
		strings.HasPrefix(name, "vd")
}

// Read opens /dev/<device> and fills rawSmart from the library. Returns an error
// for any device that exposes no SMART (virtio/LXC) so the collector skips it
// silently.
func (s *sysfsSmartReader) Read(device string) (rawSmart, error) {
	path := filepath.Join("/dev", nvmeControllerNode(device))
	isNVMe := strings.HasPrefix(device, "nvme")

	r := rawSmart{
		Device:  device,
		IsNVMe:  isNVMe,
		Details: map[string]any{},
	}

	dev, err := smart.Open(path)
	if err != nil {
		return rawSmart{}, fmt.Errorf("smart.Open %s: %w", path, err)
	}
	defer dev.Close()

	switch d := dev.(type) {
	case *smart.NVMeDevice:
		if err := fillNVMe(&r, d); err != nil {
			return rawSmart{}, err
		}
	case *smart.SataDevice:
		if err := fillSATA(&r, d); err != nil {
			return rawSmart{}, err
		}
	default:
		// GenericAttributes only — no SMART attribute detail
		attrs, err := dev.ReadGenericAttributes()
		if err != nil {
			return rawSmart{}, fmt.Errorf("ReadGenericAttributes %s: %w", device, err)
		}
		r.PowerOnHours = int64(attrs.PowerOnHours)
		if attrs.Temperature > 0 {
			t := int(attrs.Temperature)
			r.TemperatureC = &t
		}
	}

	return r, nil
}

// fillNVMe populates rawSmart from an NVMe device.
//
// Mapped fields:
//   - NvmeCriticalWarning  ← NvmeSMARTLog.CritWarning
//   - NvmePercentageUsed   ← NvmeSMARTLog.PercentUsed
//   - NvmeMediaErrors      ← NvmeSMARTLog.MediaErrors (lower 64 bits of Uint128)
//   - PowerOnHours         ← NvmeSMARTLog.PowerOnHours (lower 64 bits of Uint128)
//   - TemperatureC         ← NvmeSMARTLog.Temperature (Kelvin − 273)
//   - Model / Serial       ← NvmeIdentController.ModelNumber() / SerialNumber()
//   - CapacityBytes        ← NvmeIdentController.Tnvmcap (lower 64 bits)
//
// NOTE: CapacityBytes uses Tnvmcap (Total NVM Capacity). It may be 0 on
// some controllers that do not populate this field; in that case the field
// is left zero.
func fillNVMe(r *rawSmart, d *smart.NVMeDevice) error {
	r.IsNVMe = true

	// Identity — model, serial, capacity
	ctrl, _, err := d.Identify()
	if err != nil {
		return fmt.Errorf("NVMe Identify %s: %w", r.Device, err)
	}
	r.Model = ctrl.ModelNumber()
	r.Serial = ctrl.SerialNumber()
	r.CapacityBytes = int64(ctrl.Tnvmcap.Val[0]) // lower 64 bits; >64 bit drives are future

	// SMART log
	log, err := d.ReadSMART()
	if err != nil {
		return fmt.Errorf("NVMe ReadSMART %s: %w", r.Device, err)
	}

	r.NvmeCriticalWarning = log.CritWarning
	pct := int(log.PercentUsed)
	r.NvmePercentageUsed = &pct
	r.NvmeMediaErrors = int64(log.MediaErrors.Val[0]) // lower 64 bits
	r.PowerOnHours = int64(log.PowerOnHours.Val[0])   // lower 64 bits

	// Temperature: Kelvin to Celsius (0 K means sensor not populated)
	if log.Temperature > 0 {
		t := int(log.Temperature) - 273
		r.TemperatureC = &t
	}

	// Details — best-effort dump of interesting NVMe counters
	r.Details["nvme_avail_spare"] = log.AvailSpare
	r.Details["nvme_spare_thresh"] = log.SpareThresh
	r.Details["nvme_data_units_read"] = log.DataUnitsRead.Val[0]
	r.Details["nvme_data_units_written"] = log.DataUnitsWritten.Val[0]
	r.Details["nvme_host_reads"] = log.HostReads.Val[0]
	r.Details["nvme_host_writes"] = log.HostWrites.Val[0]
	r.Details["nvme_power_cycles"] = log.PowerCycles.Val[0]
	r.Details["nvme_unsafe_shutdowns"] = log.UnsafeShutdowns.Val[0]
	r.Details["nvme_warning_temp_time"] = log.WarningTempTime
	r.Details["nvme_crit_comp_time"] = log.CritCompTime

	return nil
}

// fillSATA populates rawSmart from a SATA device.
//
// Mapped fields:
//   - RotationRate      ← AtaIdentifyDevice.RotationRate (0x0001 = SSD)
//   - Model / Serial    ← AtaIdentifyDevice.ModelNumber() / SerialNumber()
//   - CapacityBytes     ← AtaIdentifyDevice.Capacity() (second return value)
//   - AtaOverallPassed  ← derived: all prefailure attrs have Current > threshold
//   - PowerOnHours      ← attr 9 (Power_On_Hours) ValueRaw
//   - TemperatureC      ← attr 194 (Temperature_Celsius) or 190 (Airflow_Temp)
//   - AtaDevstatPercentUsed ← ATA Device Statistics page 0x07 "Percentage Used Endurance Indicator" (primary SATA wear source)
//   - AtaWearRemaining  ← vendor-aware life attribute Current (selectAtaWearRemaining),
//     mapped vendors only (Intel→233, Kingston→231, etc.); nil for unmapped vendors
//   - AtaErrorCount     ← AtaSmartErrorLogSummary.ErrorCount
//
// NOTE: AtaOverallPassed is derived from attribute Current vs thresholds
// (pre-failure attributes only) since the library does not expose the
// ATA SMART RETURN STATUS command result via a public method. This is
// semantically equivalent for the common case; truly failing drives will
// always have at least one pre-failure attribute below threshold.
func fillSATA(r *rawSmart, d *smart.SataDevice) error {
	// Identity
	id, err := d.Identify()
	if err != nil {
		return fmt.Errorf("ATA Identify %s: %w", r.Device, err)
	}
	r.Model = id.ModelNumber()
	r.Serial = id.SerialNumber()

	// Rotation rate: 0x0001 = non-rotating (SSD); 0 = not reported.
	// Deliberately treating "not reported" (0) as SSD (zero value) since rotating
	// disks that report an RPM always populate this field correctly; an unreported
	// rate is characteristic of solid-state media. diskType() classifies RotationRate==0
	// as "ssd", which is the safe default.
	if id.RotationRate == 0x0001 {
		r.RotationRate = 0 // SSD (explicit non-rotating flag)
	} else {
		r.RotationRate = int(id.RotationRate) // HDD rpm, or 0 (not reported → treated as SSD)
	}

	_, capacity, _, _, _ := id.Capacity()
	r.CapacityBytes = int64(capacity)

	// SMART data
	page, err := d.ReadSMARTData()
	if err != nil {
		return fmt.Errorf("ReadSMARTData %s: %w", r.Device, err)
	}

	// Thresholds (for overall-pass derivation). When this read fails, leave
	// AtaOverallPassed nil rather than asserting a pass we never actually verified —
	// smartPassed() already treats nil as "don't cry wolf", but reporting a hardcoded
	// true here would be indistinguishable from a drive genuinely checked healthy.
	thresh, threshErr := d.ReadSMARTThresholds()
	if threshErr == nil && thresh != nil {
		// Derive overall health: fail if any prefailure attribute is below threshold.
		passed := true
		for id, attr := range page.Attrs {
			if attr.Flags&smart.AtaAttributeFlagPrefailure == 0 {
				continue // old-age / informational — skip
			}
			if t, ok := thresh.Thresholds[id]; ok && t > 0 && attr.Current <= t {
				passed = false
				break
			}
		}
		r.AtaOverallPassed = &passed
	}

	// Walk attributes; index normalized Current by attr ID for the vendor-aware wear
	// selection done after the loop (a single pass needs the whole table first).
	attrCurrent := make(map[uint8]int)
	for _, attr := range page.Attrs {
		attrCurrent[attr.Id] = int(attr.Current)
		switch attr.Id {
		case 9: // Power_On_Hours
			r.PowerOnHours = int64(attr.ValueRaw)
		case 194, 190: // Temperature_Celsius, Airflow_Temperature_Cel
			if r.TemperatureC == nil {
				t, _, _, _, tErr := attr.ParseAsTemperature()
				if tErr == nil {
					tc := t
					r.TemperatureC = &tc
				}
			}
		}

		// Populate Details with every attribute (full dump)
		r.Details[fmt.Sprintf("attr_%d_%s", attr.Id, attr.Name)] = map[string]any{
			"id":      attr.Id,
			"name":    attr.Name,
			"current": attr.Current,
			"worst":   attr.Worst,
			"raw":     attr.ValueRaw,
			"flags":   attr.Flags,
		}

		// Also write flat semantic keys used by the app's detail-sheet rows
		switch attr.Id {
		case 5:
			r.Details["reallocated_sectors"] = attr.ValueRaw
		case 197:
			r.Details["pending_sectors"] = attr.ValueRaw
		case 198:
			r.Details["offline_uncorrectable"] = attr.ValueRaw
		case 199:
			r.Details["udma_crc_errors"] = attr.ValueRaw
		}
	}

	// Wear (fallback): vendor-aware remaining-life attribute for mapped vendors only
	// (resolves the attr-231 ambiguity; nil for unmapped vendors). The primary wear
	// source is AtaDevstatPercentUsed, read via Device Statistics just below.
	r.AtaWearRemaining = selectAtaWearRemaining(r.Model, attrCurrent)

	// Device Statistics (GP Log 0x04): the standardized "Percentage Used
	// Endurance Indicator" (SSD page 0x07, offset 0x008) is the primary SATA
	// wear source. Best-effort — absence/error degrades to the vendor-attr
	// fallback above, so do not surface an error. Get honors the statistic's
	// supported/valid flags; the percentage lives in the low byte.
	if stats, derr := d.ReadStatistics(); derr == nil {
		if v, ok := stats.Get(smart.AtaStatPercentageUsedEndurance); ok {
			pu := int(uint8(v))
			r.AtaDevstatPercentUsed = &pu
			r.Details["devstat_percentage_used_endurance"] = pu
		}
	}

	// ATA error log entry count — best-effort
	if errLog, err := d.ReadSMARTErrorLogSummary(); err == nil {
		r.AtaErrorCount = int64(errLog.ErrorCount)
		r.Details["ata_error_log_count"] = errLog.ErrorCount
	}

	return nil
}
