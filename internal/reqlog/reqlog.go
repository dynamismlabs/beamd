// Package reqlog is the edge's per-request event pipeline (request-events-spec).
// It records one self-contained event per completed request (plus per-window
// heartbeats for long-lived connections) to an append-only file; a hosted
// shipper tails that file and ships batches to the control plane. OSS gets the
// local file for free.
package reqlog

import (
	"crypto/rand"
	"fmt"
	"net"
	"time"
)

// RequestEvent is the wire contract. JSON field names are guarded against
// internal/beamdapi.RequestEvent by a conformance test (see conformance_test.go).
// Types may differ from the generated type (int64 vs int, string vs *string) —
// the test checks names only; we match optionality (omitempty) so the JSON
// output is right (request-events-spec §6).
type RequestEvent struct {
	RequestID    string `json:"request_id"`              // edge-minted uuidv7 → the control plane's id PK
	ConnectionID string `json:"connection_id,omitempty"` // shared across a connection's heartbeats
	Slug         string `json:"slug,omitempty"`          // empty on no_route (no session)
	Transport    string `json:"transport,omitempty"`     // tcp or quic; empty when no route/session exists
	Host         string `json:"host"`
	Method       string `json:"method"`
	Path         string `json:"path,omitempty"` // omitted when capture.path is off
	Status       int    `json:"status"`
	Outcome      string `json:"outcome"`
	BytesIn      int64  `json:"bytes_in"`
	BytesOut     int64  `json:"bytes_out"`
	TTFBMs       *int64 `json:"ttfb_ms,omitempty"`
	IsWebSocket  bool   `json:"is_websocket"`
	ClientIP     string `json:"client_ip,omitempty"` // truncated/hashed at the edge
	UserAgent    string `json:"user_agent,omitempty"`
	Referer      string `json:"referer,omitempty"`
	StartedAt    string `json:"started_at"` // RFC 3339
	EndedAt      string `json:"ended_at"`   // RFC 3339
}

// Outcome values — edge-only knowledge the status code can't express.
const (
	OutcomeOK           = "ok"
	OutcomeInProgress   = "in_progress" // a long-connection heartbeat
	OutcomeNoRoute      = "no_route"
	OutcomeBackendError = "backend_error"
	OutcomeTimeout      = "timeout"
	OutcomeSizeLimit    = "size_limit"
	OutcomeClientClosed = "client_closed"
)

// Sink receives completed request events. Record must be non-blocking and
// drop-on-backpressure (fire-and-forget); it never blocks the proxy hot path.
type Sink interface {
	Record(ev RequestEvent)
}

// NopSink drops everything — the default until a file sink is wired (and handy
// in tests).
type NopSink struct{}

// Record implements Sink.
func (NopSink) Record(RequestEvent) {}

// NewID returns a uuidv7 (time-ordered) string. It's the per-emit idempotency
// key: edge-minted, sent as request_id, and mapped to the control plane's uuid
// `id` PK so a retried/replayed batch dedupes (request-events-spec §5.3).
func NewID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	_, _ = rand.Read(b[6:])
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TruncateIP zeroes the host bits of a visitor IP for privacy: /24 for IPv4,
// /48 for IPv6. Returns "" for an unparseable input. Applied at the edge so the
// raw IP never ships (request-events-spec §4.4).
func TruncateIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		v4[3] = 0
		return v4.String()
	}
	v6 := parsed.To16()
	if v6 == nil {
		return ""
	}
	for i := 6; i < 16; i++ {
		v6[i] = 0 // keep the /48 prefix
	}
	return v6.String()
}

// NowRFC3339 is the edge's event timestamp format.
func NowRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
