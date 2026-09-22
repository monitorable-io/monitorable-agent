package collectors

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSysFile(t *testing.T, root, relPath, content string) {
	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// detectCloudProvider reads DMI strings under an injectable sysRoot (defaulting to
// "/sys" in production via NewCloudInfoCollector), which is what makes it testable
// with fixtures instead of requiring the real host's /sys/class/dmi.
func TestDetectCloudProvider(t *testing.T) {
	tests := []struct {
		name  string
		setup func(root string)
		want  string
	}{
		{
			name: "AWS via sys_vendor",
			setup: func(root string) {
				writeSysFile(t, root, "class/dmi/id/sys_vendor", "Amazon EC2\n")
			},
			want: "aws",
		},
		{
			name: "GCP via product_name",
			setup: func(root string) {
				writeSysFile(t, root, "class/dmi/id/product_name", "Google Compute Engine\n")
			},
			want: "gcp",
		},
		{
			name: "Azure via sys_vendor",
			setup: func(root string) {
				writeSysFile(t, root, "class/dmi/id/sys_vendor", "Microsoft Corporation\n")
			},
			want: "azure",
		},
		{
			name:  "no DMI fixtures at all",
			setup: func(root string) {},
			want:  "",
		},
		{
			name: "unrelated vendor",
			setup: func(root string) {
				writeSysFile(t, root, "class/dmi/id/sys_vendor", "Dell Inc.\n")
				writeSysFile(t, root, "class/dmi/id/product_name", "PowerEdge R640\n")
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(root)
			c := NewCloudInfoCollector(root)
			if got := c.detectCloudProvider(); got != tt.want {
				t.Errorf("detectCloudProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCloudInfoCollector_Collect_ProviderOnly(t *testing.T) {
	root := t.TempDir()
	writeSysFile(t, root, "class/dmi/id/sys_vendor", "Amazon EC2\n")
	c := NewCloudInfoCollector(root)
	info := c.Collect()
	if info.Provider != "aws" {
		t.Errorf("Provider = %q, want aws", info.Provider)
	}
}

func TestNewCloudInfoCollector_DefaultsSysRoot(t *testing.T) {
	c := NewCloudInfoCollector("")
	if c.sysRoot != "/sys" {
		t.Errorf("sysRoot = %q, want /sys", c.sysRoot)
	}
}
