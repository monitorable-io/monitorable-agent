package collectors

import (
	"context"
	"errors"
	"io/fs"
	"strings"
)

// errSmartReadTimeout is returned by SmartCollector.Collect's internal read wrapper
// when a device's Read call is still running past the caller's ctx deadline. It is
// never surfaced outside this package — the device is simply skipped like any other
// unreadable device.
var errSmartReadTimeout = errors.New("smart: device read timed out")

// isPermissionErr reports whether err is a permission denial (EACCES/EPERM). Wrapped
// os/syscall errors satisfy this via errors.Is, since syscall.Errno implements Is
// against fs.ErrPermission for both EACCES and EPERM.
func isPermissionErr(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}

// SmartStatus is the outcome of one SMART collection attempt.
type SmartStatus int

const (
	SmartAbsent       SmartStatus = iota // no readable physical disk
	SmartNoPermission                    // device present but read denied
	SmartPresent                         // snapshot collected (possibly empty)
)

// DiskSnapshot is one physical drive's current SMART state. nil pointers mean "unavailable".
type DiskSnapshot struct {
	Device        string         `json:"device"`
	Type          string         `json:"type"` // NVMe | SSD | HDD
	Model         string         `json:"model,omitempty"`
	Serial        string         `json:"serial,omitempty"`
	CapacityBytes int64          `json:"capacity_bytes,omitempty"`
	PowerOnHours  int64          `json:"power_on_hours,omitempty"`
	SmartPassed   bool           `json:"smart_passed"`
	ErrorCount    int64          `json:"error_count"`
	WearoutPct    *int           `json:"wearout_percent,omitempty"`
	TemperatureC  *int           `json:"temperature_c,omitempty"`
	Details       map[string]any `json:"details"`
}

// rawSmart is the library-neutral view a diskReader produces for one device. The real
// adapter (Task A4) fills it from the SMART library; derivation below never touches the
// library, so it is fully unit-testable without hardware or root.
type rawSmart struct {
	Device        string
	IsNVMe        bool
	RotationRate  int // 0 = SSD, >0 = HDD (ignored when IsNVMe)
	Model         string
	Serial        string
	CapacityBytes int64
	PowerOnHours  int64
	TemperatureC  *int

	// health
	AtaOverallPassed    *bool // SATA/SAS overall self-assessment
	NvmeCriticalWarning uint8 // NVMe critical-warning byte (0 = clean)

	// errors
	AtaErrorCount   int64 // ATA error-log entry count
	NvmeMediaErrors int64 // NVMe "Media and Data Integrity Errors"

	// wear
	AtaWearRemaining      *int // SATA SSD: normalized remaining-life % (100 = new), nil if absent
	AtaDevstatPercentUsed *int // SATA SSD: ATA Device Statistics "Percentage Used Endurance Indicator" (percent USED, 0 = new), nil if absent
	NvmePercentageUsed    *int // NVMe: Percentage Used (0..100+)

	Details map[string]any // full raw attribute dump for the detail sheet
}

func diskType(r rawSmart) string {
	if r.IsNVMe {
		return "NVMe"
	}
	if r.RotationRate > 0 {
		return "HDD"
	}
	return "SSD"
}

// wearoutUsedPct returns percent-USED (0 = new). nil for HDD or when no wear metric exists.
func wearoutUsedPct(r rawSmart) *int {
	if r.IsNVMe {
		return r.NvmePercentageUsed
	}
	if r.RotationRate > 0 {
		return nil // spinning disks have no wear metric
	}
	// SATA SSD: prefer the standardized Device Statistics endurance indicator.
	if r.AtaDevstatPercentUsed != nil {
		return r.AtaDevstatPercentUsed
	}
	// Fallback: vendor-mapped remaining-life attribute (mapped vendors only).
	if r.AtaWearRemaining != nil {
		used := 100 - *r.AtaWearRemaining
		if used < 0 {
			used = 0
		}
		return &used
	}
	return nil
}

// vendorWearAttr maps a drive vendor/model substring to the SMART attribute whose
// normalized Current value (0-100) represents remaining life. Ported from Fivenines'
// VENDOR_LIFE_ATTRIBUTE_MAP. SMART attribute 231 is vendor-ambiguous (SSD-life on some
// drives, temperature on others), so a vendor-blind "first wear attribute present" scan
// mis-reads wear on those drives; matching the model resolves it. Matched case-insensitively
// (substring), first entry wins — order more-specific brands before short prefixes.
var vendorWearAttr = []struct {
	pattern string
	attrID  uint8
}{
	{"Samsung", 177},         // Wear_Leveling_Count
	{"Intel", 233},           // Media_Wearout_Indicator
	{"Crucial", 202},         // Percent_Lifetime_Remain
	{"Micron", 202},          //
	{"SanDisk", 232},         // Endurance_Remaining
	{"Western Digital", 231}, // Life_Left
	{"WDC", 231},
	{"WD", 231},
	{"Kingston", 231},
	{"Toshiba", 233},
	{"KIOXIA", 233},
	{"SK hynix", 231},
	{"HFS", 231}, // SK hynix model prefix
	{"ADATA", 231},
	{"Transcend", 177},
	{"PNY", 231},
	{"Seagate", 231},
	{"Corsair", 231},
	{"Plextor", 177},
	{"OCZ", 233},
	{"LITE-ON", 177},
	{"Team", 231},
	{"Patriot", 231},
}

// vendorWearAttrID returns the life-remaining SMART attribute ID for a drive model, or 0
// when the vendor is unrecognized.
func vendorWearAttrID(model string) uint8 {
	m := strings.ToLower(model)
	for _, v := range vendorWearAttr {
		if strings.Contains(m, strings.ToLower(v.pattern)) {
			return v.attrID
		}
	}
	return 0
}

// selectAtaWearRemaining returns a SATA SSD's normalized remaining-life % (0-100)
// from the vendor-mapped life attribute, or nil when the vendor is unmapped or its
// mapped attribute is absent. The former generic cross-vendor fallback was removed:
// attribute IDs carry different meanings per vendor (e.g. attr 233 is
// Media_Wearout_Indicator on Intel but Flash_Writes_GiB on Kingston), which produced
// false "0% used" readings on unmapped drives. The standardized Device Statistics
// endurance indicator (smart.go ReadStatistics/Get in fillSATA) is the primary
// source; this is a fallback only for drives that do not populate Device
// Statistics page 0x07.
func selectAtaWearRemaining(model string, attrCurrent map[uint8]int) *int {
	id := vendorWearAttrID(model)
	if id == 0 {
		return nil // unmapped vendor: report unknown rather than guess
	}
	if v, ok := attrCurrent[id]; ok {
		vv := v
		return &vv
	}
	return nil
}

func errorCount(r rawSmart) int64 {
	if r.IsNVMe {
		return r.NvmeMediaErrors
	}
	return r.AtaErrorCount
}

func smartPassed(r rawSmart) bool {
	if r.IsNVMe {
		return r.NvmeCriticalWarning == 0
	}
	if r.AtaOverallPassed != nil {
		return *r.AtaOverallPassed
	}
	return true // unknown → don't cry wolf
}

func deriveDiskSnapshot(r rawSmart) DiskSnapshot {
	d := r.Details
	if d == nil {
		d = map[string]any{}
	}
	return DiskSnapshot{
		Device:        r.Device,
		Type:          diskType(r),
		Model:         r.Model,
		Serial:        r.Serial,
		CapacityBytes: r.CapacityBytes,
		PowerOnHours:  r.PowerOnHours,
		SmartPassed:   smartPassed(r),
		ErrorCount:    errorCount(r),
		WearoutPct:    wearoutUsedPct(r),
		TemperatureC:  r.TemperatureC,
		Details:       d,
	}
}

// diskReader enumerates and reads physical block devices. The fake in tests and the real
// library adapter (Task A4) both satisfy it.
type diskReader interface {
	Enumerate() ([]string, error)
	Read(device string) (rawSmart, error)
}

// SmartResult is the outcome of SmartCollector.Collect.
type SmartResult struct {
	Status SmartStatus
	Disks  []DiskSnapshot
}

// SmartCollector snapshots SMART health for every readable physical disk. Devices that
// return a read error (virtio/LXC/unsupported) are skipped silently — never an error.
type SmartCollector struct {
	reader diskReader
}

func NewSmartCollector() *SmartCollector {
	return &SmartCollector{reader: newSysfsSmartReader()} // real adapter, Task A4
}

func (c *SmartCollector) Collect(ctx context.Context) SmartResult {
	devices, err := c.reader.Enumerate()
	if err != nil {
		return SmartResult{Status: SmartNoPermission}
	}
	out := make([]DiskSnapshot, 0, len(devices))
	attempted, permDenied := 0, 0
	for _, dev := range devices {
		if ctx.Err() != nil {
			break
		}
		attempted++
		raw, rerr := c.readWithDeadline(ctx, dev)
		if rerr != nil {
			if isPermissionErr(rerr) {
				permDenied++
			}
			continue // unreadable/virtio/timeout → skip silently
		}
		out = append(out, deriveDiskSnapshot(raw))
	}
	if len(out) == 0 {
		if attempted > 0 && permDenied == attempted {
			return SmartResult{Status: SmartNoPermission}
		}
		return SmartResult{Status: SmartAbsent}
	}
	return SmartResult{Status: SmartPresent, Disks: out}
}

// readWithDeadline bounds one device's Read by ctx: the ioctls it calls into
// (smart.Open/Identify/ReadSMART/ReadSMARTData) take no context and can block
// indefinitely on a wedged or failing disk — exactly the case SMART exists to catch.
// Running the read on a goroutine and selecting on ctx.Done() lets Collect move on
// (and eventually return) without waiting for the read to finish; the goroutine
// completes in the background and its result is discarded.
//
// Plainly: a read that never returns leaks one goroutine and its open fd for as long
// as the wedged ioctl stays wedged — potentially the process lifetime, one per cycle
// that times out on the same device. This is accepted, not fixed, because the
// underlying cause is bounded (a wedged disk controller, not attacker-controlled
// growth) and the anatol/smart.go library exposes no cancellable read to fix it with.
func (c *SmartCollector) readWithDeadline(ctx context.Context, dev string) (rawSmart, error) {
	type result struct {
		raw rawSmart
		err error
	}
	ch := make(chan result, 1)
	go func() {
		raw, err := c.reader.Read(dev)
		ch <- result{raw, err}
	}()
	select {
	case res := <-ch:
		return res.raw, res.err
	case <-ctx.Done():
		return rawSmart{}, errSmartReadTimeout
	}
}
