package capabilities

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates path (and its parent dirs) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTestTemperatures_HwmonFirst pins the gopsutil-mirrored probe order: hwmon is
// checked first (the common case: coretemp/k10temp), thermal_zone only as a fallback
// when hwmon is empty. A host with only one of the two must still be detected.
func TestTestTemperatures_HwmonFirst(t *testing.T) {
	tests := []struct {
		name  string
		setup func(root string)
		want  bool
	}{
		{
			name: "hwmon only",
			setup: func(root string) {
				writeFile(t, filepath.Join(root, "class/hwmon/hwmon0/temp1_input"), "45000")
			},
			want: true,
		},
		{
			name: "thermal_zone only",
			setup: func(root string) {
				writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/temp"), "45000")
			},
			want: true,
		},
		{
			name: "neither present",
			setup: func(root string) {
				// no fixtures
			},
			want: false,
		},
		{
			name: "hwmon dir present but empty, thermal_zone has data",
			setup: func(root string) {
				if err := os.MkdirAll(filepath.Join(root, "class/hwmon", "hwmon0"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(root, "class/thermal/thermal_zone0/temp"), "50000")
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(root)
			d := &Detector{sysRoot: root}
			if got := d.testTemperatures(); got != tt.want {
				t.Errorf("testTemperatures() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewDetector_DefaultsSysRootToSlashSys(t *testing.T) {
	d := NewDetector()
	if d.sysRoot != "/sys" {
		t.Errorf("sysRoot = %q, want /sys", d.sysRoot)
	}
}

func TestDetectAll_NoError(t *testing.T) {
	d := NewDetector()
	caps := d.DetectAll()
	if caps == nil {
		t.Fatal("DetectAll() returned nil")
	}
	if caps.DetectedAt.IsZero() {
		t.Error("DetectedAt not set")
	}
}

// No capability maps to a permission code any more: unreadable temperature sensors
// mean "no sensor" on a VM, not "no permission", so the flag that named root was
// removed (see GetErrorFlags). Both detection outcomes must yield no code.
func TestGetErrorFlags_NoCodeForMissingTemperatures(t *testing.T) {
	d := NewDetector()
	for _, readable := range []bool{true, false} {
		if flags := d.GetErrorFlags(&SystemCapabilities{CanReadTemperatures: readable}); len(flags) != 0 {
			t.Errorf("GetErrorFlags(CanReadTemperatures=%v) = %v, want none", readable, flags)
		}
	}
}
