package monitorable

import (
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestPutBoundedJSON_UnderLimit pins the common case: a snapshot well under
// maxSnapshotBytes is set verbatim as the attribute.
func TestPutBoundedJSON_UnderLimit(t *testing.T) {
	r := &metricsReceiver{logger: zap.NewNop()}
	attrs := pcommon.NewMap()
	r.putBoundedJSON(attrs, "test.attr", map[string]string{"a": "b"}, &r.dockerJSONWarned)

	v, ok := attrs.Get("test.attr")
	if !ok {
		t.Fatal("attribute missing")
	}
	if v.Str() != `{"a":"b"}` {
		t.Errorf("attribute = %q, want the marshaled JSON", v.Str())
	}
}

// TestPutBoundedJSON_OverLimitOmitsAndWarnsOnce pins b6: a snapshot whose marshaled
// JSON exceeds maxSnapshotBytes is omitted for that cycle, and a WARN is logged once
// (not on every subsequent cycle) even though every call remains over the ceiling.
func TestPutBoundedJSON_OverLimitOmitsAndWarnsOnce(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	r := &metricsReceiver{logger: zap.New(core)}

	// A single string comfortably over maxSnapshotBytes once marshaled.
	big := map[string]string{"blob": strings.Repeat("x", maxSnapshotBytes+1)}

	for i := range 3 {
		attrs := pcommon.NewMap()
		r.putBoundedJSON(attrs, "test.big", big, &r.dockerJSONWarned)
		if _, ok := attrs.Get("test.big"); ok {
			t.Fatalf("cycle %d: attribute set despite exceeding maxSnapshotBytes", i)
		}
	}

	if got := logs.FilterMessageSnippet("exceeds").Len(); got != 1 {
		t.Errorf("WARN logged %d times, want 1 (once, not per cycle)", got)
	}
}
