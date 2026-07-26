package edge

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dynamismlabs/beamd/internal/tunnel"
)

// metrics holds the counters and gauges exposed at /metrics. Plain
// stdlib; we deliberately avoid pulling in the prometheus client lib
// to keep deps tight in M6 — text exposition format is straightforward
// to emit by hand.
type metrics struct {
	activeSessions atomic.Int64
	activeTunnels  atomic.Int64
	// sessionsCreatedTotal monotonically counts every session ever opened.
	// Tests use it to wait for a fresh session after a forced disconnect.
	sessionsCreatedTotal atomic.Int64

	listenerUp       [2]atomic.Int64
	sessionActive    [2][2]atomic.Int64
	sessionTotal     [2]atomic.Int64
	streamActive     [2]atomic.Int64
	handshakeErrors  [2][4]atomic.Int64
	sessionCloses    [2][6]atomic.Int64
	streamOpenErrors [2][3]atomic.Int64
	capacityRejected [5]atomic.Int64
	sessionCapacity  atomic.Int64
	globalCapacity   atomic.Int64
	yamuxWindow      atomic.Int64

	mu sync.Mutex
	// requestsByStatus[status] = total seen
	requestsByStatus map[int]int64
}

var (
	transportLabels       = [...]string{"tcp", "quic"}
	sessionStateLabels    = [...]string{"preauth", "authenticated"}
	handshakeReasonLabels = [...]string{"timeout", "tls", "protocol", "other"}
	closeReasonLabels     = [...]string{"normal", "shutdown", "idle", "protocol", "network", "other"}
	openReasonLabels      = [...]string{"timeout", "closed", "other"}
	capacityScopeLabels   = [...]string{"tls_handshake", "preauth_session", "authenticated_session", "session_stream", "global_stream"}
)

func newMetrics() *metrics {
	return &metrics{
		requestsByStatus: make(map[int]int64),
	}
}

func (m *metrics) recordRequest(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestsByStatus[status]++
}

func (m *metrics) configure(sessionCapacity, globalCapacity int, yamuxWindow int64) {
	m.sessionCapacity.Store(int64(sessionCapacity))
	m.globalCapacity.Store(int64(globalCapacity))
	m.yamuxWindow.Store(yamuxWindow)
}

func transportMetricIndex(kind tunnel.Kind) int {
	if kind == tunnel.KindQUIC {
		return 1
	}
	return 0
}

func fixedIndex(value string, labels []string) int {
	for i, label := range labels {
		if value == label {
			return i
		}
	}
	return -1
}

func (m *metrics) setListener(kind tunnel.Kind, up bool) {
	var value int64
	if up {
		value = 1
	}
	m.listenerUp[transportMetricIndex(kind)].Store(value)
}

func (m *metrics) addSessionState(kind tunnel.Kind, state string, delta int64) {
	index := fixedIndex(state, sessionStateLabels[:])
	if index >= 0 {
		m.sessionActive[transportMetricIndex(kind)][index].Add(delta)
	}
}

func (m *metrics) recordSessionCreated(kind tunnel.Kind) {
	m.sessionTotal[transportMetricIndex(kind)].Add(1)
}

func (m *metrics) addStream(kind tunnel.Kind, delta int64) {
	m.streamActive[transportMetricIndex(kind)].Add(delta)
}

func (m *metrics) recordHandshakeError(kind tunnel.Kind, reason string) {
	index := fixedIndex(reason, handshakeReasonLabels[:])
	if index < 0 {
		index = len(handshakeReasonLabels) - 1
	}
	m.handshakeErrors[transportMetricIndex(kind)][index].Add(1)
}

func (m *metrics) recordSessionClose(kind tunnel.Kind, reason string) {
	index := fixedIndex(reason, closeReasonLabels[:])
	if index < 0 {
		index = len(closeReasonLabels) - 1
	}
	m.sessionCloses[transportMetricIndex(kind)][index].Add(1)
}

func (m *metrics) recordStreamOpenError(kind tunnel.Kind, reason string) {
	index := fixedIndex(reason, openReasonLabels[:])
	if index < 0 {
		index = len(openReasonLabels) - 1
	}
	m.streamOpenErrors[transportMetricIndex(kind)][index].Add(1)
}

func (m *metrics) recordCapacityRejection(scope string) {
	index := fixedIndex(scope, capacityScopeLabels[:])
	if index >= 0 {
		m.capacityRejected[index].Add(1)
	}
}

// writeText emits the Prometheus text exposition format. certIssuance
// is passed in because it lives on certs.Manager.
func (m *metrics) writeText(w io.Writer, certIssuance int64) {
	fmt.Fprintln(w, "# HELP beam_active_sessions Number of currently connected client sessions.")
	fmt.Fprintln(w, "# TYPE beam_active_sessions gauge")
	fmt.Fprintf(w, "beam_active_sessions %d\n", m.activeSessions.Load())

	fmt.Fprintln(w, "# HELP beam_active_tunnels Number of currently registered tunnels.")
	fmt.Fprintln(w, "# TYPE beam_active_tunnels gauge")
	fmt.Fprintf(w, "beam_active_tunnels %d\n", m.activeTunnels.Load())

	fmt.Fprintln(w, "# HELP beam_transport_listener_up Whether each tunnel transport listener is ready.")
	fmt.Fprintln(w, "# TYPE beam_transport_listener_up gauge")
	for ti, transport := range transportLabels {
		fmt.Fprintf(w, "beam_transport_listener_up{transport=%q} %d\n", transport, m.listenerUp[ti].Load())
	}

	fmt.Fprintln(w, "# HELP beam_transport_sessions_active Active tunnel sessions by transport and authentication state.")
	fmt.Fprintln(w, "# TYPE beam_transport_sessions_active gauge")
	for ti, transport := range transportLabels {
		for si, state := range sessionStateLabels {
			fmt.Fprintf(w, "beam_transport_sessions_active{transport=%q,state=%q} %d\n",
				transport, state, m.sessionActive[ti][si].Load())
		}
	}

	fmt.Fprintln(w, "# HELP beam_transport_sessions_total Authenticated tunnel sessions opened.")
	fmt.Fprintln(w, "# TYPE beam_transport_sessions_total counter")
	for ti, transport := range transportLabels {
		fmt.Fprintf(w, "beam_transport_sessions_total{transport=%q} %d\n", transport, m.sessionTotal[ti].Load())
	}

	fmt.Fprintln(w, "# HELP beam_transport_streams_active Leased data streams.")
	fmt.Fprintln(w, "# TYPE beam_transport_streams_active gauge")
	for ti, transport := range transportLabels {
		fmt.Fprintf(w, "beam_transport_streams_active{transport=%q} %d\n", transport, m.streamActive[ti].Load())
	}

	fmt.Fprintln(w, "# HELP beam_transport_handshake_errors_total Tunnel transport handshake errors.")
	fmt.Fprintln(w, "# TYPE beam_transport_handshake_errors_total counter")
	for ti, transport := range transportLabels {
		for ri, reason := range handshakeReasonLabels {
			fmt.Fprintf(w, "beam_transport_handshake_errors_total{transport=%q,reason=%q} %d\n",
				transport, reason, m.handshakeErrors[ti][ri].Load())
		}
	}

	fmt.Fprintln(w, "# HELP beam_transport_session_closes_total Tunnel session closes.")
	fmt.Fprintln(w, "# TYPE beam_transport_session_closes_total counter")
	for ti, transport := range transportLabels {
		for ri, reason := range closeReasonLabels {
			fmt.Fprintf(w, "beam_transport_session_closes_total{transport=%q,reason=%q} %d\n",
				transport, reason, m.sessionCloses[ti][ri].Load())
		}
	}

	fmt.Fprintln(w, "# HELP beam_transport_stream_open_errors_total Data stream open errors.")
	fmt.Fprintln(w, "# TYPE beam_transport_stream_open_errors_total counter")
	for ti, transport := range transportLabels {
		for ri, reason := range openReasonLabels {
			fmt.Fprintf(w, "beam_transport_stream_open_errors_total{transport=%q,reason=%q} %d\n",
				transport, reason, m.streamOpenErrors[ti][ri].Load())
		}
	}

	fmt.Fprintln(w, "# HELP beam_transport_capacity_rejections_total Immediate capacity rejections.")
	fmt.Fprintln(w, "# TYPE beam_transport_capacity_rejections_total counter")
	for si, scope := range capacityScopeLabels {
		fmt.Fprintf(w, "beam_transport_capacity_rejections_total{scope=%q} %d\n", scope, m.capacityRejected[si].Load())
	}

	fmt.Fprintln(w, "# HELP beam_transport_stream_capacity Configured stream capacity ceiling.")
	fmt.Fprintln(w, "# TYPE beam_transport_stream_capacity gauge")
	fmt.Fprintf(w, "beam_transport_stream_capacity{scope=\"session\"} %d\n", m.sessionCapacity.Load())
	fmt.Fprintf(w, "beam_transport_stream_capacity{scope=\"global\"} %d\n", m.globalCapacity.Load())

	fmt.Fprintln(w, "# HELP beam_yamux_stream_window_bytes Effective yamux receive window in bytes.")
	fmt.Fprintln(w, "# TYPE beam_yamux_stream_window_bytes gauge")
	fmt.Fprintf(w, "beam_yamux_stream_window_bytes %d\n", m.yamuxWindow.Load())

	fmt.Fprintln(w, "# HELP beam_cert_issuance_total Total certs ever issued by the cert manager.")
	fmt.Fprintln(w, "# TYPE beam_cert_issuance_total counter")
	fmt.Fprintf(w, "beam_cert_issuance_total %d\n", certIssuance)

	m.mu.Lock()
	statuses := make([]int, 0, len(m.requestsByStatus))
	for s := range m.requestsByStatus {
		statuses = append(statuses, s)
	}
	sort.Ints(statuses)
	statusCounts := make(map[int]int64, len(m.requestsByStatus))
	for _, s := range statuses {
		statusCounts[s] = m.requestsByStatus[s]
	}
	m.mu.Unlock()

	fmt.Fprintln(w, "# HELP beam_requests_total Total public requests served, by status code.")
	fmt.Fprintln(w, "# TYPE beam_requests_total counter")
	for _, s := range statuses {
		fmt.Fprintf(w, "beam_requests_total{status=\"%d\"} %d\n", s, statusCounts[s])
	}
}

// Per-slug/per-tunnel byte counters are emitted separately by the
// trafficStore (beam_bytes_in_total / beam_bytes_out_total).

// responseRecorder wraps http.ResponseWriter to capture status code
// and bytes written. Implements http.Hijacker so WebSocket upgrades
// still work — note: bytes after Hijack flow outside this wrapper
// and aren't counted (acceptable for M6; revisit if WS becomes a
// bandwidth-cost driver).
type responseRecorder struct {
	http.ResponseWriter
	status        int
	bytes         int64
	headerWritten bool
	// firstByteAt is when the first response byte was sent (TTFB). Zero until
	// the first WriteHeader/Write.
	firstByteAt time.Time
	// wrapHijack, if set, wraps the hijacked conn (the WebSocket/upgrade path)
	// so its bytes are counted and a heartbeat goroutine can emit per-window
	// events. The bool reports whether that goroutine took terminal-event
	// ownership. A shutdown-raced upgrade is closed but not tracked, so the
	// ordinary handler must still emit its one terminal event.
	wrapHijack   func(net.Conn) (net.Conn, bool)
	hijackedConn net.Conn
}

func (rr *responseRecorder) WriteHeader(status int) {
	if rr.headerWritten {
		return
	}
	if rr.firstByteAt.IsZero() {
		rr.firstByteAt = time.Now()
	}
	rr.status = status
	rr.headerWritten = true
	rr.ResponseWriter.WriteHeader(status)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.headerWritten {
		rr.WriteHeader(http.StatusOK)
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.bytes += int64(n)
	return n, err
}

func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	rr.headerWritten = true
	conn, brw, err := hj.Hijack()
	if err == nil && rr.wrapHijack != nil {
		var tracked bool
		conn, tracked = rr.wrapHijack(conn)
		if tracked {
			rr.hijackedConn = conn
		}
	}
	return conn, brw, err
}

func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
