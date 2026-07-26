// perfclient drives one measurement case against a beamd tunnel URL and emits a
// JSON summary (transport-performance-spec §15.3 / G1). One invocation = one
// (profile, transport, size, direction, concurrency) case; the netem harness
// loops it across the matrix. It measures TTFB (via httptrace), total elapsed,
// per-request and aggregate throughput, verifies payload integrity against the
// deterministic pattern, and reports p50/p95/p99/max plus (optionally) the raw
// per-iteration durations so the 1/2/4-second TCP timeout ladder is visible.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// patternByte mirrors perfserver: the deterministic payload byte at offset i.
func patternByte(i int64) byte { return byte(i % 251) }

type patternReader struct{ pos, n int64 }

func (r *patternReader) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, io.EOF
	}
	m := int64(len(p))
	if rem := r.n - r.pos; rem < m {
		m = rem
	}
	for i := int64(0); i < m; i++ {
		p[i] = patternByte(r.pos + i)
	}
	r.pos += m
	return int(m), nil
}

func expectedSum(n int64) string {
	h := sha256.New()
	_, _ = io.Copy(h, &patternReader{n: n})
	return hex.EncodeToString(h.Sum(nil))
}

type result struct {
	ttfbMs, elapsedMs float64
	bytes             int64
	ok                bool
	errMsg            string
}

func main() {
	var (
		urlBase     = flag.String("url", "", "tunnel base URL, e.g. https://blob-perf.perf.local")
		resolve     = flag.String("resolve", "", "host:ip DNS override, e.g. blob-perf.perf.local:10.0.0.1")
		insecure    = flag.Bool("insecure", false, "skip TLS verification (self-signed test edge)")
		size        = flag.Int64("size", 1<<20, "payload size in bytes")
		dir         = flag.String("dir", "download", "download|upload")
		n           = flag.Int("n", 50, "measured iterations")
		warmup      = flag.Int("warmup", 5, "warmup iterations (discarded)")
		concurrency = flag.Int("concurrency", 1, "concurrent workers")
		profile     = flag.String("profile", "", "profile label for the record")
		transport   = flag.String("transport", "tcp", "transport label")
		timeout     = flag.Duration("timeout", 300*time.Second, "per-request timeout")
		raw         = flag.Bool("raw", false, "include raw per-iteration elapsed_ms")
		progress    = flag.String("progress-file", "", "optional atomic JSON worker/error progress file")
	)
	flag.Parse()
	if *urlBase == "" {
		fmt.Fprintln(os.Stderr, "--url required")
		os.Exit(2)
	}

	// Honor --resolve so the client reaches the edge IP with the tunnel hostname
	// (SNI + Host) intact, without editing /etc/hosts.
	var ovHost, ovIP string
	if *resolve != "" {
		i := strings.LastIndex(*resolve, ":")
		ovHost, ovIP = (*resolve)[:i], (*resolve)[i+1:]
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && host == ovHost && ovHost != "" {
				addr = net.JoinHostPort(ovIP, port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec // self-signed test edge
	}
	client := &http.Client{Transport: tr, Timeout: *timeout}
	var progressActive, progressStarted, progressCompleted atomic.Int64
	var progressErrors, progressCorrupt atomic.Int64
	writeProgress := func() error {
		if *progress == "" {
			return nil
		}
		body, err := json.Marshal(map[string]any{
			"active":            progressActive.Load(),
			"started":           progressStarted.Load(),
			"completed":         progressCompleted.Load(),
			"errors":            progressErrors.Load(),
			"corrupt":           progressCorrupt.Load(),
			"updated_unix_nano": time.Now().UnixNano(),
		})
		if err != nil {
			return err
		}
		body = append(body, '\n')
		tmp := fmt.Sprintf("%s.tmp.%d", *progress, os.Getpid())
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, *progress)
	}
	progressStop := make(chan struct{})
	progressDone := make(chan struct{})
	progressErr := make(chan error, 1)
	if *progress != "" {
		if err := writeProgress(); err != nil {
			fmt.Fprintln(os.Stderr, "write progress:", err)
			os.Exit(2)
		}
		go func() {
			defer close(progressDone)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := writeProgress(); err != nil {
						select {
						case progressErr <- err:
						default:
						}
						return
					}
				case <-progressStop:
					if err := writeProgress(); err != nil {
						select {
						case progressErr <- err:
						default:
						}
					}
					return
				}
			}
		}()
	}

	expSum := ""
	if *dir == "upload" {
		expSum = expectedSum(*size)
	}

	doOne := func() result {
		res := result{bytes: *size}
		start := time.Now()
		var ttfb time.Duration
		trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb = time.Since(start) }}
		ctx := httptrace.WithClientTrace(context.Background(), trace)

		switch *dir {
		case "download":
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/download?n=%d", *urlBase, *size), nil)
			resp, err := client.Do(req)
			if err != nil {
				res.errMsg = err.Error()
				return res
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				_, _ = io.Copy(io.Discard, resp.Body)
				res.errMsg = fmt.Sprintf("status %d", resp.StatusCode)
				return res
			}
			buf := make([]byte, 128*1024)
			var off int64
			ok := true
			for {
				m, rerr := resp.Body.Read(buf)
				for i := 0; i < m && ok; i++ {
					if buf[i] != patternByte(off+int64(i)) {
						ok = false
					}
				}
				off += int64(m)
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					res.errMsg = rerr.Error()
					return res
				}
			}
			if off != *size {
				res.errMsg = fmt.Sprintf("short read %d/%d", off, *size)
				return res
			}
			res.ok = ok
			if !ok {
				res.errMsg = "corrupt payload"
			}
		case "upload":
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *urlBase+"/upload", &patternReader{n: *size})
			req.ContentLength = *size
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := client.Do(req)
			if err != nil {
				res.errMsg = err.Error()
				return res
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				res.errMsg = fmt.Sprintf("status %d", resp.StatusCode)
				return res
			}
			res.ok = strings.TrimSpace(string(body)) == expSum
			if !res.ok {
				res.errMsg = "checksum mismatch"
			}
		default:
			res.errMsg = "bad --dir"
			return res
		}
		res.elapsedMs = float64(time.Since(start)) / 1e6
		res.ttfbMs = float64(ttfb) / 1e6
		return res
	}

	runBatch := func(count int) []result {
		out := make([]result, count)
		tasks := make(chan int)
		workers := *concurrency
		if workers > count {
			workers = count
		}
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				progressActive.Add(1)
				defer progressActive.Add(-1)
				for idx := range tasks {
					progressStarted.Add(1)
					result := doOne()
					out[idx] = result
					switch result.errMsg {
					case "":
					case "corrupt payload", "checksum mismatch":
						progressCorrupt.Add(1)
					default:
						progressErrors.Add(1)
					}
					// Publish the terminal counter last so a progress snapshot
					// can never observe a failed request as completed-before-error.
					progressCompleted.Add(1)
				}
			}()
		}
		for i := 0; i < count; i++ {
			tasks <- i
		}
		close(tasks)
		wg.Wait()
		return out
	}

	if *warmup > 0 {
		_ = runBatch(*warmup)
	}
	wallStart := time.Now()
	results := runBatch(*n)
	wall := time.Since(wallStart)
	if *progress != "" {
		close(progressStop)
		<-progressDone
		select {
		case err := <-progressErr:
			fmt.Fprintln(os.Stderr, "write progress:", err)
			os.Exit(2)
		default:
		}
	}

	var elapsed, ttfbs []float64
	var okBytes int64
	errors, corrupt := 0, 0
	for _, r := range results {
		switch {
		case r.errMsg == "": // success
			elapsed = append(elapsed, r.elapsedMs)
			ttfbs = append(ttfbs, r.ttfbMs)
			okBytes += r.bytes
		case r.errMsg == "corrupt payload" || r.errMsg == "checksum mismatch":
			corrupt++
		default:
			errors++
		}
	}

	out := map[string]any{
		"profile":                  *profile,
		"transport":                *transport,
		"size":                     *size,
		"dir":                      *dir,
		"concurrency":              *concurrency,
		"iterations":               *n,
		"errors":                   errors,
		"corrupt":                  corrupt,
		"elapsed_ms":               pctiles(elapsed),
		"ttfb_ms":                  pctiles(ttfbs),
		"median_throughput_bps":    medianThroughput(elapsed, *size),
		"aggregate_throughput_bps": float64(okBytes) / wall.Seconds(),
		"wall_s":                   wall.Seconds(),
	}
	if *raw {
		// Ordered per-iteration samples (including errors/corruption), so the run
		// is auditable and the analyzer can detect an RTO ladder from raw TTFB.
		samples := make([]map[string]any, len(results))
		for i, r := range results {
			s := map[string]any{"i": i, "ttfb_ms": r.ttfbMs, "elapsed_ms": r.elapsedMs, "ok": r.ok}
			if r.errMsg != "" {
				s["err"] = r.errMsg
			}
			samples[i] = s
		}
		out["samples"] = samples
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func pctiles(v []float64) map[string]float64 {
	if len(v) == 0 {
		return map[string]float64{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	q := func(p float64) float64 {
		if len(s) == 1 {
			return s[0]
		}
		idx := p * float64(len(s)-1)
		lo, hi := int(math.Floor(idx)), int(math.Ceil(idx))
		if lo == hi {
			return s[lo]
		}
		frac := idx - float64(lo)
		return s[lo]*(1-frac) + s[hi]*frac
	}
	return map[string]float64{"p50": q(0.50), "p95": q(0.95), "p99": q(0.99), "min": s[0], "max": s[len(s)-1]}
}

// medianThroughput is per-request throughput at the median elapsed time
// (bytes/sec) — the solo-transfer figure the A2 gate compares to the 8-stream
// aggregate. Uses the same interpolated p50 as pctiles so throughput and the
// reported p50 stay consistent (matters at small iteration counts).
func medianThroughput(elapsedMs []float64, size int64) float64 {
	if len(elapsedMs) == 0 {
		return 0
	}
	med := pctiles(elapsedMs)["p50"]
	if med <= 0 {
		return 0
	}
	return float64(size) / (med / 1000.0)
}
