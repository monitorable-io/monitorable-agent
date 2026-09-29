package monitorable

import (
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/monitorable-io/monitorable-agent/internal/capabilities"
	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// system.network.ip.private was removed: it was "the first RFC1918 IPv4 on any
// interface", which on a typical VPS is docker0 or a bridge, not the host's
// address. The backend no longer reads it. Collect() runs against the real
// host, so on any machine with a private address (dev boxes, CI runners with
// docker0) the old code emitted it.
func TestAddCachedResourceAttributes_NoPrivateIP(t *testing.T) {
	r := &metricsReceiver{
		capabilities:      &capabilities.SystemCapabilities{},
		cachedNetworkInfo: collectors.NewNetworkInfoCollector().Collect(),
	}
	attrs := pcommon.NewMap()
	r.addCachedResourceAttributes(attrs)

	attrs.Range(func(k string, _ pcommon.Value) bool {
		if strings.Contains(k, "ip.private") {
			t.Errorf("resource attribute %q is still emitted", k)
		}
		return true
	})
}
