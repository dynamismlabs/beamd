package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"syscall"
	"time"
)

// LocalClient is an HTTP client over a unix domain socket — the
// "talk to the daemon" side of the CLI.
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

func (c *LocalClient) Expose(ctx context.Context, port int, name string) (string, error) {
	body, _ := json.Marshal(ExposeRequest{Port: port, Name: name})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/expose", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", decodeErr(resp)
	}
	var out ExposeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *LocalClient) Unexpose(ctx context.Context, name string) error {
	body, _ := json.Marshal(UnexposeRequest{Name: name})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/unexpose", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeErr(resp)
	}
	return nil
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

// EnsureRunning probes the daemon at socketPath; if it doesn't answer,
// spawns `executable daemon --socket <path>` as a detached background
// process and waits up to 5 seconds for it to start. The spawned daemon
// loads its server / token settings from the client config the CLI
// passes through env.
//
// `extraEnv` lets the caller pass through `CONDUIT_SERVER` etc. so the
// daemon picks up the same config the CLI saw.
func EnsureRunning(ctx context.Context, executable, socketPath string, extraEnv []string) error {
	if ok, _ := probe(socketPath, 200*time.Millisecond); ok {
		return nil
	}

	cmd := exec.Command(executable, "daemon", "--socket", socketPath)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	// We deliberately don't Wait — daemon should outlive the CLI.

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
	return fmt.Errorf("daemon did not start within 5s")
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
