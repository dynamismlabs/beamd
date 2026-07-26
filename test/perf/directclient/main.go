// directclient measures the protocol-matched baseline used by the B4
// qualification harness. Handshake time is recorded once but excluded from all
// transfer samples. Warmups and measurements reuse the same TLS/TCP or QUIC
// connection. The agent-side client opens one long-lived control stream; the
// edge-side server opens every QUIC data stream, matching production roles.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"time"

	quic "github.com/quic-go/quic-go"
)

const (
	alpn      = "beamd-perf-direct/1"
	planMagic = "B4P1"
	planSize  = 17
)

var errCorrupt = errors.New("payload checksum mismatch")

type patternReader struct {
	pos int64
	n   int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, io.EOF
	}
	n := int64(len(p))
	if remaining := r.n - r.pos; remaining < n {
		n = remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = byte((r.pos + i) % 251)
	}
	r.pos += n
	return int(n), nil
}

type sample struct {
	Index     int     `json:"i"`
	TTFBMs    float64 `json:"ttfb_ms"`
	ElapsedMs float64 `json:"elapsed_ms"`
	OK        bool    `json:"ok"`
	Corrupt   bool    `json:"corrupt,omitempty"`
	Error     string  `json:"err,omitempty"`
}

type stream interface {
	io.Reader
	io.Writer
	Close() error
	SetDeadline(time.Time) error
}

type directConn interface {
	Configure(context.Context, string, int64, int) error
	Next(context.Context) (stream, error)
	Close() error
}

type tcpDirectConn struct {
	conn *tls.Conn
}

type borrowedStream struct {
	*tls.Conn
}

func (s borrowedStream) Close() error { return nil }

func (c *tcpDirectConn) Next(context.Context) (stream, error) {
	return borrowedStream{c.conn}, nil
}

func (c *tcpDirectConn) Configure(
	ctx context.Context,
	direction string,
	size int64,
	operations int,
) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetDeadline(deadline); err != nil {
			return err
		}
		defer c.conn.SetDeadline(time.Time{})
	}
	return sendPlan(c.conn, direction, size, operations)
}

func (c *tcpDirectConn) Close() error { return c.conn.Close() }

type quicDirectConn struct {
	conn    *quic.Conn
	control *quic.Stream
}

func (c *quicDirectConn) Next(ctx context.Context) (stream, error) {
	return c.conn.AcceptStream(ctx)
}

func (c *quicDirectConn) Configure(
	ctx context.Context,
	direction string,
	size int64,
	operations int,
) error {
	control, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := control.SetDeadline(deadline); err != nil {
			return err
		}
		defer control.SetDeadline(time.Time{})
	}
	if err := sendPlan(control, direction, size, operations); err != nil {
		_ = control.Close()
		return err
	}
	c.control = control
	return nil
}

func (c *quicDirectConn) Close() error {
	if c.control != nil {
		_ = c.control.Close()
	}
	return c.conn.CloseWithError(0, "")
}

func sendPlan(rw io.ReadWriter, direction string, size int64, operations int) error {
	if operations < 1 || uint64(operations) > uint64(^uint32(0)) {
		return fmt.Errorf("invalid operation count %d", operations)
	}
	var plan [planSize]byte
	copy(plan[:4], planMagic)
	if direction == "download" {
		plan[4] = 'D'
	} else {
		plan[4] = 'U'
	}
	binary.BigEndian.PutUint64(plan[5:13], uint64(size))
	binary.BigEndian.PutUint32(plan[13:17], uint32(operations))
	if _, err := rw.Write(plan[:]); err != nil {
		return err
	}
	var ack [1]byte
	if _, err := io.ReadFull(rw, ack[:]); err != nil {
		return err
	}
	if ack[0] != 1 {
		return errors.New("direct fixture rejected measurement plan")
	}
	return nil
}

func quicConfig() *quic.Config {
	return &quic.Config{
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 16 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
		MaxIncomingStreams:             64,
		MaxIncomingUniStreams:          -1,
		HandshakeIdleTimeout:           10 * time.Second,
		MaxIdleTimeout:                 75 * time.Second,
		KeepAlivePeriod:                0,
	}
}

func tlsConfig(caFile, serverName string, insecure bool) (*tls.Config, error) {
	var roots *x509.CertPool
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contained no certificates")
		}
	}
	return &tls.Config{
		RootCAs:            roots,
		ServerName:         serverName,
		InsecureSkipVerify: insecure, //nolint:gosec // isolated qualification fixture; matches beamd test client
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
	}, nil
}

func dial(ctx context.Context, transport, addr string, tlsConf *tls.Config) (directConn, float64, error) {
	start := time.Now()
	switch transport {
	case "tcp":
		raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, 0, err
		}
		if tcp, ok := raw.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		conn := tls.Client(raw, tlsConf)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, 0, err
		}
		return &tcpDirectConn{conn: conn}, float64(time.Since(start)) / 1e6, nil
	case "quic":
		conn, err := quic.DialAddr(ctx, addr, tlsConf, quicConfig())
		if err != nil {
			return nil, 0, err
		}
		return &quicDirectConn{conn: conn}, float64(time.Since(start)) / 1e6, nil
	default:
		return nil, 0, fmt.Errorf("unknown transport %q", transport)
	}
}

func verifyPattern(r io.Reader, n int64, firstByte func()) error {
	buf := make([]byte, 128*1024)
	var offset int64
	for offset < n {
		want := int64(len(buf))
		if remaining := n - offset; remaining < want {
			want = remaining
		}
		got, err := io.ReadFull(r, buf[:want])
		if got > 0 && offset == 0 {
			firstByte()
		}
		if err != nil {
			return err
		}
		for i := 0; i < got; i++ {
			if buf[i] != byte((offset+int64(i))%251) {
				return fmt.Errorf("%w at offset %d", errCorrupt, offset+int64(i))
			}
		}
		offset += int64(got)
	}
	return nil
}

func runOne(
	parent context.Context,
	timeout time.Duration,
	conn directConn,
	index int,
	direction string,
	size int64,
) sample {
	out := sample{Index: index}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	rw, err := conn.Next(ctx)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer rw.Close()
	if err := rw.SetDeadline(time.Now().Add(timeout)); err != nil {
		out.Error = err.Error()
		return out
	}
	defer rw.SetDeadline(time.Time{})

	start := time.Now()
	var header [9]byte
	if _, err := io.ReadFull(rw, header[:]); err != nil {
		out.Error = err.Error()
		return out
	}
	expectedOperation := byte('U')
	if direction == "download" {
		expectedOperation = 'D'
	}
	if header[0] != expectedOperation ||
		binary.BigEndian.Uint64(header[1:]) != uint64(size) {
		out.Error = "server sent a mismatched measurement operation"
		return out
	}

	switch direction {
	case "download":
		if _, err := io.CopyN(rw, &patternReader{n: size}, size); err != nil {
			out.Error = err.Error()
			return out
		}
		if quicStream, ok := rw.(*quic.Stream); ok {
			if err := quicStream.Close(); err != nil {
				out.Error = err.Error()
				return out
			}
		}
		var status [1]byte
		if _, err := io.ReadFull(rw, status[:]); err != nil {
			out.Error = err.Error()
			return out
		}
		out.TTFBMs = float64(time.Since(start)) / 1e6
		if status[0] != 1 {
			out.Error = "server rejected download checksum"
			out.Corrupt = true
			return out
		}
	case "upload":
		if err := verifyPattern(rw, size, func() {
			if out.TTFBMs == 0 {
				out.TTFBMs = float64(time.Since(start)) / 1e6
			}
		}); err != nil {
			out.Error = err.Error()
			out.Corrupt = errors.Is(err, errCorrupt)
			return out
		}
		if _, err := rw.Write([]byte{1}); err != nil {
			out.Error = err.Error()
			return out
		}
		if quicStream, ok := rw.(*quic.Stream); ok {
			if err := quicStream.Close(); err != nil {
				out.Error = err.Error()
				return out
			}
		}
	default:
		out.Error = "direction must be download or upload"
		return out
	}
	out.ElapsedMs = float64(time.Since(start)) / 1e6
	out.OK = true
	if quicStream, ok := rw.(*quic.Stream); ok {
		// Cleanup is outside the transfer sample but completes before the next
		// request, preserving one warmed connection without leaking stream
		// credit. The server closes its send side after consuming our FIN.
		if _, err := io.Copy(io.Discard, quicStream); err != nil {
			out.OK = false
			out.Error = err.Error()
		}
	}
	return out
}

func stats(values []float64) map[string]float64 {
	if len(values) == 0 {
		return map[string]float64{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	at := func(p float64) float64 {
		position := p * float64(len(sorted)-1)
		low, high := int(math.Floor(position)), int(math.Ceil(position))
		if low == high {
			return sorted[low]
		}
		fraction := position - float64(low)
		return sorted[low]*(1-fraction) + sorted[high]*fraction
	}
	return map[string]float64{
		"min": sorted[0], "p50": at(.50), "p95": at(.95),
		"p99": at(.99), "max": sorted[len(sorted)-1],
	}
}

func main() {
	transport := flag.String("transport", "", "tcp or quic")
	addr := flag.String("addr", "", "fixture address")
	serverName := flag.String("server-name", "direct.perf.local", "TLS server name")
	caFile := flag.String("ca", "", "trusted fixture certificate")
	insecure := flag.Bool("insecure", false, "disable certificate verification for trust-equivalent fixture runs")
	direction := flag.String("dir", "download", "tunnel direction: download or upload")
	size := flag.Int64("size", 1<<20, "payload bytes")
	iterations := flag.Int("n", 50, "measured iterations")
	warmups := flag.Int("warmup", 5, "unmeasured warmups")
	profile := flag.String("profile", "", "impairment profile label")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-operation timeout")
	flag.Parse()

	if (*transport != "tcp" && *transport != "quic") ||
		*addr == "" || (*caFile == "" && !*insecure) ||
		(*direction != "download" && *direction != "upload") ||
		*size < 0 || *iterations < 1 || *warmups < 0 {
		fmt.Fprintln(os.Stderr, "invalid arguments; transport, addr, ca (unless insecure), direction, size, n, and warmup are required")
		os.Exit(2)
	}
	tlsConf, err := tlsConfig(*caFile, *serverName, *insecure)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	conn, handshakeMs, err := dial(dialCtx, *transport, *addr, tlsConf)
	dialCancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer conn.Close()
	configureCtx, configureCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := conn.Configure(
		configureCtx,
		*direction,
		*size,
		*warmups+*iterations,
	); err != nil {
		configureCancel()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	configureCancel()

	for i := 0; i < *warmups; i++ {
		if result := runOne(ctx, *timeout, conn, i, *direction, *size); !result.OK {
			fmt.Fprintf(os.Stderr, "warmup %d failed: %s\n", i, result.Error)
			os.Exit(2)
		}
	}

	samples := make([]sample, *iterations)
	elapsed := make([]float64, 0, *iterations)
	ttfb := make([]float64, 0, *iterations)
	errorsCount, corrupt := 0, 0
	start := time.Now()
	for i := range samples {
		samples[i] = runOne(ctx, *timeout, conn, i, *direction, *size)
		if samples[i].OK {
			elapsed = append(elapsed, samples[i].ElapsedMs)
			ttfb = append(ttfb, samples[i].TTFBMs)
		} else if samples[i].Corrupt {
			corrupt++
		} else {
			errorsCount++
		}
	}
	wall := time.Since(start).Seconds()
	medianThroughput := 0.0
	if p50 := stats(elapsed)["p50"]; p50 > 0 {
		medianThroughput = float64(*size) / (p50 / 1000)
	}
	successes := len(elapsed)
	wireDirection := "edge-to-agent"
	if *direction == "download" {
		wireDirection = "agent-to-edge"
	}
	record := map[string]any{
		"fixture":                  "direct",
		"profile":                  *profile,
		"transport":                "direct-" + *transport,
		"size":                     *size,
		"dir":                      *direction,
		"wire_direction":           wireDirection,
		"data_stream_initiator":    "edge",
		"concurrency":              1,
		"iterations":               *iterations,
		"errors":                   errorsCount,
		"corrupt":                  corrupt,
		"elapsed_ms":               stats(elapsed),
		"ttfb_ms":                  stats(ttfb),
		"median_throughput_bps":    medianThroughput,
		"aggregate_throughput_bps": float64(successes) * float64(*size) / wall,
		"wall_s":                   wall,
		"handshake_ms":             handshakeMs,
		"handshake_included":       false,
		"samples":                  samples,
	}
	if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
