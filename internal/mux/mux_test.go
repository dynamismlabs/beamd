package mux

import (
	"io"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestDefaultStreamWindowIs4MiB(t *testing.T) {
	if DefaultStreamWindow != 4<<20 {
		t.Fatalf("DefaultStreamWindow = %d, want 4 MiB (%d)", DefaultStreamWindow, 4<<20)
	}
}

func TestConfigWindow(t *testing.T) {
	// 0 → the tuned 4 MiB default (not yamux's 256 KiB library default).
	if got := Config(0).MaxStreamWindowSize; got != DefaultStreamWindow {
		t.Errorf("Config(0) window = %d, want DefaultStreamWindow %d", got, DefaultStreamWindow)
	}
	// A non-zero value is passed through verbatim.
	const w = 8 << 20
	if got := Config(w).MaxStreamWindowSize; got != w {
		t.Errorf("Config(%d) window = %d, want %d", w, got, w)
	}
}

// TestConfigChangesOnlyWindow pins Part A's scope: the only field that differs
// from yamux's library default is MaxStreamWindowSize (plus beamd's pre-existing
// keepalive/write-timeout/log-output settings). If Part B yamux hardening
// (AcceptBacklog, EnableKeepAlive, stream timeouts) ever leaks into Part A, this
// fails.
func TestConfigChangesOnlyWindow(t *testing.T) {
	def := yamux.DefaultConfig()
	got := Config(8 << 20)

	// The one intended change.
	if got.MaxStreamWindowSize != 8<<20 {
		t.Errorf("MaxStreamWindowSize = %d, want %d", got.MaxStreamWindowSize, 8<<20)
	}
	// beamd's long-standing framing settings (unchanged by Part A).
	if got.KeepAliveInterval != 20*time.Second {
		t.Errorf("KeepAliveInterval = %v, want 20s", got.KeepAliveInterval)
	}
	if got.ConnectionWriteTimeout != 30*time.Second {
		t.Errorf("ConnectionWriteTimeout = %v, want 30s", got.ConnectionWriteTimeout)
	}
	if got.LogOutput != io.Discard {
		t.Errorf("LogOutput = %v, want io.Discard", got.LogOutput)
	}
	// Part B yamux hardening must stay at the yamux defaults during Part A.
	if got.AcceptBacklog != def.AcceptBacklog {
		t.Errorf("AcceptBacklog = %d, want yamux default %d (Part B only)", got.AcceptBacklog, def.AcceptBacklog)
	}
	if got.EnableKeepAlive != def.EnableKeepAlive {
		t.Errorf("EnableKeepAlive = %v, want yamux default %v (Part B only)", got.EnableKeepAlive, def.EnableKeepAlive)
	}
	if got.StreamOpenTimeout != def.StreamOpenTimeout {
		t.Errorf("StreamOpenTimeout = %v, want yamux default %v (Part B only)", got.StreamOpenTimeout, def.StreamOpenTimeout)
	}
	if got.StreamCloseTimeout != def.StreamCloseTimeout {
		t.Errorf("StreamCloseTimeout = %v, want yamux default %v (Part B only)", got.StreamCloseTimeout, def.StreamCloseTimeout)
	}
}
