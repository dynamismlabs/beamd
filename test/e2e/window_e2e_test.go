package e2e

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/config"
)

// windowPayload returns n deterministic bytes so both sides can checksum-compare.
func windowPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

// startSizedBackend serves GET /download?n=<bytes> (n deterministic bytes) and
// POST /upload (replies with the hex SHA-256 of the request body). Returns its port.
func startSizedBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			n, _ := strconv.Atoi(r.URL.Query().Get("n"))
			_, _ = w.Write(windowPayload(n))
		case "/upload":
			sum := sha256.New()
			if _, err := io.Copy(sum, r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, "%x", sum.Sum(nil))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// TestPartA_AsymmetricWindows_ChecksumUpDownload proves the tuned window works
// end to end: the edge and agent use DIFFERENT receive windows (each receiver
// sets its own), and a payload well above the old 256 KiB cap round-trips
// intact in BOTH directions. The size exceeds both windows so each direction
// crosses several flow-control grant cycles.
func TestPartA_AsymmetricWindows_ChecksumUpDownload(t *testing.T) {
	const (
		edgeWindow  = 512 << 10 // 512 KiB receive window on the edge (governs downloads)
		agentWindow = 768 << 10 // 768 KiB receive window on the agent (governs uploads)
		size        = 1_500_000 // ~1.43 MiB — above 256 KiB and above both windows
	)
	port := startSizedBackend(t)

	_, edgeAddr := startEdgeCfg(t, map[string]string{"T1": "turing"}, func(cfg *config.Server) {
		cfg.YamuxStreamWindowBytes = edgeWindow
	})
	c := connectClientWithOpts(t, edgeAddr, "T1", client.Options{
		HeartbeatInterval:      200 * time.Millisecond,
		RegisterTimeout:        2 * time.Second,
		YamuxStreamWindowBytes: agentWindow,
	})
	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "api.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	want := windowPayload(size)
	wantSum := fmt.Sprintf("%x", sha256.Sum256(want))

	// Download: app → agent → edge → visitor (exercises the edge's receive window).
	resp, err := hc.Get(fmt.Sprintf("https://%s/download?n=%d", host, size))
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	if len(got) != size {
		t.Fatalf("download size = %d, want %d", len(got), size)
	}
	if gotSum := fmt.Sprintf("%x", sha256.Sum256(got)); gotSum != wantSum {
		t.Errorf("download checksum mismatch (size %d, edge window %d)", size, edgeWindow)
	}

	// Upload: visitor → edge → agent → app (exercises the agent's receive window).
	// The backend echoes the SHA-256 of exactly what it received.
	resp2, err := hc.Post("https://"+host+"/upload", "application/octet-stream", bytes.NewReader(want))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	body, err := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if err != nil {
		t.Fatalf("read upload reply: %v", err)
	}
	if string(body) != wantSum {
		t.Errorf("upload checksum = %s, want %s (size %d, agent window %d)", string(body), wantSum, size, agentWindow)
	}
}
