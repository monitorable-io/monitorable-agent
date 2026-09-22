package sensors

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/common"
	"github.com/shirou/gopsutil/v4/sensors"
)

// sensorConfig tracks sensor configuration and filtering. Unexported: nothing outside
// this package references it.
type sensorConfig struct {
	sysSensorsPath string // non-empty overrides gopsutil's HostSys env lookup
	sensors        map[string]struct{}
	primarySensor  string
	isBlacklist    bool
	hasWildcards   bool
	skipCollection bool
}

// TemperatureCollector manages temperature sensor collection with filtering
type TemperatureCollector struct {
	config *sensorConfig
	logger *slog.Logger
}

// NewTemperatureCollector creates a new temperature collector with filtering capabilities
func NewTemperatureCollector(primarySensor string, sysSensorsPath string, sensorFilter []string, logger *slog.Logger) *TemperatureCollector {
	config := &sensorConfig{
		primarySensor: primarySensor,
		sensors:       make(map[string]struct{}),
	}

	if sysSensorsPath != "" {
		logger.Info("Using custom sys sensors path", "path", sysSensorsPath)
		config.sysSensorsPath = sysSensorsPath
	}

	// Parse sensor filter configuration
	config.skipCollection = len(sensorFilter) == 1 && sensorFilter[0] == ""
	if len(sensorFilter) > 0 && !config.skipCollection {
		sensorsEnvVal := strings.Join(sensorFilter, ",")

		// handle blacklist (starts with -)
		if strings.HasPrefix(sensorsEnvVal, "-") {
			config.isBlacklist = true
			sensorsEnvVal = sensorsEnvVal[1:]
		}

		for _, sensor := range strings.Split(sensorsEnvVal, ",") {
			sensor = strings.TrimSpace(sensor)
			if sensor != "" {
				config.sensors[sensor] = struct{}{}
				if strings.Contains(sensor, "*") {
					config.hasWildcards = true
				}
			}
		}
	}

	return &TemperatureCollector{
		config: config,
		logger: logger,
	}
}

// CollectTemperatures retrieves temperature data from all available sensors with
// filtering. ctx bounds the underlying sysfs read (the caller wraps it in a timeout —
// see the monitorable receiver — since a stuck hwmon/thermal read must not be allowed
// to wedge the collection goroutine indefinitely).
func (tc *TemperatureCollector) CollectTemperatures(ctx context.Context) (map[string]float64, float64, error) {
	// skip if sensors filter is set to empty string
	if tc.config.skipCollection {
		tc.logger.Debug("Skipping temperature collection")
		return make(map[string]float64), 0, nil
	}

	temps, err := tc.getTempsWithPanicRecovery(ctx)
	if err != nil {
		// retry once on panic (gopsutil/issues/1832)
		temps, err = tc.getTempsWithPanicRecovery(ctx)
		if err != nil {
			tc.logger.Warn("Error collecting temperatures", "err", err)
			return make(map[string]float64), 0, err
		}
	}

	tc.logger.Debug("Raw temperature sensors", "count", len(temps))

	// return if no sensors
	if len(temps) == 0 {
		return make(map[string]float64), 0, nil
	}

	temperatures := make(map[string]float64, len(temps))
	var dashboardTemp float64
	var cpuTemps []float64 // Collect CPU package sensor temperatures separately

	for i, sensor := range temps {
		// scale temperature if needed
		temp := sensor.Temperature
		if temp != 0 && temp < 1 {
			temp = scaleTemperature(temp)
		}

		// skip if temperature is unreasonable
		if temp <= 0 || temp >= 200 {
			tc.logger.Debug("Skipping unreasonable temperature", "sensor", sensor.SensorKey, "temp", temp)
			continue
		}

		sensorName := sensor.SensorKey
		if _, ok := temperatures[sensorName]; ok {
			// if key already exists, append int to key
			sensorName = sensorName + "_" + strconv.Itoa(i)
		}

		// skip if not in whitelist or blacklist
		if !tc.isValidSensor(sensorName) {
			tc.logger.Debug("Sensor filtered out", "sensor", sensorName)
			continue
		}

		// Check if this is a CPU package sensor and collect separately
		if tc.isCPUPackageSensor(sensorName) {
			cpuTemps = append(cpuTemps, temp)
			tc.logger.Debug("CPU package sensor detected", "sensor", sensorName, "temp", temp)
		}

		// set dashboard temperature (prioritize specific primary sensor if set)
		switch tc.config.primarySensor {
		case "":
			// Will be calculated after the loop using CPU prioritization
		case sensorName:
			dashboardTemp = temp
		}

		temperatures[sensorName] = twoDecimals(temp)
		tc.logger.Debug("Collected temperature", "sensor", sensorName, "temp", temp)
	}

	// Calculate dashboard temperature with CPU sensor prioritization (only if no specific primary sensor set)
	if tc.config.primarySensor == "" {
		if len(cpuTemps) > 0 {
			// Use max of CPU package sensors if available
			for _, temp := range cpuTemps {
				dashboardTemp = max(dashboardTemp, temp)
			}
			tc.logger.Debug("Dashboard temperature from CPU package sensors", "cpu_sensors_count", len(cpuTemps), "dashboard_temp", dashboardTemp)
		} else {
			// Fall back to max of all sensors
			for _, temp := range temperatures {
				dashboardTemp = max(dashboardTemp, temp)
			}
			tc.logger.Debug("Dashboard temperature from all sensors (no CPU sensors found)", "dashboard_temp", dashboardTemp)
		}
	}

	tc.logger.Debug("Temperature collection complete", "sensors_collected", len(temperatures), "dashboard_temp", dashboardTemp)
	return temperatures, dashboardTemp, nil
}

// getTempsWithPanicRecovery wraps sensors.TemperaturesWithContext to recover from panics (gopsutil/issues/1832)
func (tc *TemperatureCollector) getTempsWithPanicRecovery(ctx context.Context) (temps []sensors.TemperatureStat, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	// get sensor data (error ignored intentionally as it may be only with one sensor)
	temps, _ = sensors.TemperaturesWithContext(tc.gopsutilContext(ctx))
	return
}

// gopsutilContext layers the sys-sensors-path override (common.EnvKey/HostSysEnvKey)
// onto the caller's ctx, so the override survives even though ctx (and its deadline)
// now comes from the caller on every call rather than being baked in at construction.
func (tc *TemperatureCollector) gopsutilContext(ctx context.Context) context.Context {
	if tc.config.sysSensorsPath == "" {
		return ctx
	}
	return context.WithValue(ctx, common.EnvKey, common.EnvMap{common.HostSysEnvKey: tc.config.sysSensorsPath})
}

// isValidSensor checks if a sensor is valid based on the sensor name and the sensor config
func (tc *TemperatureCollector) isValidSensor(sensorName string) bool {
	// if no sensors configured, everything is valid
	if len(tc.config.sensors) == 0 {
		return true
	}

	// Exact match - return true if whitelist, false if blacklist
	if _, exactMatch := tc.config.sensors[sensorName]; exactMatch {
		return !tc.config.isBlacklist
	}

	// If no wildcards, return true if blacklist, false if whitelist
	if !tc.config.hasWildcards {
		return tc.config.isBlacklist
	}

	// Check for wildcard patterns
	for pattern := range tc.config.sensors {
		if !strings.Contains(pattern, "*") {
			continue
		}
		if match, _ := path.Match(pattern, sensorName); match {
			return !tc.config.isBlacklist
		}
	}

	return tc.config.isBlacklist
}

// isCPUPackageSensor checks if a sensor name represents a CPU package temperature sensor
func (tc *TemperatureCollector) isCPUPackageSensor(sensorName string) bool {
	sensorLower := strings.ToLower(sensorName)

	// Intel CPU sensors (coretemp)
	if strings.Contains(sensorLower, "coretemp") && strings.Contains(sensorLower, "package") {
		return true
	}

	// AMD CPU sensors (k10temp, zenpower)
	if strings.Contains(sensorLower, "k10temp") || strings.Contains(sensorLower, "zenpower") {
		return true
	}

	// Generic CPU package patterns
	if strings.Contains(sensorLower, "cpu") &&
		(strings.Contains(sensorLower, "package") || strings.Contains(sensorLower, "tdie") || strings.Contains(sensorLower, "tctl")) {
		return true
	}

	return false
}

// scaleTemperature scales temperatures in fractional values to reasonable Celsius values
func scaleTemperature(temp float64) float64 {
	if temp > 1 {
		return temp
	}
	scaled100 := temp * 100
	scaled1000 := temp * 1000

	if scaled100 >= 15 && scaled100 <= 95 {
		return scaled100
	} else if scaled1000 >= 15 && scaled1000 <= 95 {
		return scaled1000
	}
	return scaled100
}

// twoDecimals rounds a float to two decimal places
func twoDecimals(val float64) float64 {
	return float64(int(val*100+0.5)) / 100
}
