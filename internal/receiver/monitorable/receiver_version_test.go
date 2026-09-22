package monitorable

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/monitorable-io/monitorable-agent/internal/capabilities"
	"github.com/monitorable-io/monitorable-agent/internal/version"
)

// The dedicated attribute exists because the fleet's baked-in configs upsert a
// hardcoded service.version over the receiver's value; monitorable.agent.version
// passes through old and new configs untouched.
func TestAddCachedResourceAttributes_AgentVersion(t *testing.T) {
	// capabilities must be non-nil: addCachedResourceAttributes unconditionally
	// calls r.capDetector.GetErrorFlags(r.capabilities), which dereferences
	// caps.CanReadTemperatures. In production this field is always populated
	// by NewReceiver before this method ever runs; a bare &metricsReceiver{}
	// hits that unrelated nil-pointer dereference before reaching the
	// assertion below, so this test needs a zero-value (non-nil) capabilities.
	r := &metricsReceiver{capabilities: &capabilities.SystemCapabilities{}}
	attrs := pcommon.NewMap()
	r.addCachedResourceAttributes(attrs)

	v, ok := attrs.Get("monitorable.agent.version")
	if !ok {
		t.Fatal("monitorable.agent.version attribute missing")
	}
	if v.Str() != version.Version {
		t.Fatalf("monitorable.agent.version = %q, want %q", v.Str(), version.Version)
	}
}
