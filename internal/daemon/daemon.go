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
	"syscall"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/naming"
)

// Daemon wraps a long-lived *client.Client behind a loopback HTTP API
// served over a unix domain socket (PRD §10, §17). FS permissions on
// the socket enforce single-user access; no in-band auth.
type Daemon struct {
	client *client.Client
	socket string

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	lock *os.File // flock on <socket>.lock; held for the daemon's lifetime

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

	// Exclusive flock before touching the socket. The probe→spawn window in
	// EnsureRunning means two CLIs can both spawn agents; without the lock the
	// loser's unconditional Remove unlinks the winner's LIVE socket, orphaning
	// an agent that still holds edge registrations but that no `beamd
	// list`/`close`/`reload` can ever reach. The kernel drops the lock if we
	// die, so a crashed agent never wedges the next one.
	lock, err := acquireLock(d.socket+".lock", 6*time.Second)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.lock = lock
	d.mu.Unlock()

	// Safe now: the lock proves no live agent owns this socket path.
	_ = os.Remove(d.socket)

	ln, err := net.Listen("unix", d.socket)
	if err != nil {
		d.releaseLock()
		return fmt.Errorf("listen unix %s: %w", d.socket, err)
	}
	if err := os.Chmod(d.socket, 0o600); err != nil {
		_ = ln.Close()
		d.releaseLock()
		return fmt.Errorf("chmod socket: %w", err)
	}
	slog.Info("agent listening", "socket", d.socket)

	mux := http.NewServeMux()
	mux.HandleFunc("/open", d.handleOpen)
	mux.HandleFunc("/close", d.handleClose)
	mux.HandleFunc("/list", d.handleList)
	mux.HandleFunc("/healthz", d.handleHealthz)
	mux.HandleFunc("/shutdown", d.handleShutdown)

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
// socket file. The socket removal happens while we still hold the lock, so a
// replacement agent (already waiting on the flock) can never have its fresh
// socket unlinked by our teardown.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	srv := d.srv
	d.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	_ = os.Remove(d.socket)
	d.releaseLock()
	return err
}

// acquireLock takes an exclusive flock on path, retrying until the deadline.
// The retry window covers `beamd reload`: the replacement agent starts while
// the old one is still draining (up to ~5s) and must wait its turn, not die.
func acquireLock(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another agent already holds %s", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (d *Daemon) releaseLock() {
	d.mu.Lock()
	lock := d.lock
	d.lock = nil
	d.mu.Unlock()
	if lock != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
}

func (d *Daemon) handleOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req OpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "port must be 1..65535")
		return
	}
	// The agent's edge session is pinned to one scope from spawn. A caller
	// that resolved a different scope (other repo's beamd.yaml, --scope, a
	// re-login) must not be silently served from the wrong org.
	if req.Scope != "" && req.Scope != d.client.Slug() {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"agent is connected to scope %q but this open resolved scope %q — run `beamd reload` to restart the agent with the new scope",
			d.client.Slug(), req.Scope))
		return
	}

	url, err := d.client.Register(req.Name, req.Port)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	resolvedName := req.Name
	if resolvedName == "" {
		// Server derived the name from the port (naming.LabelFromPort);
		// mirror that derivation locally so our records line up.
		resolvedName = naming.LabelFromPort(req.Port)
	}

	d.urlsMu.Lock()
	d.urls[resolvedName] = url
	d.urlsMu.Unlock()

	writeJSON(w, http.StatusOK, OpenResponse{
		URL:        url,
		Name:       resolvedName,
		Port:       req.Port,
		Slug:       d.client.Slug(),
		BaseDomain: d.client.BaseDomain(),
	})
}

func (d *Daemon) handleClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req CloseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}

	d.urlsMu.Lock()
	_, existed := d.urls[req.Name]
	delete(d.urls, req.Name)
	d.urlsMu.Unlock()

	if err := d.client.Unregister(req.Name); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CloseResponse{Removed: existed})
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

// handleShutdown stops the agent process (used by `beamd reload` to restart
// it with fresh credentials). It replies first, then shuts the server down
// asynchronously so Serve() unblocks and the agent process exits.
func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Shutdown(ctx)
	}()
}

func (d *Daemon) handleHealthz(w http.ResponseWriter, r *http.Request) {
	diagnostics := d.client.Diagnostics()
	writeJSON(w, http.StatusOK, HealthzResponse{
		Status:              "ok",
		Slug:                d.client.Slug(),
		Healthy:             d.client.IsHealthy(),
		Transport:           string(d.client.Transport()),
		ConfiguredTransport: diagnostics.ConfiguredTransport,
		FallbackCount:       diagnostics.FallbackCount,
		LastFallbackReason:  diagnostics.LastFallbackReason,
		ReconnectCount:      diagnostics.ReconnectCount,
		LastCloseReason:     diagnostics.LastCloseReason,
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
