package edge

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dynamismlabs/beamd/internal/reqlog"
)

// SetReqSink registers the per-request event sink (the file sink, plus an
// optional shipper). Must be called before Serve — it's read lock-free on the
// hot path. Defaults to a NopSink, so the edge runs fine without it.
func (e *Edge) SetReqSink(s reqlog.Sink) { e.reqSink = s }

// reqMeta is the per-request metadata captured before proxying. Analytics fields
// are already gated by the capture config (empty when not captured).
type reqMeta struct {
	host, slug, method, path     string
	clientIP, userAgent, referer string
	started                      time.Time
}

// metaFor builds reqMeta from a request, applying the edge's capture + IP
// minimization config (billing fields always populated; analytics fields gated).
func (e *Edge) metaFor(host, slug, method, path, remoteAddr, ua, ref string, started time.Time) reqMeta {
	m := reqMeta{host: host, slug: slug, method: method, started: started}
	if e.capPath {
		m.path = path
	}
	if e.capClientIP {
		ip := remoteAddr
		if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
			ip = h
		}
		if e.ipTruncate {
			ip = reqlog.TruncateIP(ip)
		}
		m.clientIP = ip
	}
	if e.capUserAgent {
		m.userAgent = ua
	}
	if e.capReferer {
		m.referer = ref
	}
	return m
}

// emitRequest records a completed (non-WS) request event.
func (e *Edge) emitRequest(m reqMeta, status int, outcome string, bytesIn, bytesOut int64, firstByteAt time.Time) {
	ev := reqlog.RequestEvent{
		RequestID: reqlog.NewID(),
		Slug:      m.slug,
		Host:      m.host,
		Method:    m.method,
		Path:      m.path,
		Status:    status,
		Outcome:   outcome,
		BytesIn:   bytesIn,
		BytesOut:  bytesOut,
		ClientIP:  m.clientIP,
		UserAgent: m.userAgent,
		Referer:   m.referer,
		StartedAt: reqlog.NowRFC3339(m.started),
		EndedAt:   reqlog.NowRFC3339(time.Now()),
	}
	if !firstByteAt.IsZero() {
		ms := firstByteAt.Sub(m.started).Milliseconds()
		ev.TTFBMs = &ms
	}
	e.reqSink.Record(ev)
}

// wsCountingConn wraps a hijacked (WebSocket/upgrade) conn to count bytes in/out
// with atomic counters — the heartbeat goroutine reads them while the proxy
// copies, so they MUST be atomic (request-events-spec §4.4). Closing it stops
// the heartbeat.
type wsCountingConn struct {
	net.Conn
	in        atomic.Int64
	out       atomic.Int64
	closeOnce sync.Once
	done      chan struct{}
	onClose   func()
}

func (c *wsCountingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.in.Add(int64(n))
	return n, err
}

func (c *wsCountingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.out.Add(int64(n))
	return n, err
}

func (c *wsCountingConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		close(c.done)
	})
	return err
}

// startWSHeartbeat wraps a hijacked conn and runs a heartbeat goroutine emitting
// per-window `in_progress` events — and a final `ok` event on close — all sharing
// one connection_id. Each event carries its window's DELTA bytes and a distinct
// window-start `started_at` (the load-bearing invariant: per-window started_at so
// bytes bucket into the right period; per-emit request_id so retries dedupe).
func (e *Edge) startWSHeartbeat(m reqMeta, raw net.Conn, callbacks ...func()) net.Conn {
	var onClose func()
	if len(callbacks) > 0 {
		onClose = callbacks[0]
	}
	cc := &wsCountingConn{Conn: raw, done: make(chan struct{}), onClose: onClose}
	connID := reqlog.NewID()
	interval := e.reqHeartbeat
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastIn, lastOut int64
		winStart := m.started
		emit := func(outcome string) {
			in, out := cc.in.Load(), cc.out.Load()
			now := time.Now()
			e.reqSink.Record(reqlog.RequestEvent{
				RequestID:    reqlog.NewID(),
				ConnectionID: connID,
				Slug:         m.slug,
				Host:         m.host,
				Method:       m.method,
				Path:         m.path,
				Status:       101, // Switching Protocols
				Outcome:      outcome,
				BytesIn:      in - lastIn,
				BytesOut:     out - lastOut,
				IsWebSocket:  true,
				ClientIP:     m.clientIP,
				UserAgent:    m.userAgent,
				Referer:      m.referer,
				StartedAt:    reqlog.NowRFC3339(winStart),
				EndedAt:      reqlog.NowRFC3339(now),
			})
			lastIn, lastOut, winStart = in, out, now
		}
		for {
			select {
			case <-ticker.C:
				emit(reqlog.OutcomeInProgress)
			case <-cc.done:
				emit(reqlog.OutcomeOK) // final window [last-window-end, close]
				// The close callback releases the edge's proxy worker. Keep it
				// behind the final event so Shutdown cannot finish and close
				// the request sink while this record is still in flight.
				if cc.onClose != nil {
					cc.onClose()
				}
				return
			}
		}
	}()
	return cc
}

// countingReader counts bytes read from a request body (bytes_in), delegating
// Close to the underlying body.
type countingReader struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (cr *countingReader) Read(b []byte) (int, error) {
	n, err := cr.rc.Read(b)
	cr.n.Add(int64(n))
	return n, err
}

func (cr *countingReader) Close() error { return cr.rc.Close() }

func (cr *countingReader) Count() int64 { return cr.n.Load() }
