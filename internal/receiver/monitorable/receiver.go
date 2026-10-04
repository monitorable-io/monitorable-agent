package monitorable

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monitorable-io/monitorable-agent/internal/addressprobe"
	"github.com/monitorable-io/monitorable-agent/internal/capabilities"
	"github.com/monitorable-io/monitorable-agent/internal/collectors"
	"github.com/monitorable-io/monitorable-agent/internal/sensors"
	"github.com/monitorable-io/monitorable-agent/internal/version"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// metricsReceiver implements the receiver.Metrics interface
type metricsReceiver struct {
	cfg      *Config
	consumer consumer.Metrics
	logger   *zap.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Collection components. capabilities is written by the redetectCapabilities
	// goroutine (performCapabilityRedetection) and read every cycle by
	// collectMetrics's goroutine (performUnifiedCollection) — both sides go
	// through metadataMutex, the same lock that guards the cached* fields below.
	capabilities  *capabilities.SystemCapabilities
	capDetector   *capabilities.Detector
	collectors    *collectorSet
	tempCollector *sensors.TemperatureCollector

	// Docker container snapshot (nil when docker.mode=off)
	dockerCollector  *collectors.DockerCollector
	dockerMode       string
	dockerPrev       collectors.DockerStatus
	dockerJSONWarned bool // WARN once per process lifetime when docker.containers exceeds maxSnapshotBytes

	// Systemd service snapshot (nil when systemd.mode=off). Sampled on a slow cadence.
	systemdCollector  *collectors.SystemdCollector
	systemdMode       string
	systemdPrev       collectors.SystemdStatus
	systemdInterval   time.Duration
	lastSystemdAt     time.Time
	systemdJSONWarned bool // WARN once per process lifetime when systemd.services exceeds maxSnapshotBytes

	// SMART disk-health snapshot (nil when smart.mode=off). Sampled on a slow cadence.
	smartCollector  *collectors.SmartCollector
	smartInterval   time.Duration
	lastSmartAt     time.Time
	smartMode       string
	smartPrev       collectors.SmartStatus
	smartJSONWarned bool // WARN once per process lifetime when smart.disks exceeds maxSnapshotBytes

	// Cached system metadata (updated periodically, used in all OTLP payloads)
	cachedSystemInfo   *collectors.SystemInfo
	cachedHardwareSpec *collectors.HardwareSpec
	cachedCloudInfo    *collectors.CloudInfo
	metadataMutex      sync.RWMutex
}

// collectorSet holds all the metric collectors
type collectorSet struct {
	systemInfo *collectors.SystemInfoCollector
	hwSpec     *collectors.HardwareSpecCollector
	cloudInfo  *collectors.CloudInfoCollector
	cpuTime    *collectors.CPUTimeCollector
}

// newMetricsReceiver creates a new metrics receiver. It runs uncancellable I/O
// (capability detection, collector construction, the first metadata refresh)
// during construction; none of it currently accepts a context, so there is no ctx
// param to bound it with.
func newMetricsReceiver(
	set receiver.Settings,
	cfg *Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	r := &metricsReceiver{
		cfg:         cfg,
		consumer:    consumer,
		logger:      set.Logger,
		capDetector: capabilities.NewDetector(),
	}

	// Initialize capability detection
	r.capabilities = r.capDetector.DetectAll()

	// Match the slog-based collectors' log level to the collector's configured zap
	// level so per-item DEBUG logs are suppressed unless the operator opts in.
	slogLevel := slog.LevelInfo
	if r.logger.Core().Enabled(zapcore.DebugLevel) {
		slogLevel = slog.LevelDebug
	}
	slogLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))

	// Initialize collectors
	r.collectors = r.initializeCollectors(slogLogger)

	// Docker container snapshot: self-detecting collector (nil when mode=off).
	r.dockerMode = cfg.Docker.Mode
	if r.dockerMode == "" {
		r.dockerMode = DockerModeAuto
	}
	r.dockerPrev = collectors.DockerStatus(-1) // unset → first status always logs
	if r.dockerMode != DockerModeOff {
		r.dockerCollector = collectors.NewDockerCollector(cfg.Docker.Endpoint, cfg.Docker.ExcludedImages, slogLogger)
	}

	// Systemd service snapshot: self-detecting collector (nil when mode=off).
	r.systemdMode = cfg.Systemd.Mode
	if r.systemdMode == "" {
		r.systemdMode = SystemdModeAuto
	}
	r.systemdPrev = collectors.SystemdStatus(-1) // unset → first status always logs
	r.systemdInterval = cfg.Systemd.Interval
	if r.systemdMode != SystemdModeOff {
		r.systemdCollector = collectors.NewSystemdCollector(slogLogger)
	}

	// SMART disk-health snapshot: self-detecting collector (nil when mode=off).
	r.smartMode = cfg.Smart.Mode
	if r.smartMode == "" {
		r.smartMode = SmartModeAuto
	}
	r.smartPrev = collectors.SmartStatus(-1) // unset → first status always logs
	r.smartInterval = cfg.Smart.Interval
	if r.smartMode != SmartModeOff {
		r.smartCollector = collectors.NewSmartCollector()
	}

	// Initialize temperature collector with robust sensor support
	r.tempCollector = sensors.NewTemperatureCollector(
		cfg.PrimarySensor,
		cfg.SysSensorsPath,
		cfg.SensorFilter,
		slogLogger,
	)

	// Initialize cached system metadata
	r.refreshCachedMetadata()

	return r, nil
}

// Start begins the metrics collection
func (r *metricsReceiver) Start(ctx context.Context, host component.Host) error {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("Starting Monitorable receiver",
		zap.Duration("collection_interval", r.cfg.CollectionInterval),
		zap.Any("capabilities", r.capabilities),
	)

	// Start metrics collection goroutine (collects both metrics and metadata)
	r.wg.Add(1)
	go r.collectMetrics(ctx)

	// Start capability re-detection goroutine
	r.wg.Add(1)
	go r.redetectCapabilities(ctx)

	if ap := r.cfg.AddressProbe; ap.Endpoint != "" {
		prober := addressprobe.New(addressprobe.Config{
			Endpoint: ap.Endpoint,
			APIKey:   string(ap.APIKey),
			Interval: ap.Interval,
		}, r.logger.Named("address_probe"))
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			prober.Run(ctx)
		}()
	}

	return nil
}

// Shutdown stops the receiver
func (r *metricsReceiver) Shutdown(ctx context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	if r.dockerCollector != nil {
		_ = r.dockerCollector.Close()
	}
	if r.systemdCollector != nil {
		_ = r.systemdCollector.Close()
	}
	r.logger.Info("Monitorable receiver stopped")
	return nil
}

// collectMetrics runs the unified metrics and metadata collection
func (r *metricsReceiver) collectMetrics(ctx context.Context) {
	defer r.wg.Done()

	// Use the batch processor's timeout from config if available
	// Default to 60 seconds if not specified
	interval := 60 * time.Second
	if r.cfg.CollectionInterval > 0 {
		interval = r.cfg.CollectionInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Collect immediately on start
	r.performUnifiedCollection(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.performUnifiedCollection(ctx)
		}
	}
}

// redetectCapabilities runs capability re-detection (24h)
func (r *metricsReceiver) redetectCapabilities(ctx context.Context) {
	defer r.wg.Done()

	ticker := time.NewTicker(r.cfg.CapabilityInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.performCapabilityRedetection()
		}
	}
}

// performUnifiedCollection collects and sends both metrics and metadata
func (r *metricsReceiver) performUnifiedCollection(ctx context.Context) {
	// Refresh cached metadata with latest system information
	r.refreshCachedMetadata()

	// capabilities is written by the redetectCapabilities goroutine
	// (performCapabilityRedetection); read it once here, under the same
	// metadataMutex, into a local so the rest of this cycle sees a consistent
	// snapshot instead of racing that goroutine on every access.
	r.metadataMutex.RLock()
	caps := r.capabilities
	r.metadataMutex.RUnlock()

	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()

	// Add cached resource attributes (includes all metadata)
	resourceAttrs := resourceMetrics.Resource().Attributes()
	r.addCachedResourceAttributes(resourceAttrs)
	r.collectSystemdServices(ctx, resourceAttrs)
	r.collectSmartDisks(ctx, resourceAttrs)
	r.collectDockerContainers(ctx, resourceAttrs)

	scopeMetrics := resourceMetrics.ScopeMetrics().AppendEmpty()

	// Collect all metrics
	r.collectCPUMetrics(scopeMetrics.Metrics())

	// Collect metadata metrics
	r.collectSystemInfoMetrics(scopeMetrics.Metrics())
	r.collectHardwareHealthMetrics(ctx, scopeMetrics.Metrics(), caps)

	// Collect dynamic hardware metrics. CPU temperature is already collected by
	// collectHardwareHealthMetrics above (same CanReadTemperatures guard); calling it
	// again here emitted system.cpu.temperature{,.dashboard} twice per cycle.
	r.collectCPUFrequency(scopeMetrics.Metrics())

	// Send all metrics to consumer
	if err := r.consumer.ConsumeMetrics(ctx, metrics); err != nil {
		r.logger.Error("Failed to send metrics", zap.Error(err))
	}
}

// performCapabilityRedetection re-detects system capabilities. The read-then-write
// against r.capabilities is done entirely under metadataMutex so it cannot race
// performUnifiedCollection's read above.
func (r *metricsReceiver) performCapabilityRedetection() {
	newCapabilities := r.capDetector.DetectAll()

	// Always store the fresh result (DetectedAt is not read anywhere, so refreshing
	// it every cycle is harmless); only the log line is gated on an actual change.
	r.metadataMutex.Lock()
	old := r.capabilities
	changed := old == nil || old.CanReadTemperatures != newCapabilities.CanReadTemperatures
	r.capabilities = newCapabilities
	r.metadataMutex.Unlock()

	if changed {
		r.logger.Info("System capabilities changed",
			zap.Any("old", old),
			zap.Any("new", newCapabilities),
		)
	}
}

// Metric collection methods - basic implementations
func (r *metricsReceiver) collectCPUMetrics(metrics pmetric.MetricSlice) {
	times, err := r.collectors.cpuTime.Collect()
	if err != nil {
		// A transient /proc/stat read failure skips only this cycle's CPU metric and
		// recovers next interval; it never affects the other metrics in this batch.
		r.logger.Debug("Failed to collect CPU time", zap.Error(err))
		return
	}
	buildCPUTimeMetric(metrics, times, time.Now())
}

func (r *metricsReceiver) collectSystemInfoMetrics(metrics pmetric.MetricSlice) {
	// Collect system uptime as a gauge metric
	systemInfo := r.collectors.systemInfo.Collect()
	uptimeMetric := metrics.AppendEmpty()
	uptimeMetric.SetName("system.uptime")
	uptimeMetric.SetDescription("Time since system boot")
	uptimeMetric.SetUnit("s")

	gauge := uptimeMetric.SetEmptyGauge()
	dataPoint := gauge.DataPoints().AppendEmpty()
	dataPoint.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
	dataPoint.SetIntValue(systemInfo.Uptime)
}

func (r *metricsReceiver) collectHardwareHealthMetrics(ctx context.Context, metrics pmetric.MetricSlice, caps *capabilities.SystemCapabilities) {
	// Only collect temperature if capability is detected
	if caps != nil && caps.CanReadTemperatures {
		r.collectCPUTemperature(ctx, metrics)
	}
}

// temperatureReadTimeout bounds the sysfs temperature read (hwmon/thermal_zone) so a
// stuck read cannot wedge the single collection goroutine indefinitely. A constant,
// not a config knob — nothing else in this receiver has a temperature section.
const temperatureReadTimeout = 10 * time.Second

func (r *metricsReceiver) collectCPUTemperature(ctx context.Context, metrics pmetric.MetricSlice) {
	cctx, cancel := context.WithTimeout(ctx, temperatureReadTimeout)
	defer cancel()

	// Use the robust temperature collector
	temperatures, dashboardTemp, err := r.tempCollector.CollectTemperatures(cctx)
	if err != nil {
		r.logger.Debug("Failed to collect temperatures", zap.Error(err))
		return
	}

	if len(temperatures) == 0 {
		r.logger.Debug("No temperature sensors available")
		return
	}

	// Create metrics for each sensor
	for sensorName, temp := range temperatures {
		metric := metrics.AppendEmpty()
		metric.SetName("system.cpu.temperature")
		metric.SetDescription("CPU temperature in Celsius")
		metric.SetUnit("Cel")

		gauge := metric.SetEmptyGauge()
		dataPoint := gauge.DataPoints().AppendEmpty()
		dataPoint.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
		dataPoint.SetDoubleValue(temp)

		// Add sensor name as attribute
		dataPoint.Attributes().PutStr("sensor", sensorName)

		r.logger.Debug("Collected CPU temperature",
			zap.String("sensor", sensorName),
			zap.Float64("temp_celsius", temp))
	}

	// Also create a dashboard temperature metric if available
	if dashboardTemp > 0 {
		metric := metrics.AppendEmpty()
		metric.SetName("system.cpu.temperature.dashboard")
		metric.SetDescription("Primary CPU temperature for dashboard display")
		metric.SetUnit("Cel")

		gauge := metric.SetEmptyGauge()
		dataPoint := gauge.DataPoints().AppendEmpty()
		dataPoint.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
		dataPoint.SetDoubleValue(dashboardTemp)

		r.logger.Debug("Dashboard temperature", zap.Float64("temp_celsius", dashboardTemp))
	}
}

func (r *metricsReceiver) collectCPUFrequency(metrics pmetric.MetricSlice) {
	// Read current CPU frequency from /proc/cpuinfo or /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq
	freqPath := "/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq"
	data, err := os.ReadFile(freqPath)
	if err != nil {
		// Fallback: try to get from collectors if available. cachedHardwareSpec is
		// written by refreshCachedMetadata under metadataMutex, so it must be read
		// under the same lock — into a local, so the nil check and the field read
		// below see a consistent snapshot instead of racing a concurrent write.
		r.metadataMutex.RLock()
		hwSpec := r.cachedHardwareSpec
		r.metadataMutex.RUnlock()
		if hwSpec != nil && hwSpec.CPUFrequencyCurrent > 0 {
			freqMHz := float64(hwSpec.CPUFrequencyCurrent)
			r.createFrequencyMetric(metrics, freqMHz)
		}
		return
	}

	freqStr := strings.TrimSpace(string(data))
	freqKHz, err := strconv.ParseInt(freqStr, 10, 64)
	if err != nil {
		return
	}

	// Convert from kHz to MHz
	freqMHz := float64(freqKHz) / 1000.0

	r.createFrequencyMetric(metrics, freqMHz)
	r.logger.Debug("Collected CPU frequency", zap.Float64("freq_mhz", freqMHz))
}

func (r *metricsReceiver) createFrequencyMetric(metrics pmetric.MetricSlice, freqMHz float64) {
	// Create CPU frequency metric
	metric := metrics.AppendEmpty()
	metric.SetName("system.cpu.frequency.current")
	metric.SetDescription("Current CPU frequency in MHz")
	metric.SetUnit("MHz")

	gauge := metric.SetEmptyGauge()
	dataPoint := gauge.DataPoints().AppendEmpty()
	dataPoint.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
	dataPoint.SetDoubleValue(freqMHz)
}

func (r *metricsReceiver) initializeCollectors(logger *slog.Logger) *collectorSet {
	return &collectorSet{
		systemInfo: collectors.NewSystemInfoCollector(logger),
		hwSpec:     collectors.NewHardwareSpecCollector(),
		cloudInfo:  collectors.NewCloudInfoCollector(""), // "" → default sysRoot "/sys"
		cpuTime:    collectors.NewCPUTimeCollector(),
	}
}

// refreshCachedMetadata collects and caches system metadata. None of the three
// collectors it calls can fail (they degrade internally to zero-value/"unknown"
// fields and log their own swallowed failures where that matters), so there is
// nothing for this method itself to report as an error.
func (r *metricsReceiver) refreshCachedMetadata() {
	r.logger.Debug("Refreshing cached system metadata")

	systemInfo := r.collectors.systemInfo.Collect()
	hardwareSpec := r.collectors.hwSpec.Collect()
	cloudInfo := r.collectors.cloudInfo.Collect()

	// Update cached metadata with lock
	r.metadataMutex.Lock()
	r.cachedSystemInfo = systemInfo
	r.cachedHardwareSpec = hardwareSpec
	r.cachedCloudInfo = cloudInfo
	r.metadataMutex.Unlock()

	r.logger.Debug("Cached system metadata refreshed successfully",
		zap.String("hostname", systemInfo.Hostname),
		zap.String("os", systemInfo.OSName),
		zap.String("cpu_model", hardwareSpec.CPUModel),
		zap.Int("cpu_cores", hardwareSpec.CPUCores))
}

// addCachedResourceAttributes adds cached system metadata to resource attributes
func (r *metricsReceiver) addCachedResourceAttributes(attrs pcommon.Map) {
	r.metadataMutex.RLock()
	defer r.metadataMutex.RUnlock()

	// Add service attributes (always present)
	attrs.PutStr("service.name", "monitorable-agent")
	attrs.PutStr("service.version", version.Version)
	attrs.PutStr("monitorable.agent.version", version.Version)

	// Add cached system info attributes
	if r.cachedSystemInfo != nil {
		attrs.PutStr("system.hostname", r.cachedSystemInfo.Hostname)
		attrs.PutStr("system.os.name", r.cachedSystemInfo.OSName)
		attrs.PutStr("system.os.version", r.cachedSystemInfo.OSVersion)
		attrs.PutStr("system.kernel.version", r.cachedSystemInfo.KernelVersion)
		attrs.PutStr("system.architecture", r.cachedSystemInfo.Architecture)
		attrs.PutInt("system.uptime", r.cachedSystemInfo.Uptime)
		attrs.PutBool("system.reboot_required", r.cachedSystemInfo.RebootRequired)
	}

	// Add cached hardware spec attributes
	if r.cachedHardwareSpec != nil {
		attrs.PutStr("system.cpu.model", r.cachedHardwareSpec.CPUModel)
		attrs.PutInt("system.cpu.cores.logical", int64(r.cachedHardwareSpec.CPUCores))
		attrs.PutInt("system.cpu.threads_per_core", int64(r.cachedHardwareSpec.CPUThreadsPerCore))
		attrs.PutInt("system.cpu.threads.total", int64(r.cachedHardwareSpec.CPUThreadsTotal))
		if r.cachedHardwareSpec.TotalRAM > 0 {
			attrs.PutInt("system.memory.total", r.cachedHardwareSpec.TotalRAM)
		}
	}

	// Add cached cloud info attributes. Only Provider is real (detected from DMI/
	// hypervisor signals); instance_id/region are not emitted — there is no real
	// cloud metadata-service integration yet, and fabricating placeholder values
	// for them was the bug this fix removes (see CloudInfo's doc comment).
	if r.cachedCloudInfo != nil && r.cachedCloudInfo.Provider != "" {
		attrs.PutStr("system.cloud.provider", r.cachedCloudInfo.Provider)
	}

	// Add permission error flags
	errorFlags := r.capDetector.GetErrorFlags(r.capabilities)
	if len(errorFlags) > 0 {
		attrs.PutStr("monitorable.permission_errors", fmt.Sprintf("%v", errorFlags))
	}
}

// maxSnapshotBytes bounds a single JSON snapshot resource attribute (docker.containers,
// smart.disks, systemd.services). The per-item caps upstream (Docker's maxContainers,
// systemd's maxUnits, and both collectors' field-length truncation) bound the common
// case; this is the last-resort ceiling against pathological content still producing a
// multi-MB attribute that risks tripping OTLP/gRPC message-size limits and dropping the
// whole batch's metrics for that cycle.
const maxSnapshotBytes = 256 * 1024

// putBoundedJSON marshals v and sets it as attrs[key] when the result is at or under
// maxSnapshotBytes. Over the ceiling, the attribute is omitted for this cycle (the rest
// of the batch still sends) and a WARN is logged once per process lifetime via warned —
// not every cycle, since a host that stays over the ceiling would otherwise flood the
// journal identically every collection interval.
func (r *metricsReceiver) putBoundedJSON(attrs pcommon.Map, key string, v any, warned *bool) {
	data, err := json.Marshal(v)
	if err != nil {
		r.logger.Warn("Failed to marshal "+key, zap.Error(err))
		return
	}
	if len(data) > maxSnapshotBytes {
		if !*warned {
			*warned = true
			r.logger.Warn("snapshot attribute exceeds size ceiling, omitting this cycle",
				zap.String("attribute", key),
				zap.Int("bytes", len(data)),
				zap.Int("limit", maxSnapshotBytes))
		}
		return
	}
	attrs.PutStr(key, string(data))
}

// collectDockerContainers attaches a JSON snapshot of containers (running + recently
// exited) as the `docker.containers` resource attribute, every cycle. No-op (and never
// an error) when Docker is off or absent; a hung daemon is bounded by docker.timeout.
func (r *metricsReceiver) collectDockerContainers(ctx context.Context, attrs pcommon.Map) {
	if r.dockerCollector == nil {
		return // docker.mode=off
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.Docker.Timeout)
	defer cancel()

	res := r.dockerCollector.Collect(cctx)
	r.logDockerTransition(res.Status)
	if res.Status != collectors.DockerPresent {
		return
	}
	r.putBoundedJSON(attrs, "docker.containers", res.Containers, &r.dockerJSONWarned)
}

// collectSmartDisks attaches a JSON snapshot of physical-disk SMART health as the
// `smart.disks` resource attribute, at most once per smart.interval. No-op when SMART is
// off or not yet due; an uncollectable host (no physical disk, or read denied) is
// reported by the SMART collector's own SmartAbsent/SmartNoPermission status below —
// there is no separate capability pre-check gating this call. A hung read is bounded
// by smart.timeout.
func (r *metricsReceiver) collectSmartDisks(ctx context.Context, attrs pcommon.Map) {
	if r.smartCollector == nil {
		return
	}
	now := time.Now()
	if !r.lastSmartAt.IsZero() && now.Sub(r.lastSmartAt) < r.smartInterval {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.Smart.Timeout)
	defer cancel()

	res := r.smartCollector.Collect(cctx)
	r.lastSmartAt = now // throttle all hosts regardless of outcome; reset on restart
	r.logSmartTransition(res.Status)
	if res.Status != collectors.SmartPresent {
		return
	}

	r.putBoundedJSON(attrs, "smart.disks", res.Disks, &r.smartJSONWarned)
}

// logSmartTransition logs once whenever the SMART presence status changes, so steady-state
// produces no log noise. Permission errors yield an actionable WARN; "absent" warns only under mode=on.
func (r *metricsReceiver) logSmartTransition(status collectors.SmartStatus) {
	if status == r.smartPrev {
		return
	}
	r.smartPrev = status
	switch status {
	case collectors.SmartAbsent:
		if r.smartMode == SmartModeOn {
			r.logger.Warn("SMART: no readable physical disk")
		}
	case collectors.SmartNoPermission:
		r.logger.Warn("SMART: device read denied (missing CAP_SYS_RAWIO/CAP_SYS_ADMIN or disk group?)")
	case collectors.SmartPresent:
		r.logger.Info("SMART: disk health snapshot collected")
	}
}

// collectSystemdServices attaches a JSON snapshot of active/failed systemd units as the
// `systemd.services` resource attribute, at most once per systemd.interval. No-op when
// systemd is off or not yet due; an undetected host is reported by the systemd
// collector's own SystemdAbsent/SystemdNoPermission/SystemdEnumError status below —
// there is no separate capability pre-check gating this call (a stale/incorrect
// pre-check could false-negative or false-positive relative to the collector's real
// D-Bus probe). A hung D-Bus call is bounded by systemd.timeout.
func (r *metricsReceiver) collectSystemdServices(ctx context.Context, attrs pcommon.Map) {
	if r.systemdCollector == nil {
		return
	}
	now := time.Now()
	if !r.lastSystemdAt.IsZero() && now.Sub(r.lastSystemdAt) < r.systemdInterval {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.Systemd.Timeout)
	defer cancel()

	res := r.systemdCollector.Collect(cctx)
	r.logSystemdTransition(res.Status, res.Err)
	if res.Status != collectors.SystemdPresent {
		return
	}
	r.lastSystemdAt = now

	r.putBoundedJSON(attrs, "systemd.services", res.Services, &r.systemdJSONWarned)
}

// logSystemdTransition logs once whenever the systemd presence status changes, so steady-state
// produces no log noise. A true permission wall (AccessDenied) yields an actionable WARN; a
// transient enumeration error yields a non-alarming "will retry" WARN. Both include the underlying
// D-Bus error so the journal shows the real failure. "absent" warns only under mode=on.
func (r *metricsReceiver) logSystemdTransition(status collectors.SystemdStatus, err error) {
	if status == r.systemdPrev {
		return
	}
	switch status {
	case collectors.SystemdPresent:
		r.logger.Info("Systemd detected, collecting service snapshot")
	case collectors.SystemdNoPermission:
		r.logger.Warn("Systemd D-Bus reachable but unit enumeration denied by policy (AccessDenied); "+
			"the collector user lacks permission to enumerate units — review D-Bus/polkit policy or run "+
			"under a permitted user/group. No service snapshot.", zap.Error(err))
	case collectors.SystemdEnumError:
		r.logger.Warn("Systemd unit enumeration failed transiently; skipping this snapshot, will retry "+
			"next cycle", zap.Error(err))
	case collectors.SystemdAbsent:
		if r.systemdMode == SystemdModeOn {
			r.logger.Warn("Systemd not reachable but systemd.mode=on; no service snapshot")
		} else {
			r.logger.Debug("Systemd not reachable; not collecting service snapshot")
		}
	}
	r.systemdPrev = status
}

// logDockerTransition logs once whenever the Docker presence status changes, so steady-state
// produces no log noise. EACCES yields an actionable WARN; "absent" warns only under mode=on.
func (r *metricsReceiver) logDockerTransition(status collectors.DockerStatus) {
	if status == r.dockerPrev {
		return
	}
	switch status {
	case collectors.DockerPresent:
		r.logger.Info("Docker detected, collecting container snapshot")
	case collectors.DockerNoPermission:
		r.logger.Warn("Docker socket present but not accessible by the collector user. " +
			"Grant access and restart:  usermod -aG docker monitorable && systemctl restart monitorable-agent")
	case collectors.DockerAbsent:
		if r.dockerMode == DockerModeOn {
			r.logger.Warn("Docker not reachable but docker.mode=on; no container snapshot")
		} else {
			r.logger.Debug("Docker not reachable; not collecting container snapshot")
		}
	}
	r.dockerPrev = status
}
