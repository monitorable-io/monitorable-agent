package collectors

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseOSRelease is a pure function (no I/O) extracted from getOSVersion so the
// PRETTY_NAME parsing is directly unit-testable without touching /etc/os-release.
func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical Debian/Ubuntu os-release",
			in:   "NAME=\"Ubuntu\"\nVERSION=\"24.04 LTS\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\nID=ubuntu\n",
			want: "Ubuntu 24.04 LTS",
		},
		{
			name: "PRETTY_NAME first line",
			in:   "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\n",
			want: "Debian GNU/Linux 12 (bookworm)",
		},
		{
			name: "no PRETTY_NAME line",
			in:   "NAME=\"Alpine Linux\"\nID=alpine\n",
			want: "unknown",
		},
		{
			name: "empty content",
			in:   "",
			want: "unknown",
		},
		{
			name: "unquoted value",
			in:   "PRETTY_NAME=CustomDistro\n",
			want: "CustomDistro",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseOSRelease([]byte(tt.in)); got != tt.want {
				t.Errorf("parseOSRelease(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// isRebootRequired's RHEL/CentOS branch was pure dead code (an if-body containing
// only comments); deleting it must not change behavior. In this test environment
// (Linux CI container, not a reboot-pending host) neither sentinel file exists, so
// the function must return false either way — pinning the one case actually
// exercisable without root/fixture injection.
func TestIsRebootRequired_NoSentinelFiles(t *testing.T) {
	c := NewSystemInfoCollector(nil)
	if c.isRebootRequired() {
		t.Error("isRebootRequired() = true, want false (no reboot-required sentinel present in test env)")
	}
}

func debugCapture() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestGetOSVersion_NoPrettyNameLogsDebug pins b17: the no-PRETTY_NAME parse-failure
// path must log at Debug, like the I/O-failure path already does.
func TestGetOSVersion_NoPrettyNameLogsDebug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	if err := os.WriteFile(path, []byte("NAME=\"Alpine Linux\"\nID=alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger, buf := debugCapture()
	c := &SystemInfoCollector{logger: logger, osReleasePath: path, procVersionPath: "/proc/version"}

	if got := c.getOSVersion(); got != "unknown" {
		t.Fatalf("getOSVersion() = %q, want %q", got, "unknown")
	}
	if !strings.Contains(buf.String(), "unknown OS version") {
		t.Errorf("expected a Debug line about falling back to unknown OS version, got: %s", buf.String())
	}
}

// TestGetKernelVersion_TooFewFieldsLogsDebug pins b17: the too-few-fields parse-failure
// path must log at Debug, like the I/O-failure path already does.
func TestGetKernelVersion_TooFewFieldsLogsDebug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version")
	if err := os.WriteFile(path, []byte("onlyonefield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger, buf := debugCapture()
	c := &SystemInfoCollector{logger: logger, osReleasePath: "/etc/os-release", procVersionPath: path}

	if got := c.getKernelVersion(); got != "unknown" {
		t.Fatalf("getKernelVersion() = %q, want %q", got, "unknown")
	}
	if !strings.Contains(buf.String(), "unknown kernel version") {
		t.Errorf("expected a Debug line about falling back to unknown kernel version, got: %s", buf.String())
	}
}
