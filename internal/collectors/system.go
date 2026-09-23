package collectors

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// SystemInfoCollector collects basic system identification information
type SystemInfoCollector struct {
	logger *slog.Logger // may be nil (tests); swallowed-failure Debug logs are then no-ops

	// overridable in tests; production always uses the real paths (NewSystemInfoCollector)
	osReleasePath   string
	procVersionPath string
}

// SystemInfo holds system identification data
type SystemInfo struct {
	Hostname       string
	OSName         string
	OSVersion      string
	KernelVersion  string
	Architecture   string
	Uptime         int64
	RebootRequired bool
}

// NewSystemInfoCollector creates a new system info collector
func NewSystemInfoCollector(logger *slog.Logger) *SystemInfoCollector {
	return &SystemInfoCollector{logger: logger, osReleasePath: "/etc/os-release", procVersionPath: "/proc/version"}
}

// Collect gathers system information
func (c *SystemInfoCollector) Collect() *SystemInfo {
	info := &SystemInfo{}

	// Get hostname
	if hostname, err := os.Hostname(); err == nil {
		info.Hostname = hostname
	}

	// Get OS information
	info.OSName = runtime.GOOS
	info.Architecture = runtime.GOARCH

	// Get detailed OS version from /etc/os-release
	info.OSVersion = c.getOSVersion()

	// Get kernel version from /proc/version
	info.KernelVersion = c.getKernelVersion()

	// Get system uptime
	info.Uptime = c.getUptime()

	// Check if reboot is required
	info.RebootRequired = c.isRebootRequired()

	return info
}

func (c *SystemInfoCollector) debug(msg string, args ...any) {
	if c.logger != nil {
		c.logger.Debug(msg, args...)
	}
}

// getOSVersion reads OS version from /etc/os-release
func (c *SystemInfoCollector) getOSVersion() string {
	data, err := os.ReadFile(c.osReleasePath)
	if err != nil {
		c.debug("falling back to unknown OS version", "err", err)
		return "unknown"
	}
	version := parseOSRelease(data)
	if version == "unknown" {
		c.debug("falling back to unknown OS version: no PRETTY_NAME line in " + c.osReleasePath)
	}
	return version
}

// parseOSRelease extracts PRETTY_NAME from the contents of /etc/os-release. Pure
// (no I/O), so it is directly unit-testable independent of the real file.
func parseOSRelease(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			// Remove PRETTY_NAME= and quotes
			version := strings.TrimPrefix(line, "PRETTY_NAME=")
			version = strings.Trim(version, "\"")
			return version
		}
	}
	// A scan error (a line over bufio's 64 KiB token limit) ends the loop early;
	// the fallback is the same either way, but don't pretend the file was read.
	if err := scanner.Err(); err != nil {
		return "unknown"
	}

	return "unknown"
}

// getKernelVersion reads kernel version from /proc/version
func (c *SystemInfoCollector) getKernelVersion() string {
	data, err := os.ReadFile(c.procVersionPath)
	if err != nil {
		c.debug("falling back to unknown kernel version", "err", err)
		return "unknown"
	}

	// Parse kernel version from the first part of /proc/version
	parts := strings.Fields(string(data))
	if len(parts) >= 3 {
		return parts[2] // Usually the third field is the kernel version
	}

	c.debug("falling back to unknown kernel version: fewer than 3 whitespace-separated fields in " + c.procVersionPath)
	return "unknown"
}

// getUptime reads system uptime from /proc/uptime
func (c *SystemInfoCollector) getUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}

	// /proc/uptime contains two numbers: uptime in seconds and idle time
	parts := strings.Fields(string(data))
	if len(parts) >= 1 {
		if uptime, err := strconv.ParseFloat(parts[0], 64); err == nil {
			return int64(uptime)
		}
	}

	return 0
}

// isRebootRequired checks if a reboot is required
func (c *SystemInfoCollector) isRebootRequired() bool {
	// Check for Ubuntu/Debian reboot-required file
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		return true
	}

	return false
}

// HardwareSpecCollector collects hardware specifications
type HardwareSpecCollector struct{}

// HardwareSpec holds hardware specification data
type HardwareSpec struct {
	CPUModel            string
	CPUCores            int
	CPUThreadsPerCore   int
	CPUThreadsTotal     int
	CPUFrequencyCurrent int64
	TotalRAM            int64
}

// NewHardwareSpecCollector creates a new hardware spec collector
func NewHardwareSpecCollector() *HardwareSpecCollector {
	return &HardwareSpecCollector{}
}

// Collect gathers hardware specifications
func (c *HardwareSpecCollector) Collect() *HardwareSpec {
	spec := &HardwareSpec{}

	// Get CPU information from /proc/cpuinfo
	c.parseCPUInfo(spec)

	// Get memory information from /proc/meminfo
	spec.TotalRAM = c.getTotalRAM()

	// Get current CPU frequency (if available)
	spec.CPUFrequencyCurrent = c.getCurrentCPUFreq()

	return spec
}

// parseCPUInfo parses CPU information from /proc/cpuinfo
func (c *HardwareSpecCollector) parseCPUInfo(spec *HardwareSpec) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	processorCount := 0

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "processor") {
			processorCount++
		} else if strings.HasPrefix(line, "model name") && spec.CPUModel == "" {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				spec.CPUModel = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "cpu cores") && spec.CPUCores == 0 {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				if cores, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
					spec.CPUCores = cores
				}
			}
		} else if strings.HasPrefix(line, "siblings") && spec.CPUThreadsPerCore == 0 {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				if siblings, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
					if spec.CPUCores > 0 {
						spec.CPUThreadsPerCore = siblings / spec.CPUCores
					}
				}
			}
		}
	}

	// A scan error (an over-long line) ends the loop early; whatever was parsed
	// before it stands, and the fallbacks below fill the rest as for a short file.
	if err := scanner.Err(); err != nil && processorCount == 0 {
		processorCount = runtime.NumCPU()
	}
	spec.CPUThreadsTotal = processorCount

	// If we couldn't determine cores from cpuinfo, estimate from logical processors
	if spec.CPUCores == 0 {
		spec.CPUCores = runtime.NumCPU()
	}

	// If we couldn't determine threads per core, assume 1
	if spec.CPUThreadsPerCore == 0 {
		spec.CPUThreadsPerCore = 1
	}
}

// getTotalRAM gets total system RAM from /proc/meminfo
func (c *HardwareSpecCollector) getTotalRAM() int64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if kb, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					return kb * 1024 // Convert KB to bytes
				}
			}
			break
		}
	}
	// Reached only without a MemTotal line: either the file lacks one or the scan
	// stopped on an error; both mean "unknown", which 0 already encodes.
	_ = scanner.Err()

	return 0
}

// getCurrentCPUFreq gets current CPU frequency (average across all cores)
func (c *HardwareSpecCollector) getCurrentCPUFreq() int64 {
	// Try to read from /proc/cpuinfo first
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return c.getCPUFreqFromSys()
	}
	defer file.Close()

	var frequencies []int64
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu MHz") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				if freqMHz, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
					frequencies = append(frequencies, int64(freqMHz))
				}
			}
		}
	}

	// A scan error before any "cpu MHz" line means /proc/cpuinfo could not be
	// read through; fall back to sysfs exactly as when the file failed to open.
	if err := scanner.Err(); err != nil && len(frequencies) == 0 {
		return c.getCPUFreqFromSys()
	}

	// Calculate average frequency
	if len(frequencies) > 0 {
		var sum int64
		for _, freq := range frequencies {
			sum += freq
		}
		return sum / int64(len(frequencies))
	}

	return c.getCPUFreqFromSys()
}

// getCPUFreqFromSys gets CPU frequency from /sys filesystem
func (c *HardwareSpecCollector) getCPUFreqFromSys() int64 {
	// Try to read from /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq")
	if err != nil {
		return 0
	}

	// scaling_cur_freq is in kHz
	if freqKHz, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
		return freqKHz / 1000 // Convert kHz to MHz
	}

	return 0
}
