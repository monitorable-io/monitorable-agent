package collectors

import "testing"

func TestNvmeControllerNode(t *testing.T) {
	cases := map[string]string{
		"nvme0n1": "nvme0", "nvme1n1": "nvme1", "nvme0n1p2": "nvme0",
		"nvme10n3": "nvme10", "nvme0": "nvme0", "sda": "sda", "vdb": "vdb",
	}
	for in, want := range cases {
		if got := nvmeControllerNode(in); got != want {
			t.Errorf("nvmeControllerNode(%q) = %q, want %q", in, got, want)
		}
	}
}
