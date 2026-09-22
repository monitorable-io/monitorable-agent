package monitorable

import (
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/monitorable-io/monitorable-agent/internal/collectors"
)

// The receiver must log a real permission denial and a transient enumeration
// error differently: a permission wall is an actionable WARN, a transient error
// is a non-alarming "will retry" WARN. Both must include the underlying error so
// the journal shows the real D-Bus failure rather than a misleading "denied".
func TestLogSystemdTransitionBranches(t *testing.T) {
	tests := []struct {
		name      string
		status    collectors.SystemdStatus
		err       error
		wantLevel zapcore.Level
		wantSub   string // substring expected in the message
		wantErr   bool   // expect the underlying error attached as a field
	}{
		{"denied", collectors.SystemdNoPermission, errors.New("Rejected send message"), zapcore.WarnLevel, "denied", true},
		{"transient", collectors.SystemdEnumError, errors.New("dbus: connection closed by user"), zapcore.WarnLevel, "transient", true},
		{"present", collectors.SystemdPresent, nil, zapcore.InfoLevel, "detected", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			r := &metricsReceiver{
				logger:      zap.New(core),
				systemdPrev: collectors.SystemdStatus(-1), // unset → first status logs
				systemdMode: SystemdModeAuto,
			}
			r.logSystemdTransition(tc.status, tc.err)

			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("got %d log entries, want 1", len(entries))
			}
			e := entries[0]
			if e.Level != tc.wantLevel {
				t.Errorf("level = %v, want %v", e.Level, tc.wantLevel)
			}
			if !strings.Contains(strings.ToLower(e.Message), tc.wantSub) {
				t.Errorf("message %q missing %q", e.Message, tc.wantSub)
			}
			if _, hasErr := e.ContextMap()["error"]; hasErr != tc.wantErr {
				t.Errorf("error field present = %v, want %v (fields: %v)", hasErr, tc.wantErr, e.ContextMap())
			}
		})
	}
}
