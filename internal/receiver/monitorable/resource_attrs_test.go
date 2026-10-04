package monitorable

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.uber.org/zap"

	"github.com/monitorable-io/monitorable-agent/internal/capabilities"
)

// newRefreshedReceiver returns a receiver whose cached metadata was filled by
// the real collectors against the real host, so the resource-attribute tests
// exercise the actual emission path.
func newRefreshedReceiver(t *testing.T) *metricsReceiver {
	t.Helper()
	r := &metricsReceiver{
		logger:       zap.NewNop(),
		capabilities: &capabilities.SystemCapabilities{},
		capDetector:  capabilities.NewDetector(),
	}
	r.collectors = r.initializeCollectors(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.refreshCachedMetadata()
	return r
}

func refreshedAttrs(t *testing.T) pcommon.Map {
	t.Helper()
	attrs := pcommon.NewMap()
	newRefreshedReceiver(t).addCachedResourceAttributes(attrs)
	if _, ok := attrs.Get("system.hostname"); !ok {
		t.Fatal("system.hostname missing: the refresh path did not run")
	}
	return attrs
}

// system.network.ip.private was removed: it was "the first RFC1918 IPv4 on any
// interface", which on a typical VPS is docker0 or a bridge, not the host's
// address. The backend no longer reads it. The collectors run against the real
// host, so on any machine with a private address (dev boxes, CI runners with
// docker0) the old code emitted it.
func TestAddCachedResourceAttributes_NoPrivateIP(t *testing.T) {
	refreshedAttrs(t).Range(func(k string, _ pcommon.Value) bool {
		if strings.Contains(k, "ip.private") {
			t.Errorf("resource attribute %q is still emitted", k)
		}
		return true
	})
}

// MAC addresses are no longer collected or emitted: the backend never read
// system.network.interface.<N>.mac and it carried a privacy footprint.
func TestAddCachedResourceAttributes_NoMACAddresses(t *testing.T) {
	refreshedAttrs(t).Range(func(k string, _ pcommon.Value) bool {
		if strings.HasPrefix(k, "system.network.interface.") {
			t.Errorf("resource attribute %q is still emitted", k)
		}
		return true
	})
}
