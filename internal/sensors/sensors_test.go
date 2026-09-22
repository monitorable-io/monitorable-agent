package sensors

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
)

// discardLogger keeps test output pristine — the collector logs at Debug/Info.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newCollector(primary string, filter []string) *TemperatureCollector {
	return NewTemperatureCollector(primary, "", filter, discardLogger())
}

func TestNewTemperatureCollector_FilterParsing(t *testing.T) {
	tests := []struct {
		name          string
		filter        []string
		wantSkip      bool
		wantBlacklist bool
		wantWildcards bool
		wantSensors   []string // keys expected in config.sensors ("" -> expect empty set)
	}{
		{name: "nil filter collects all, empty set", filter: nil},
		{name: "single empty string disables collection", filter: []string{""}, wantSkip: true},
		{name: "whitelist exact names", filter: []string{"coretemp_package", "acpitz"}, wantSensors: []string{"coretemp_package", "acpitz"}},
		{name: "leading dash marks blacklist and is stripped", filter: []string{"-acpitz"}, wantBlacklist: true, wantSensors: []string{"acpitz"}},
		{name: "asterisk flags wildcards", filter: []string{"coretemp*"}, wantWildcards: true, wantSensors: []string{"coretemp*"}},
		{name: "blacklist with wildcards across multiple entries", filter: []string{"-coretemp*", "k10temp*"}, wantBlacklist: true, wantWildcards: true, wantSensors: []string{"coretemp*", "k10temp*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCollector("", tt.filter)
			if c.config.skipCollection != tt.wantSkip {
				t.Errorf("skipCollection = %v, want %v", c.config.skipCollection, tt.wantSkip)
			}
			if c.config.isBlacklist != tt.wantBlacklist {
				t.Errorf("isBlacklist = %v, want %v", c.config.isBlacklist, tt.wantBlacklist)
			}
			if c.config.hasWildcards != tt.wantWildcards {
				t.Errorf("hasWildcards = %v, want %v", c.config.hasWildcards, tt.wantWildcards)
			}
			for _, s := range tt.wantSensors {
				if _, ok := c.config.sensors[s]; !ok {
					t.Errorf("config.sensors missing %q (got %v)", s, c.config.sensors)
				}
			}
			if len(tt.wantSensors) != len(c.config.sensors) {
				t.Errorf("config.sensors size = %d, want %d (%v)", len(c.config.sensors), len(tt.wantSensors), c.config.sensors)
			}
		})
	}
}

func TestIsValidSensor(t *testing.T) {
	tests := []struct {
		name   string
		filter []string
		sensor string
		want   bool
	}{
		{name: "no filter allows everything", filter: nil, sensor: "anything", want: true},
		{name: "whitelist keeps exact match", filter: []string{"coretemp_package"}, sensor: "coretemp_package", want: true},
		{name: "whitelist drops non-match", filter: []string{"coretemp_package"}, sensor: "acpitz", want: false},
		{name: "blacklist drops exact match", filter: []string{"-acpitz"}, sensor: "acpitz", want: false},
		{name: "blacklist keeps non-match", filter: []string{"-acpitz"}, sensor: "coretemp_package", want: true},
		{name: "whitelist wildcard keeps match", filter: []string{"coretemp*"}, sensor: "coretemp_package", want: true},
		{name: "whitelist wildcard drops non-match", filter: []string{"coretemp*"}, sensor: "acpitz", want: false},
		{name: "blacklist wildcard drops match", filter: []string{"-coretemp*"}, sensor: "coretemp_package", want: false},
		{name: "blacklist wildcard keeps non-match", filter: []string{"-coretemp*"}, sensor: "acpitz", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCollector("", tt.filter)
			if got := c.isValidSensor(tt.sensor); got != tt.want {
				t.Errorf("isValidSensor(%q) = %v, want %v", tt.sensor, got, tt.want)
			}
		})
	}
}

func TestIsCPUPackageSensor(t *testing.T) {
	c := newCollector("", nil)
	tests := []struct {
		sensor string
		want   bool
	}{
		{"coretemp_packageid0", true}, // Intel: coretemp + package
		{"CORETEMP_Package", true},    // case-insensitive
		{"coretemp_core0", false},     // Intel per-core (no "package", no "cpu")
		{"k10temp_tctl", true},        // AMD k10temp
		{"zenpower_tdie", true},       // AMD zenpower
		{"cpu_thermal_tdie", true},    // generic: cpu + tdie
		{"cpu0_package", true},        // generic: cpu + package
		{"cpu_thermal", false},        // cpu present but no package/tdie/tctl
		{"acpitz", false},             // ACPI thermal zone, not a CPU package
		{"nvme_composite", false},     // unrelated device
	}
	for _, tt := range tests {
		t.Run(tt.sensor, func(t *testing.T) {
			if got := c.isCPUPackageSensor(tt.sensor); got != tt.want {
				t.Errorf("isCPUPackageSensor(%q) = %v, want %v", tt.sensor, got, tt.want)
			}
		})
	}
}

func TestScaleTemperature(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "already celsius passes through", in: 45.0, want: 45.0},
		{name: "hundredths scale into range", in: 0.45, want: 45.0},      // *100 = 45 in [15,95]
		{name: "thousandths scale into range", in: 0.045, want: 45.0},    // *100 = 4.5 out, *1000 = 45 in
		{name: "neither range falls back to *100", in: 0.009, want: 0.9}, // *100 = 0.9, *1000 = 9, both out
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scaleTemperature(tt.in); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("scaleTemperature(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTwoDecimals(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "rounds up", in: 45.678, want: 45.68},
		{name: "rounds down", in: 45.674, want: 45.67},
		{name: "whole number unchanged", in: 45.0, want: 45.0},
		{name: "half rounds up", in: 0.005, want: 0.01},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := twoDecimals(tt.in); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("twoDecimals(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCollectTemperatures_SkipCollection(t *testing.T) {
	// A filter of exactly [""] disables collection entirely — verified without
	// touching hardware sensors (the early return precedes the gopsutil read).
	c := newCollector("", []string{""})
	temps, dashboard, err := c.CollectTemperatures(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dashboard != 0 {
		t.Errorf("dashboard temp = %v, want 0", dashboard)
	}
	if len(temps) != 0 {
		t.Errorf("temps = %v, want empty map", temps)
	}
}
