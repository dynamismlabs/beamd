// perfserver is the deterministic payload backend for the transport
// measurement harness (transport-performance-spec §15.3 / G1). It serves a
// reproducible byte stream so the client can verify integrity without holding a
// golden copy, and echoes the SHA-256 of uploads.
//
//	GET  /download?n=<bytes>  -> n deterministic bytes (Content-Length set)
//	POST /upload              -> hex SHA-256 of the request body
//	GET  /healthz             -> "ok"
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
)

// patternByte is the deterministic payload byte at absolute offset i. Both the
// server (download) and client (verify) generate it, so no golden file is
// needed. 251 is prime, so the pattern doesn't align to power-of-two buffers.
func patternByte(i int64) byte { return byte(i % 251) }

// patternReader streams n deterministic bytes from offset 0 without allocating
// the whole payload (100 MiB cases stay memory-light).
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

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })

	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyBuffer(w, &patternReader{n: n}, make([]byte, 128*1024))
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		if _, err := io.Copy(h, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, hex.EncodeToString(h.Sum(nil)))
	})

	log.Printf("perfserver listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}
