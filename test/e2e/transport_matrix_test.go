package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

const fullE2EEnv = "BEAMD_E2E_FULL"

// deterministicPayloadReader emits a reproducible byte sequence without
// retaining large qualification payloads in memory.
type deterministicPayloadReader struct {
	offset int64
	left   int64
}

func newDeterministicPayloadReader(size int64) *deterministicPayloadReader {
	return &deterministicPayloadReader{left: size}
}

func (r *deterministicPayloadReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	for i := range p {
		p[i] = byte(((r.offset+int64(i))*31 + 7) % 251)
	}
	r.offset += int64(len(p))
	r.left -= int64(len(p))
	return len(p), nil
}

func deterministicPayloadSHA256(t *testing.T, size int64) string {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, newDeterministicPayloadReader(size)); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func transportMatrixSizes() []int64 {
	sizes := []int64{253 << 10, 257 << 10, 1 << 20, 16 << 20}
	if os.Getenv(fullE2EEnv) == "1" {
		sizes = append(sizes, 100<<20)
	}
	return sizes
}

func assertSelectedE2ETransport(t *testing.T, c *client.Client) {
	t.Helper()
	want := tunnel.KindYamux
	if e2eTransport(t) == "quic" {
		want = tunnel.KindQUIC
	}
	if got := c.Transport(); got != want {
		t.Fatalf("selected transport = %q, want forced %q", got, want)
	}
}

// TestTransportMatrix_PayloadChecksums covers both sides of the data path at
// every ordinary matrix size. Set BEAMD_E2E_FULL=1 to add the 100 MiB case
// used by functional qualification.
func TestTransportMatrix_PayloadChecksums(t *testing.T) {
	port := startSizedBackend(t)
	_, edgeAddr := startEdgeCfg(t, map[string]string{"T1": "turing"}, func(cfg *config.Server) {
		cfg.MaxRequestBodyBytes = 128 << 20
	})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("matrix", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "matrix.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	hc.Timeout = 90 * time.Second

	for _, size := range transportMatrixSizes() {
		t.Run(fmt.Sprintf("%d-bytes", size), func(t *testing.T) {
			wantSum := deterministicPayloadSHA256(t, size)

			resp, err := hc.Get(fmt.Sprintf("https://%s/download?n=%d", host, size))
			if err != nil {
				t.Fatalf("download: %v", err)
			}
			h := sha256.New()
			n, readErr := io.Copy(h, resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("download body: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close download body: %v", closeErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("download status = %d", resp.StatusCode)
			}
			if n != size {
				t.Fatalf("download bytes = %d, want %d", n, size)
			}
			if got := fmt.Sprintf("%x", h.Sum(nil)); got != wantSum {
				t.Fatalf("download checksum = %s, want %s", got, wantSum)
			}

			req, err := http.NewRequest(
				http.MethodPost,
				"https://"+host+"/upload",
				io.NopCloser(newDeterministicPayloadReader(size)),
			)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = size
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err = hc.Do(req)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			gotSum, readErr := io.ReadAll(resp.Body)
			closeErr = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("upload response: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close upload response: %v", closeErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("upload status = %d, body = %q", resp.StatusCode, gotSum)
			}
			if string(gotSum) != wantSum {
				t.Fatalf("upload checksum = %s, want %s", gotSum, wantSum)
			}
		})
	}
}

func TestTransportMatrix_ProductionDefaultBodyCapRejects100MiB(t *testing.T) {
	if os.Getenv(fullE2EEnv) != "1" {
		t.Skip("set BEAMD_E2E_FULL=1 for the 100 MiB qualification case")
	}
	port := startSizedBackend(t)
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("bodycap", port); err != nil {
		t.Fatal(err)
	}
	host := "bodycap.turing." + testBaseDomain
	const size = int64(100 << 20)
	req, err := http.NewRequest(
		http.MethodPost,
		"https://"+host+"/upload",
		io.NopCloser(newDeterministicPayloadReader(size)),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	resp, err := publicHTTPSClient(edgeAddr, host).Do(req)
	if err != nil {
		t.Fatalf("100 MiB upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413; body = %q", resp.StatusCode, body)
	}
}

func startFlushingBackend(t *testing.T) (port int, releases map[string]chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	releases = map[string]chan struct{}{
		"/stream": make(chan struct{}),
		"/sse":    make(chan struct{}),
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := releases[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/sse" {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "flushing unsupported", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "first\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
		flusher.Flush()
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		for _, release := range releases {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		_ = srv.Close()
	})
	return ln.Addr().(*net.TCPAddr).Port, releases
}

// TestTransportMatrix_Streaming verifies that headers and the first chunk are
// delivered before the backend completes, for ordinary chunked responses and
// SSE. A buffered-until-EOF implementation deadlocks this test until timeout.
func TestTransportMatrix_Streaming(t *testing.T) {
	port, releases := startFlushingBackend(t)
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("stream", port); err != nil {
		t.Fatal(err)
	}
	host := "stream.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	hc.Timeout = 5 * time.Second

	for _, path := range []string{"/stream", "/sse"} {
		t.Run(path[1:], func(t *testing.T) {
			respCh := make(chan *http.Response, 1)
			errCh := make(chan error, 1)
			go func() {
				resp, err := hc.Get("https://" + host + path)
				if err != nil {
					errCh <- err
					return
				}
				respCh <- resp
			}()

			var resp *http.Response
			select {
			case err := <-errCh:
				t.Fatalf("GET: %v", err)
			case resp = <-respCh:
			case <-time.After(2 * time.Second):
				t.Fatal("response headers were buffered until backend completion")
			}
			defer resp.Body.Close()
			lineCh := make(chan string, 1)
			readErrCh := make(chan error, 1)
			br := bufio.NewReader(resp.Body)
			go func() {
				line, err := br.ReadString('\n')
				if err != nil {
					readErrCh <- err
					return
				}
				lineCh <- line
			}()
			select {
			case err := <-readErrCh:
				t.Fatalf("first chunk: %v", err)
			case line := <-lineCh:
				if line != "first\n" {
					t.Fatalf("first chunk = %q", line)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first response chunk was buffered until backend completion")
			}

			close(releases[path])
			rest, err := io.ReadAll(br)
			if err != nil {
				t.Fatalf("remaining response: %v", err)
			}
			if string(rest) != "second\n" {
				t.Fatalf("remaining response = %q", rest)
			}
		})
	}
}

func startHoldingBackend(t *testing.T, want int) (port int, allStarted <-chan struct{}, release chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release = make(chan struct{})
	var count atomic.Int64
	var once sync.Once
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(count.Add(1)) == want {
			once.Do(func() { close(started) })
		}
		<-release
		_, _ = io.WriteString(w, "ok\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = srv.Close()
	})
	return ln.Addr().(*net.TCPAddr).Port, started, release
}

func startControlledHoldingBackend(t *testing.T, want int) (
	port int,
	allStarted <-chan struct{},
	release chan struct{},
	closeRelease func(),
) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release = make(chan struct{})
	var count atomic.Int64
	var once sync.Once
	var releaseOnce sync.Once
	closeRelease = func() { releaseOnce.Do(func() { close(release) }) }
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(count.Add(1)) == want {
			once.Do(func() { close(started) })
		}
		<-release
		_, _ = io.WriteString(w, "released\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		closeRelease()
		_ = srv.Close()
	})
	return ln.Addr().(*net.TCPAddr).Port, started, release, closeRelease
}

func launchHeldRequests(hc *http.Client, url string, count int) <-chan error {
	results := make(chan error, count)
	for range count {
		go func() {
			resp, err := hc.Get(url)
			if err != nil {
				results <- err
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			switch {
			case readErr != nil:
				results <- readErr
			case closeErr != nil:
				results <- closeErr
			case resp.StatusCode != http.StatusOK:
				results <- fmt.Errorf("held request status = %d", resp.StatusCode)
			case string(body) != "released\n":
				results <- fmt.Errorf("held request body = %q", body)
			default:
				results <- nil
			}
		}()
	}
	return results
}

func expectImmediateCapacityResponse(t *testing.T, hc *http.Client, url string) {
	t.Helper()
	started := time.Now()
	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("capacity probe: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("capacity response: %v", readErr)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("capacity response took %v, want <=250ms", elapsed)
	}
	if resp.StatusCode != http.StatusServiceUnavailable ||
		resp.Header.Get("Retry-After") != "1" ||
		string(body) != "{\"error\":\"tunnel capacity reached\"}\n" {
		t.Fatalf("capacity response = status %d retry-after %q body %q",
			resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

func waitForSuccessfulProbe(t *testing.T, hc *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && string(body) == "probe: GET /probe\n" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("probe did not recover after capacity was released")
}

func awaitHeldResults(t *testing.T, results <-chan error, count int) {
	t.Helper()
	for range count {
		select {
		case err := <-results:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("held request did not finish")
		}
	}
}

func waitForStreamGaugeZero(t *testing.T, edgeAddr string) {
	t.Helper()
	transport := e2eTransport(t)
	needle := fmt.Sprintf(`beam_transport_streams_active{transport=%q} 0`, transport)
	hc := publicHTTPSClient(edgeAddr, testBaseDomain)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := getMetrics(t, hc)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil && resp.StatusCode == http.StatusOK && strings.Contains(string(body), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stream gauge did not return to zero for %s", transport)
}

// TestTransportMatrix_ConcurrentRequests holds every backend request open so
// the requested concurrency is real rather than a fast sequential coincidence.
func TestTransportMatrix_ConcurrentRequests(t *testing.T) {
	for _, concurrency := range []int{1, 8, 32, 64} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			port, allStarted, release := startHoldingBackend(t, concurrency)
			_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
			c := connectClient(t, edgeAddr, "T1")
			assertSelectedE2ETransport(t, c)
			if _, err := c.Register("hold", port); err != nil {
				t.Fatal(err)
			}
			host := "hold.turing." + testBaseDomain
			hc := publicHTTPSClient(edgeAddr, host)

			errs := make(chan error, concurrency)
			for range concurrency {
				go func() {
					resp, err := hc.Get("https://" + host + "/hold")
					if err != nil {
						errs <- err
						return
					}
					body, readErr := io.ReadAll(resp.Body)
					closeErr := resp.Body.Close()
					switch {
					case readErr != nil:
						errs <- readErr
					case closeErr != nil:
						errs <- closeErr
					case resp.StatusCode != http.StatusOK:
						errs <- fmt.Errorf("status = %d", resp.StatusCode)
					case string(body) != "ok\n":
						errs <- fmt.Errorf("body = %q", body)
					default:
						errs <- nil
					}
				}()
			}

			select {
			case <-allStarted:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatalf("only a subset of %d requests reached the backend concurrently", concurrency)
			}
			close(release)
			for range concurrency {
				if err := <-errs; err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestTransportMatrix_StreamCapacityAndRecovery(t *testing.T) {
	t.Run("per-session 129th", func(t *testing.T) {
		holdPort, allStarted, release, closeRelease := startControlledHoldingBackend(t, 128)
		probePort := startDummyApp(t, "probe")
		_, edgeAddr := startEdgeCfg(t, map[string]string{"T1": "turing"}, func(cfg *config.Server) {
			cfg.MaxStreamsPerSession = 128
			cfg.MaxStreamsTotal = 256
		})
		c := connectClient(t, edgeAddr, "T1")
		assertSelectedE2ETransport(t, c)
		if _, err := c.Register("hold", holdPort); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Register("probe", probePort); err != nil {
			t.Fatal(err)
		}

		holdHost := "hold.turing." + testBaseDomain
		probeHost := "probe.turing." + testBaseDomain
		holdClient := publicHTTPSClient(edgeAddr, holdHost)
		probeClient := publicHTTPSClient(edgeAddr, probeHost)
		checkResponse(t, probeClient, "https://"+probeHost+"/probe", "probe: GET /probe\n")
		waitForStreamGaugeZero(t, edgeAddr)
		results := launchHeldRequests(holdClient, "https://"+holdHost+"/hold", 128)
		select {
		case <-allStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("128 per-session streams did not become active")
		}

		expectImmediateCapacityResponse(t, probeClient, "https://"+probeHost+"/probe")
		release <- struct{}{}
		waitForSuccessfulProbe(t, probeClient, "https://"+probeHost+"/probe")
		closeRelease()
		awaitHeldResults(t, results, 128)
		waitForStreamGaugeZero(t, edgeAddr)
	})

	t.Run("global 257th", func(t *testing.T) {
		type holder struct {
			name         string
			port         int
			allStarted   <-chan struct{}
			release      chan struct{}
			closeRelease func()
			results      <-chan error
		}
		holders := make([]holder, 4)
		for i, name := range []string{"holda", "holdb", "holdc", "holdd"} {
			port, allStarted, release, closeRelease := startControlledHoldingBackend(t, 64)
			holders[i] = holder{
				name: name, port: port, allStarted: allStarted,
				release: release, closeRelease: closeRelease,
			}
		}
		probePort := startDummyApp(t, "probe")
		_, edgeAddr := startEdgeCfg(t, map[string]string{"T1": "turing"}, func(cfg *config.Server) {
			cfg.MaxStreamsPerSession = 128
			cfg.MaxStreamsTotal = 256
		})
		for i := range holders {
			c := connectClient(t, edgeAddr, "T1")
			if _, err := c.Register(holders[i].name, holders[i].port); err != nil {
				t.Fatal(err)
			}
		}
		probeAgent := connectClient(t, edgeAddr, "T1")
		if _, err := probeAgent.Register("probe", probePort); err != nil {
			t.Fatal(err)
		}

		probeHost := "probe.turing." + testBaseDomain
		probeClient := publicHTTPSClient(edgeAddr, probeHost)
		checkResponse(t, probeClient, "https://"+probeHost+"/probe", "probe: GET /probe\n")
		waitForStreamGaugeZero(t, edgeAddr)
		// Four sessions at 64 streams each isolate the global 256-stream
		// boundary from the separately tested 128-stream session boundary.
		for i := range holders {
			host := holders[i].name + ".turing." + testBaseDomain
			holders[i].results = launchHeldRequests(
				publicHTTPSClient(edgeAddr, host), "https://"+host+"/hold", 64,
			)
			select {
			case <-holders[i].allStarted:
			case <-time.After(5 * time.Second):
				t.Fatalf("64 streams on session %s did not become active", holders[i].name)
			}
		}

		expectImmediateCapacityResponse(t, probeClient, "https://"+probeHost+"/probe")
		holders[0].release <- struct{}{}
		waitForSuccessfulProbe(t, probeClient, "https://"+probeHost+"/probe")
		for _, h := range holders {
			h.closeRelease()
			awaitHeldResults(t, h.results, 64)
		}
		waitForStreamGaugeZero(t, edgeAddr)
	})
}

func startCancellationBackend(t *testing.T) (port int, started, canceled <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	startedCh := make(chan struct{})
	canceledCh := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		startedOnce.Do(func() { close(startedCh) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceledCh) })
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, startedCh, canceledCh
}

// TestTransportMatrix_PublicCancellation proves a visitor cancellation reaches
// the backend and does not poison the session for a subsequent request.
func TestTransportMatrix_PublicCancellation(t *testing.T) {
	port, started, canceled := startCancellationBackend(t)
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("cancel", port); err != nil {
		t.Fatal(err)
	}
	host := "cancel.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		resp, doErr := hc.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- doErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached backend")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled public request unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("public cancellation did not unblock the request")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("public cancellation did not reach backend")
	}

	checkResponse(t, hc, "https://"+host+"/ok", "ok\n")
}

// TestTransportMatrix_BackendHalfClose uses a raw HTTP/1.1 backend that
// half-closes its write side after the response. The visitor must still
// receive the complete body over either tunnel adapter.
func TestTransportMatrix_BackendHalfClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					line, readErr := br.ReadString('\n')
					if readErr != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(conn,
					"HTTP/1.1 200 OK\r\nContent-Length: 10\r\nConnection: close\r\n\r\nhalf-close")
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("half", ln.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	host := "half.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/", "half-close")
}

// TestTransportMatrix_PublicHTTP2 proves the edge serves the protocol it
// advertises during public TLS ALPN negotiation. The per-connection listener
// must return the concrete *tls.Conn; wrapping it makes net/http parse the h2
// preface as HTTP/1.1 and breaks modern browser/curl clients.
func TestTransportMatrix_PublicHTTP2(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{})
	dialer := &net.Dialer{}
	hc := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // self-signed test edge
				ServerName:         testBaseDomain,
			},
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, edgeAddr)
			},
		},
		Timeout: 5 * time.Second,
	}
	t.Cleanup(hc.CloseIdleConnections)

	resp, err := hc.Get("https://" + testBaseDomain + "/healthz")
	if err != nil {
		t.Fatalf("HTTP/2 health request: %v", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("HTTP/2 health body: %v", readErr)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated protocol = %q, want HTTP/2", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK ||
		!bytes.Contains(body, []byte(`"version":"test"`)) {
		t.Fatalf("health response = status %d body %q", resp.StatusCode, body)
	}
}

// TestTransportMatrix_EarlyBackendResponse proves a response can complete
// while the public request body is still producing data. The tunnel must
// preserve the response FIN and unblock the body writer rather than hanging
// the two copy loops or resetting reliable response bytes.
func TestTransportMatrix_EarlyBackendResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backendDone := make(chan struct{})
	releaseBackend := make(chan struct{})
	var releaseOnce sync.Once
	go func() {
		defer close(backendDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, readErr := br.ReadString('\n')
			if readErr != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(conn,
			"HTTP/1.1 409 Conflict\r\nContent-Length: 6\r\nConnection: close\r\n\r\nearly\n")
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		// Do not consume the request body and do not close the socket until the
		// assertion completes. Only the agent's early-response cleanup can wake
		// the public body writer.
		<-releaseBackend
	}()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseBackend) })
		_ = ln.Close()
		select {
		case <-backendDone:
		case <-time.After(time.Second):
		}
	})

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("early", ln.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	host := "early.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	hc.Timeout = 5 * time.Second

	bodyReader, bodyWriter := io.Pipe()
	writerDone := make(chan error, 1)
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			if _, writeErr := bodyWriter.Write(chunk); writeErr != nil {
				writerDone <- writeErr
				return
			}
		}
	}()
	req, err := http.NewRequest(http.MethodPost, "https://"+host+"/early", bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		_ = bodyReader.Close()
		_ = bodyWriter.Close()
		t.Fatalf("early response request: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("early response body: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close early response body: %v", closeErr)
	}
	if resp.StatusCode != http.StatusConflict || string(body) != "early\n" {
		t.Fatalf("early response = status %d body %q", resp.StatusCode, body)
	}
	_ = bodyReader.Close()
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		_ = bodyWriter.Close()
		t.Fatal("request-body writer remained blocked after early response")
	}
	releaseOnce.Do(func() { close(releaseBackend) })
}

// TestTransportMatrix_BackendDisappearance verifies a dead local target maps
// to a prompt 502 while the tunnel session itself remains connected.
func TestTransportMatrix_BackendDisappearance(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	assertSelectedE2ETransport(t, c)
	if _, err := c.Register("gone", port); err != nil {
		t.Fatal(err)
	}
	host := "gone.turing." + testBaseDomain
	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502; body = %q", resp.StatusCode, body)
	}
}
