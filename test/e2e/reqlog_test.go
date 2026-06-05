package e2e

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/reqlog"
)

// captureSink collects emitted request events for assertions.
type captureSink struct {
	mu sync.Mutex
	ev []reqlog.RequestEvent
}

func (c *captureSink) Record(ev reqlog.RequestEvent) {
	c.mu.Lock()
	c.ev = append(c.ev, ev)
	c.mu.Unlock()
}

// waitForEvent polls (the event is emitted just after the response flushes).
func (c *captureSink) waitForEvent(host string) *reqlog.RequestEvent {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for i := range c.ev {
			if c.ev[i].Host == host {
				ev := c.ev[i]
				c.mu.Unlock()
				return &ev
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func TestReqlog_EmitsPerRequestEvent(t *testing.T) {
	dummyPort := startDummyApp(t, "reqlog")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	sink := &captureSink{}
	e.SetReqSink(sink)

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", dummyPort); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "api.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	resp, err := hc.Get("https://" + host + "/foo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	ev := sink.waitForEvent(host)
	if ev == nil {
		t.Fatalf("no request event emitted for %q", host)
	}
	if ev.Slug != "turing" {
		t.Errorf("slug = %q, want turing", ev.Slug)
	}
	if ev.Method != "GET" {
		t.Errorf("method = %q, want GET", ev.Method)
	}
	if ev.Status != 200 {
		t.Errorf("status = %d, want 200", ev.Status)
	}
	if ev.Outcome != reqlog.OutcomeOK {
		t.Errorf("outcome = %q, want ok", ev.Outcome)
	}
	if ev.Path != "/foo" {
		t.Errorf("path = %q, want /foo", ev.Path)
	}
	if ev.RequestID == "" {
		t.Errorf("missing request_id")
	}
	if ev.BytesOut <= 0 {
		t.Errorf("bytes_out = %d, want > 0", ev.BytesOut)
	}
	if ev.IsWebSocket {
		t.Errorf("is_websocket = true for a plain GET")
	}
}

func TestReqlog_NoRouteEvent(t *testing.T) {
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	sink := &captureSink{}
	e.SetReqSink(sink)

	host := "ghost.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	resp, err := hc.Get("https://" + host + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	ev := sink.waitForEvent(host)
	if ev == nil {
		t.Fatalf("no event emitted for the no_route host")
	}
	if ev.Outcome != reqlog.OutcomeNoRoute {
		t.Errorf("outcome = %q, want no_route", ev.Outcome)
	}
	if ev.Slug != "" {
		t.Errorf("slug = %q, want empty on no_route", ev.Slug)
	}
	if ev.Status != 404 {
		t.Errorf("status = %d, want 404", ev.Status)
	}
}
