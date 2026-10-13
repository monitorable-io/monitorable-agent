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
	// addCachedResourceAttributes calls r.capDetector.GetErrorFlags(r.capabilities);
	// in production NewReceiver populates both before this method ever runs, so the
	// test mirrors that rather than relying on nil-safety it does not promise.
	r := &metricsReceiver{capabilities: &capabilities.SystemCapabilities{}, capDetector: capabilities.NewDetector()}
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
