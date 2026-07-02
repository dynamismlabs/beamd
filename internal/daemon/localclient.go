package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// LocalClient is an HTTP client over a unix domain socket — the
// "talk to the agent" side of the CLI.
type LocalClient struct {
	socket string
	http   *http.Client
}

func NewLocalClient(socketPath string) *LocalClient {
	return &LocalClient{
		socket: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}
}

func (c *LocalClient) Ping(ctx context.Context) (*HealthzResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/healthz", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon /healthz returned %s", resp.Status)
	}
	var out HealthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Open brings a local port up as a public tunnel via the agent and
// returns the resolved tunnel identity. A non-empty scope pins the request:
// the agent refuses (409) if its session is connected to a different org.
func (c *LocalClient) Open(ctx context.Context, port int, name, scope string) (*OpenResponse, error) {
	body, _ := json.Marshal(OpenRequest{Port: port, Name: name, Scope: scope})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/open", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var out OpenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Close removes (closes) the named tunnel and reports whether it was
// present. A missing name is not an error (the call is idempotent).
// NB: this closes a *tunnel* by name — it does not release the
// LocalClient itself (which owns no persistent connection).
func (c *LocalClient) Close(ctx context.Context, name string) (bool, error) {
	body, _ := json.Marshal(CloseRequest{Name: name})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/close", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, decodeErr(resp)
	}
	var out CloseResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Removed, nil
}

// Shutdown asks the agent to stop. The agent replies then exits, so the
// HTTP round-trip may error as the connection drops mid-response — that's
// still a successful shutdown, so callers treat any result as best-effort
// and poll IsRunning to confirm the socket has freed.
func (c *LocalClient) Shutdown(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/shutdown", nil)
	resp, err := c.http.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}

func (c *LocalClient) List(ctx context.Context) ([]ListItem, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/list", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var out []ListItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeErr(resp *http.Response) error {
	var e ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err == nil && e.Error != "" {
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	return fmt.Errorf("daemon returned %s", resp.Status)
}

// EnsureRunning probes the agent at socketPath; if it doesn't answer,
// spawns `executable agent --socket <path>` as a detached background
// process and waits up to 5 seconds for it to start. The spawned agent
// loads its server / token settings from the client config the CLI
// passes through env.
//
// `extraEnv` lets the caller pass through `BEAMD_CONFIG` etc. so the
// agent picks up the same config the CLI saw.
func EnsureRunning(ctx context.Context, executable, socketPath string, extraEnv []string) error {
	if ok, _ := probe(socketPath, 200*time.Millisecond); ok {
		return nil
	}

	cmd := exec.Command(executable, "agent", "--socket", socketPath)
	// Inherit the caller's environment (so the agent can resolve $HOME for
	// its log, etc.) and add the pass-through vars (e.g. BEAMD_CONFIG).
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn agent: %w", err)
	}
	// We deliberately don't Wait — the agent should outlive the CLI.

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := probe(socketPath, 200*time.Millisecond); ok {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return fmt.Errorf("agent did not start within 5s")
}

// IsRunning reports whether an agent is currently listening on
// socketPath, without spawning one. Used by read-only / teardown
// commands (list, down, status) that must not start an agent just to
// answer.
func IsRunning(socketPath string) bool {
	ok, _ := probe(socketPath, 200*time.Millisecond)
	return ok
}

func probe(socketPath string, timeout time.Duration) (bool, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.Dial("unix", socketPath)
	if err != nil {
		return false, err
	}
	_ = c.Close()
	return true, nil
}
