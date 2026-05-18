package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/treyhuffine/conduit/internal/client"
)

// Daemon wraps a long-lived *client.Client behind a loopback HTTP API
// served over a unix domain socket (PRD §10, §17). FS permissions on
// the socket enforce single-user access; no in-band auth.
type Daemon struct {
	client *client.Client
	socket string

	mu  sync.Mutex
	ln  net.Listener
	srv *http.Server

	// urls caches the public URL returned for each registered name so
	// GET /list can answer without re-querying the edge.
	urlsMu sync.Mutex
	urls   map[string]string
}

func New(c *client.Client, socketPath string) *Daemon {
	return &Daemon{
		client: c,
		socket: socketPath,
		urls:   make(map[string]string),
	}
}

// Serve binds the unix socket (0600) and serves the HTTP API. Blocks
// until Shutdown is called or the listener fails.
func (d *Daemon) Serve() error {
	if err := os.MkdirAll(filepath.Dir(d.socket), 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	_ = os.Remove(d.socket) // stale socket from previous run

	ln, err := net.Listen("unix", d.socket)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", d.socket, err)
	}
	if err := os.Chmod(d.socket, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	slog.Info("daemon listening", "socket", d.socket)

	mux := http.NewServeMux()
	mux.HandleFunc("/expose", d.handleExpose)
	mux.HandleFunc("/unexpose", d.handleUnexpose)
	mux.HandleFunc("/list", d.handleList)
	mux.HandleFunc("/healthz", d.handleHealthz)

	srv := &http.Server{Handler: mux}

	d.mu.Lock()
	d.ln = ln
	d.srv = srv
	d.mu.Unlock()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the daemon's HTTP server and removes the
// socket file.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	srv := d.srv
	d.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	_ = os.Remove(d.socket)
	return err
}

func (d *Daemon) handleExpose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req ExposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "port must be 1..65535")
		return
	}

	url, err := d.client.Register(req.Name, req.Port)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	resolvedName := req.Name
	if resolvedName == "" {
		// Server derived the name from the port; mirror that locally.
		resolvedName = fmt.Sprintf("%d", req.Port)
	}

	d.urlsMu.Lock()
	d.urls[resolvedName] = url
	d.urlsMu.Unlock()

	writeJSON(w, http.StatusOK, ExposeResponse{URL: url})
}

func (d *Daemon) handleUnexpose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req UnexposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if err := d.client.Unregister(req.Name); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	d.urlsMu.Lock()
	delete(d.urls, req.Name)
	d.urlsMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (d *Daemon) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	healthy := d.client.IsHealthy()
	intended := d.client.Intended()

	d.urlsMu.Lock()
	urls := make(map[string]string, len(d.urls))
	for k, v := range d.urls {
		urls[k] = v
	}
	d.urlsMu.Unlock()

	items := make([]ListItem, 0, len(intended))
	for name, port := range intended {
		items = append(items, ListItem{
			Name:    name,
			Port:    port,
			URL:     urls[name],
			Healthy: healthy,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (d *Daemon) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthzResponse{
		Status:  "ok",
		Slug:    d.client.Slug(),
		Healthy: d.client.IsHealthy(),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}
