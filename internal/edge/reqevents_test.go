package edge

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/reqlog"
)

type capSink struct {
	mu sync.Mutex
	ev []reqlog.RequestEvent
}

func (c *capSink) Record(e reqlog.RequestEvent) {
	c.mu.Lock()
	c.ev = append(c.ev, e)
	c.mu.Unlock()
}

func (c *capSink) events() []reqlog.RequestEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reqlog.RequestEvent(nil), c.ev...)
}

// The WS/SSE heartbeat (request-events-spec §4.4) emits one event per window
// while a long connection is open + a final event on close — all sharing one
// connection_id, each with its OWN per-emit request_id (so retries dedupe) and a
// window-start started_at. This is the spec's "load-bearing invariant".
func TestWSHeartbeatEmitsPerWindow(t *testing.T) {
	sink := &capSink{}
	e := &Edge{reqSink: sink, reqHeartbeat: 30 * time.Millisecond}
	meta := reqMeta{host: "ws-acme.beamd.run", slug: "acme", method: "GET", started: time.Now()}

	client, server := net.Pipe()
	wrapped := e.startWSHeartbeat(meta, server)

	// Push 5 bytes through so the byte counters move (net.Pipe is synchronous).
	go func() { _, _ = client.Write([]byte("hello")) }()
	buf := make([]byte, 5)
	_, _ = wrapped.Read(buf) // bytes_in += 5

	time.Sleep(100 * time.Millisecond) // ~3 heartbeat windows
	_ = wrapped.Close()                // → final ok event
	_ = client.Close()
	time.Sleep(20 * time.Millisecond) // let the final emit land

	evs := sink.events()
	if len(evs) < 2 {
		t.Fatalf("want ≥2 events (heartbeats + final), got %d", len(evs))
	}

	connID := evs[0].ConnectionID
	if connID == "" {
		t.Fatalf("events must carry a connection_id")
	}
	ids := map[string]bool{}
	var inProgress, final int
	var totalIn int64
	for _, ev := range evs {
		if ev.ConnectionID != connID {
			t.Errorf("connection_id differs across windows: %q vs %q", ev.ConnectionID, connID)
		}
		if !ev.IsWebSocket {
			t.Errorf("is_websocket must be true for a WS heartbeat")
		}
		if ids[ev.RequestID] {
			t.Errorf("duplicate request_id %q — each emit must have its own", ev.RequestID)
		}
		ids[ev.RequestID] = true
		switch ev.Outcome {
		case reqlog.OutcomeInProgress:
			inProgress++
		case reqlog.OutcomeOK:
			final++
		}
		totalIn += ev.BytesIn
	}
	if inProgress < 1 {
		t.Errorf("want ≥1 in_progress window, got %d", inProgress)
	}
	if final != 1 {
		t.Errorf("want exactly 1 final ok event, got %d", final)
	}
	if totalIn != 5 {
		t.Errorf("bytes_in across windows = %d, want 5 (delta bytes sum to the total)", totalIn)
	}
}
