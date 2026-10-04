package collectors

import (
	"os"
	"path/filepath"
	"strings"
)

// CloudInfoCollector collects cloud provider information. Detection reads real DMI/
// hypervisor signals under sysRoot; there is no metadata-service call (that would be
// a feature, not this fix) — see CloudInfo's doc comment.
type CloudInfoCollector struct {
	sysRoot string // overridable in tests; production always uses "/sys" (NewCloudInfoCollector(""))
}

// CloudInfo holds cloud provider information. InstanceID/Region/Zone were removed:
// the collector never queried the real cloud metadata service for them, it only ever
// set hardcoded placeholder values ("i-placeholder", "us-east-1", ...), which would
// have shipped fabricated resource attributes to every real AWS/GCP/Azure customer.
// Provider is real (detected from DMI/hypervisor signals); the backend columns for
// instance/region/zone stay empty until a real metadata-service integration exists.
type CloudInfo struct {
	Provider string
}

// NewCloudInfoCollector creates a new cloud info collector. sysRoot overrides the
// "/sys" DMI root for tests; production callers pass "".
func NewCloudInfoCollector(sysRoot string) *CloudInfoCollector {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	return &CloudInfoCollector{sysRoot: sysRoot}
}

// Collect gathers cloud provider information
func (c *CloudInfoCollector) Collect() *CloudInfo {
	return &CloudInfo{Provider: c.detectCloudProvider()}
}

// detectCloudProvider tries to detect the cloud provider
func (c *CloudInfoCollector) detectCloudProvider() string {
	if c.checkAWS() {
		return "aws"
	}
	if c.checkGCP() {
		return "gcp"
	}
	if c.checkAzure() {
		return "azure"
	}
	return ""
}

// sysPath joins a path relative to sysRoot.
func (c *CloudInfoCollector) sysPath(rel string) string {
	return filepath.Join(c.sysRoot, rel)
}

// checkAWS checks for AWS environment
func (c *CloudInfoCollector) checkAWS() bool {
	awsFiles := []string{
		"hypervisor/uuid",
		"class/dmi/id/product_uuid",
		"class/dmi/id/board_vendor",
	}

	for _, file := range awsFiles {
		if data, err := os.ReadFile(c.sysPath(file)); err == nil {
			content := strings.ToLower(string(data))
			if strings.Contains(content, "amazon") || strings.Contains(content, "ec2") {
				return true
			}
			// AWS instances often have UUID starting with "EC2" or specific patterns
			if strings.HasPrefix(content, "ec2") {
				return true
			}
		}
	}

	if data, err := os.ReadFile(c.sysPath("class/dmi/id/sys_vendor")); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "amazon") {
			return true
		}
	}

	return false
}

// checkGCP checks for Google Cloud Platform
func (c *CloudInfoCollector) checkGCP() bool {
	if data, err := os.ReadFile(c.sysPath("class/dmi/id/product_name")); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "google") {
			return true
		}
	}

	if data, err := os.ReadFile(c.sysPath("class/dmi/id/sys_vendor")); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "google") {
			return true
		}
	}

	return false
}

// checkAzure checks for Microsoft Azure
func (c *CloudInfoCollector) checkAzure() bool {
	if data, err := os.ReadFile(c.sysPath("class/dmi/id/sys_vendor")); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "microsoft") {
			return true
		}
	}

	if data, err := os.ReadFile(c.sysPath("class/dmi/id/product_name")); err == nil {
		content := strings.ToLower(string(data))
		if strings.Contains(content, "virtual machine") && strings.Contains(content, "microsoft") {
			return true
		}
	}

	return false
}
