package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/dynamismlabs/beamd/internal/tunnel"
)

// TrafficRecorder receives per-tunnel byte deltas as proxied connections
// close. The edge is agnostic to where the bytes go: the self-hosted
// build records into an in-memory + persisted store (exposed at
// /metrics and via the usage webhook); a hosted deployment can register
// an additional recorder (via Edge.AddTrafficSink) that maps slug → tenant
// account and persists durably for billing. The contract is intentionally
// account-free — the proxy only knows the slug; mapping slug → account is
// the hosted recorder's job.
//
//	bytesIn  = bytes received from visitors (requests/uploads)
//	bytesOut = bytes sent to visitors (responses/downloads)
//
// Counted at the tunnel stream, so WebSocket/HMR traffic is included.
type TrafficRecorder interface {
	RecordTraffic(slug, name string, bytesIn, bytesOut int64)
}

// trafficKey identifies one tunnel for accounting: a developer slug plus
// the app/tunnel name under it.
type trafficKey struct {
	Slug string
	Name string
}

type trafficCounts struct {
	In  int64
	Out int64
}

// trafficStore is the self-hosted TrafficRecorder: in-memory aggregation
// keyed by (slug, name), optionally persisted to a JSON file so totals
// survive restarts. A blank path disables persistence (used in tests and
// when no data_dir is configured) and the store is purely in-memory.
type trafficStore struct {
	path string

	mu sync.Mutex
	m  map[trafficKey]trafficCounts
}

func newTrafficStore(path string) *trafficStore {
	s := &trafficStore{path: path, m: make(map[trafficKey]trafficCounts)}
	s.load()
	return s
}

func (s *trafficStore) RecordTraffic(slug, name string, bytesIn, bytesOut int64) {
	if slug == "" {
		return
	}
	k := trafficKey{Slug: slug, Name: name}
	s.mu.Lock()
	c := s.m[k]
	c.In += bytesIn
	c.Out += bytesOut
	s.m[k] = c
	s.mu.Unlock()
}

// bytesOutBySlug rolls egress up to the slug level — the "bytes proxied"
// figure the usage webhook reports (egress is the billable dimension).
func (s *trafficStore) bytesOutBySlug() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64)
	for k, c := range s.m {
		out[k.Slug] += c.Out
	}
	return out
}

// writeMetrics emits the per-(slug,name) byte counters in Prometheus text
// format. Summing by the `slug` label in a query yields per-developer
// totals; the raw lines give per-app detail.
func (s *trafficStore) writeMetrics(w io.Writer) {
	s.mu.Lock()
	keys := make([]trafficKey, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	counts := make(map[trafficKey]trafficCounts, len(s.m))
	for k, c := range s.m {
		counts[k] = c
	}
	s.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Slug != keys[j].Slug {
			return keys[i].Slug < keys[j].Slug
		}
		return keys[i].Name < keys[j].Name
	})

	fmt.Fprintln(w, "# HELP beam_bytes_in_total Request bytes received from visitors, by slug and tunnel.")
	fmt.Fprintln(w, "# TYPE beam_bytes_in_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "beam_bytes_in_total{slug=%q,name=%q} %d\n", k.Slug, k.Name, counts[k].In)
	}

	fmt.Fprintln(w, "# HELP beam_bytes_out_total Response bytes sent to visitors, by slug and tunnel.")
	fmt.Fprintln(w, "# TYPE beam_bytes_out_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "beam_bytes_out_total{slug=%q,name=%q} %d\n", k.Slug, k.Name, counts[k].Out)
	}
}

// trafficRecord is the on-disk form of one (slug, name) entry.
type trafficRecord struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	BytesIn  int64  `json:"bytesIn"`
	BytesOut int64  `json:"bytesOut"`
}

func (s *trafficStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return // missing/unreadable → start empty
	}
	var recs []trafficRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return
	}
	s.mu.Lock()
	for _, r := range recs {
		s.m[trafficKey{Slug: r.Slug, Name: r.Name}] = trafficCounts{In: r.BytesIn, Out: r.BytesOut}
	}
	s.mu.Unlock()
}

// Flush writes the current totals to disk (atomic tmp+rename). No-op when
// persistence is disabled (blank path).
func (s *trafficStore) Flush() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	recs := make([]trafficRecord, 0, len(s.m))
	for k, c := range s.m {
		recs = append(recs, trafficRecord{Slug: k.Slug, Name: k.Name, BytesIn: c.In, BytesOut: c.Out})
	}
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// countingConn wraps the per-request tunnel stream to tally bytes in both
// directions. Writes carry the request to the backend (ingress); reads
// carry the response back (egress). The totals are reported once, when
// the connection closes — which for a WebSocket is at the end of the
// upgraded session, so streamed/HMR bytes are included.
type countingConn struct {
	net.Conn
	onClose func(bytesIn, bytesOut int64)

	in   atomic.Int64
	out  atomic.Int64
	once sync.Once
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.out.Add(int64(n))
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.in.Add(int64(n))
	}
	return n, err
}

func (c *countingConn) Close() error {
	err := c.Conn.Close()
	c.finalize()
	return err
}

func (c *countingConn) finalize() {
	c.once.Do(func() { c.onClose(c.in.Load(), c.out.Load()) })
}

// leasedConn keeps an edge admission lease until the transport adapter says
// both stream directions are terminal. In particular, net.Conn.Close is not
// enough for yamux: it starts a local half-close and the receive side can stay
// alive until remote EOF or the adapter's close timeout.
type leasedConn struct {
	*countingConn
	stream tunnel.Stream
	ctx    context.Context

	normalClose chan struct{}
	normalOnce  sync.Once
}

func newLeasedConn(
	ctx context.Context,
	stream tunnel.Stream,
	onTraffic func(bytesIn, bytesOut int64),
	onDone func(),
) *leasedConn {
	c := &leasedConn{
		countingConn: &countingConn{Conn: stream, onClose: onTraffic},
		stream:       stream,
		ctx:          ctx,
		normalClose:  make(chan struct{}),
	}
	go c.watch(onDone)
	return c
}

func (c *leasedConn) watch(onDone func()) {
	select {
	case <-c.ctx.Done():
		c.stream.Abort(tunnel.StreamCanceled)
	case <-c.normalClose:
		// The HTTP transport completed normally and has initiated graceful
		// stream cleanup. Cancellation can still race that close, so keep
		// watching until both stream directions are terminal.
		select {
		case <-c.ctx.Done():
			c.stream.Abort(tunnel.StreamCanceled)
		case <-c.stream.Done():
		}
	case <-c.stream.Done():
	}

	<-c.stream.Done()
	c.countingConn.finalize()
	onDone()
}

func (c *leasedConn) Read(p []byte) (int, error) {
	n, err := c.countingConn.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.stream.Abort(tunnel.StreamCanceled)
	}
	return n, err
}

func (c *leasedConn) Write(p []byte) (int, error) {
	n, err := c.countingConn.Write(p)
	if err != nil {
		c.stream.Abort(tunnel.StreamCanceled)
	}
	return n, err
}

func (c *leasedConn) Close() error {
	if c.ctx.Err() != nil {
		c.stream.Abort(tunnel.StreamCanceled)
	} else {
		c.normalOnce.Do(func() { close(c.normalClose) })
	}
	err := c.countingConn.Close()
	if err != nil {
		c.stream.Abort(tunnel.StreamCanceled)
	}
	return err
}

var _ net.Conn = (*leasedConn)(nil)
