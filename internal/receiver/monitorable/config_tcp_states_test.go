package monitorable

import "testing"

func TestTCPStatesModeDefaultsToOff(t *testing.T) {
	if got := createDefaultConfig().(*Config).TCPStates.Mode; got != TCPStatesModeOff {
		t.Errorf("createDefaultConfig tcp_states.mode = %q, want off", got)
	}
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.TCPStates.Mode != TCPStatesModeOff {
		t.Errorf("empty Config after Validate: tcp_states.mode = %q, want off (a v1.8.0 config keeps hostmetrics' counts)", cfg.TCPStates.Mode)
	}
}

func TestTCPStatesModeAccepted(t *testing.T) {
	for _, mode := range []string{TCPStatesModeAuto, TCPStatesModeOn, TCPStatesModeOff} {
		cfg := &Config{TCPStates: TCPStatesConfig{Mode: mode}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", mode, err)
		}
		if cfg.TCPStates.Mode != mode {
			t.Errorf("mode %q changed to %q", mode, cfg.TCPStates.Mode)
		}
	}
}

func TestTCPStatesModeInvalid(t *testing.T) {
	cfg := &Config{TCPStates: TCPStatesConfig{Mode: "yes"}}
	err := cfg.Validate()
	if err == nil || err.Error() != `monitorable: invalid tcp_states.mode "yes" (want auto|on|off)` {
		t.Fatalf("Validate = %v", err)
	}
}
