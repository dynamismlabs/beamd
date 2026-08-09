package tunnel

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPrefixSetupTimeoutIsTransportSpecific(t *testing.T) {
	if got := PrefixSetupTimeout(KindQUIC); got != 5*time.Second {
		t.Fatalf("QUIC prefix setup timeout = %s, want 5s", got)
	}
	if got := PrefixSetupTimeout(KindYamux); got != 60*time.Second {
		t.Fatalf("yamux prefix setup timeout = %s, want 60s", got)
	}
	if got := PrefixSetupTimeout(Kind("unknown")); got != 5*time.Second {
		t.Fatalf("unknown transport prefix setup timeout = %s, want conservative 5s", got)
	}
}

func TestStableErrorCodes(t *testing.T) {
	tests := map[ErrorCode]uint64{
		CloseNormal:     0x00,
		CloseShutdown:   0x01,
		CloseProtocol:   0x02,
		CloseAuth:       0x03,
		CloseSuperseded: 0x04,
		CloseCapacity:   0x05,
		CloseIdle:       0x06,
		StreamCanceled:  0x10,
		StreamCapacity:  0x11,
	}
	for code, want := range tests {
		if uint64(code) != want {
			t.Errorf("code %#x = %#x, want %#x", code, uint64(code), want)
		}
	}
}

func TestCloseReasonMapping(t *testing.T) {
	tests := []struct {
		info CloseInfo
		want string
	}{
		{CloseInfo{CodeValid: true, Code: CloseNormal}, "normal"},
		{CloseInfo{CodeValid: true, Code: CloseSuperseded}, "normal"},
		{CloseInfo{CodeValid: true, Code: CloseShutdown}, "shutdown"},
		{CloseInfo{CodeValid: true, Code: CloseProtocol}, "protocol"},
		{CloseInfo{CodeValid: true, Code: CloseAuth}, "protocol"},
		{CloseInfo{CodeValid: true, Code: CloseIdle}, "idle"},
		{CloseInfo{CodeValid: true, Code: CloseCapacity}, "other"},
		{CloseInfo{CodeValid: true, Code: 0xff}, "other"},
		{CloseInfo{Reason: "idle"}, "idle"},
		{CloseInfo{Reason: "network"}, "network"},
		{CloseInfo{Reason: "arbitrary peer text"}, "other"},
	}
	for _, test := range tests {
		if got := CloseReason(test.info); got != test.want {
			t.Errorf("CloseReason(%+v) = %q, want %q", test.info, got, test.want)
		}
	}
}

func TestSanitizeReasonUTF8AndLength(t *testing.T) {
	reason := strings.Repeat("é", 200)
	got := sanitizeReason(reason)
	if len(got) > 256 {
		t.Fatalf("reason length = %d, want <= 256", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("reason is invalid UTF-8: %x", got[len(got)-4:])
	}
	invalid := string([]byte{'a', 0xff, 'b'})
	if got := sanitizeReason(invalid); strings.ContainsRune(got, '\uFFFD') == false {
		t.Fatalf("invalid UTF-8 was not replaced: %q", got)
	}
}

type observedChild struct {
	terminated atomic.Bool
}

func (c *observedChild) parentTerminated() {
	c.terminated.Store(true)
}

func TestSessionStateFirstEventWinsAndChildrenPrecedeDone(t *testing.T) {
	state := newSessionState()
	child := &observedChild{}
	if !state.register(child) {
		t.Fatal("initial child registration failed")
	}
	firstCause := errors.New("first")
	if !state.claim(CloseInfo{CodeValid: true, Code: CloseProtocol, Cause: firstCause}) {
		t.Fatal("first terminal claim lost")
	}
	if state.claim(CloseInfo{CodeValid: true, Code: CloseShutdown}) {
		t.Fatal("second terminal claim unexpectedly won")
	}

	state.finish(CloseInfo{Reason: "network"})
	<-state.doneChan()
	if !child.terminated.Load() {
		t.Fatal("Session.Done became observable before child termination")
	}
	info := state.closeInfo()
	if info.Code != CloseProtocol || !errors.Is(info.Cause, firstCause) {
		t.Fatalf("CloseInfo = %+v, want first terminal event", info)
	}
	if state.register(&observedChild{}) {
		t.Fatal("registered a child after session termination")
	}
}
