// beam-testapp is a small HTTP server you run locally to verify
// your beam deployment. Expose it through a tunnel and curl its
// routes to confirm the proxy is healthy end-to-end.
//
// Usage:
//
//	beam-testapp                # listen on :8765
//	beam-testapp --port 3001    # custom port
//
// Pair with `beam expose 8765 --as test` and curl the resulting URL.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	port := flag.Int("port", 8765, "listen port")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/headers", handleHeaders)
	mux.HandleFunc("/echo", handleEcho)
	mux.HandleFunc("/sleep", handleSleep)
	mux.HandleFunc("/size", handleSize)
	mux.HandleFunc("/sse", handleSSE)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("beam-testapp listening on %s", addr)
	log.Printf("expose with:  beam expose %d --as test", *port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	host, _ := os.Hostname()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, `beam-testapp
===============

backend hostname:      %s
backend Go:            %s
request time:          %s

your request:
  method:              %s
  path:                %s
  Host header:         %s
  X-Forwarded-For:     %s
  X-Forwarded-Proto:   %s
  X-Forwarded-Host:    %s

useful routes (curl any of these through your tunnel URL):

  GET  /              this page
  GET  /headers       JSON dump of every request header (verify forwarding)
  GET  /sleep?ms=N    sleep N ms before responding (default 500)
  GET  /size?bytes=N  return N bytes of 'x' (default 1024)
  GET  /sse           Server-Sent Events stream, 5 ticks 500ms apart
  POST /echo          echo the request body back
`,
		host,
		runtime.Version(),
		time.Now().UTC().Format(time.RFC3339),
		r.Method, r.URL.Path, r.Host,
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Forwarded-Proto"),
		r.Header.Get("X-Forwarded-Host"),
	)
}

func handleHeaders(w http.ResponseWriter, r *http.Request) {
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	headers := make(map[string]string, len(keys))
	for _, k := range keys {
		headers[k] = strings.Join(r.Header[k], ", ")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"method":  r.Method,
		"host":    r.Host,
		"path":    r.URL.Path,
		"headers": headers,
	})
}

func handleEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, r.Body)
}

func handleSleep(w http.ResponseWriter, r *http.Request) {
	ms := 500
	if v := r.URL.Query().Get("ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ms = n
		}
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	fmt.Fprintf(w, "slept %d ms\n", ms)
}

func handleSize(w http.ResponseWriter, r *http.Request) {
	n := 1024
	if v := r.URL.Query().Get("bytes"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x >= 0 && x <= 100*1024*1024 {
			n = x
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(n))
	buf := make([]byte, 4096)
	for i := range buf {
		buf[i] = 'x'
	}
	remaining := n
	for remaining > 0 {
		chunk := remaining
		if chunk > len(buf) {
			chunk = len(buf)
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return
		}
		remaining -= chunk
	}
}

func handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by ResponseWriter", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for i := 0; i < 5; i++ {
		if _, err := fmt.Fprintf(w, "data: tick %d at %s\n\n", i, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
