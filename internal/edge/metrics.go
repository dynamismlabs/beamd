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

	mu sync.Mutex
	// requestsByStatus[status] = total seen
	requestsByStatus map[int]int64
}

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

// writeText emits the Prometheus text exposition format. certIssuance
// is passed in because it lives on certs.Manager.
func (m *metrics) writeText(w io.Writer, certIssuance int64) {
	fmt.Fprintln(w, "# HELP beam_active_sessions Number of currently connected client sessions.")
	fmt.Fprintln(w, "# TYPE beam_active_sessions gauge")
	fmt.Fprintf(w, "beam_active_sessions %d\n", m.activeSessions.Load())

	fmt.Fprintln(w, "# HELP beam_active_tunnels Number of currently registered tunnels.")
	fmt.Fprintln(w, "# TYPE beam_active_tunnels gauge")
	fmt.Fprintf(w, "beam_active_tunnels %d\n", m.activeTunnels.Load())

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
}

func (rr *responseRecorder) WriteHeader(status int) {
	if rr.headerWritten {
		return
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
	return hj.Hijack()
}

func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
