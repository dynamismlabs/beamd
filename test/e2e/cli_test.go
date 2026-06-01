package e2e

// CLI integration tests: build the real `beamd` binary and drive it as a
// subprocess against an in-process edge, so the command layer (open
// foreground + detach, run, list, close, status, --json, signal teardown)
// is exercised end-to-end — not just the library primitives underneath it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	cliBuildOnce sync.Once
	cliBeamd     string
	cliTestapp   string
	cliBuildErr  error
)

// buildBinaries compiles beamd + beam-testapp once for the whole CLI test
// run and returns their paths.
func buildBinaries(t *testing.T) (beamd, testapp string) {
	t.Helper()
	cliBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "beamd-cli")
		if err != nil {
			cliBuildErr = err
			return
		}
		cliBeamd = filepath.Join(dir, "beamd")
		cliTestapp = filepath.Join(dir, "beam-testapp")
		for _, b := range []struct{ out, pkg string }{
			{cliBeamd, "github.com/dynamismlabs/beamd/cmd/beamd"},
			{cliTestapp, "github.com/dynamismlabs/beamd/cmd/beam-testapp"},
		} {
			if out, err := exec.Command("go", "build", "-o", b.out, b.pkg).CombinedOutput(); err != nil {
				cliBuildErr = fmt.Errorf("build %s: %v\n%s", b.pkg, err, out)
				return
			}
		}
	})
	if cliBuildErr != nil {
		t.Fatalf("build binaries: %v", cliBuildErr)
	}
	return cliBeamd, cliTestapp
}

// writeCLIConfig writes a client config pointing at the test edge, with an
// isolated agent socket so detached runs don't collide with a real agent.
func writeCLIConfig(t *testing.T, server, token string) (configPath, socketPath string) {
	t.Helper()
	// Keep the socket path short (macOS caps unix paths at ~104 chars).
	sf, err := os.CreateTemp("/tmp", "beamd-it-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath = sf.Name()
	_ = sf.Close()
	_ = os.Remove(socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	configPath = filepath.Join(t.TempDir(), "config")
	body := fmt.Sprintf("server: %s\ntoken: %s\nagent_socket: %s\ninsecure_skip_verify: true\n", server, token, socketPath)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, socketPath
}

// envWithHome returns the current environment with HOME redirected, so a
// spawned agent writes its log under a temp dir, not the real home.
func envWithHome(home string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "HOME="+home)
}

// runBeamd runs a beamd subcommand to completion and returns its output.
func runBeamd(t *testing.T, env []string, bin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return so.String(), se.String(), ee.ExitCode()
		}
		t.Fatalf("run beamd %v: %v", args, err)
	}
	return so.String(), se.String(), 0
}

// stopAgent stops the detached agent identified by its unique socket path.
func stopAgent(socketPath string) {
	_ = exec.Command("pkill", "-TERM", "-f", "agent --socket "+socketPath).Run()
}

// safeBuffer is a goroutine-safe buffer for capturing a long-running
// subprocess's output while the test polls it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCLI_DetachLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries + spawns processes")
	}
	beamd, _ := buildBinaries(t)
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	cfg, socket := writeCLIConfig(t, edgeAddr, "T1")
	env := envWithHome(t.TempDir())
	t.Cleanup(func() { stopAgent(socket) })

	// open -d --json → returns immediately with the full tunnel identity.
	out, errOut, code := runBeamd(t, env, beamd, "open", strconv.Itoa(port), "--as", "api", "-d", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("open -d exit=%d stderr=%s", code, errOut)
	}
	var res struct {
		URL        string `json:"url"`
		Name       string `json:"name"`
		Port       int    `json:"port"`
		Slug       string `json:"slug"`
		BaseDomain string `json:"baseDomain"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("open --json not a single JSON object: %q (%v)", out, err)
	}
	wantURL := "https://api.turing." + testBaseDomain
	if res.URL != wantURL || res.Name != "api" || res.Port != port || res.Slug != "turing" || res.BaseDomain != testBaseDomain {
		t.Fatalf("open --json = %+v, want url=%s name=api port=%d slug=turing base=%s", res, wantURL, port, testBaseDomain)
	}

	// A public request flows through the CLI-spawned agent to the backend.
	host := "api.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "api: GET /x\n")

	// list --json shows it.
	out, _, code = runBeamd(t, env, beamd, "list", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"name":"api"`) {
		t.Errorf("list --json = %q (exit %d), want it to contain api", out, code)
	}

	// status --json reports a running, connected agent.
	out, _, code = runBeamd(t, env, beamd, "status", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"agentRunning":true`) || !strings.Contains(out, `"slug":"turing"`) {
		t.Errorf("status --json = %q (exit %d)", out, code)
	}

	// close --json removes it.
	out, _, code = runBeamd(t, env, beamd, "close", "api", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"removed":true`) {
		t.Errorf("close --json = %q (exit %d), want removed:true", out, code)
	}
	waitUntil(t, "route removed after close", 2*time.Second, func() bool {
		resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/x")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound
	})

	// Idempotent: closing again → removed:false, still exit 0.
	out, _, code = runBeamd(t, env, beamd, "close", "api", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"removed":false`) {
		t.Errorf("second close --json = %q (exit %d), want removed:false exit 0", out, code)
	}
}

func TestCLI_ForegroundOpenServesAndStopsOnSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries + spawns processes")
	}
	beamd, _ := buildBinaries(t)
	port := startDummyApp(t, "fg")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	cfg, _ := writeCLIConfig(t, edgeAddr, "T1")
	env := envWithHome(t.TempDir())

	var out safeBuffer
	cmd := exec.Command(beamd, "open", strconv.Itoa(port), "--as", "fg", "--config", cfg)
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	host := "fg.turing." + testBaseDomain
	waitUntil(t, "foreground open prints the URL", 5*time.Second, func() bool {
		return strings.Contains(out.String(), "https://"+host)
	})
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/y", "fg: GET /y\n")

	// Ctrl-C → the process exits and the tunnel is torn down.
	_ = cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground open did not exit after SIGINT")
	}
	waitUntil(t, "route gone after foreground stop", 3*time.Second, func() bool {
		resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/y")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound
	})
}

func TestCLI_RunWrapsCommandAndStopsOnSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries + spawns processes")
	}
	beamd, testapp := buildBinaries(t)
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	cfg, _ := writeCLIConfig(t, edgeAddr, "T1")
	env := envWithHome(t.TempDir())

	var out safeBuffer
	// `run` wraps beam-testapp, which binds $PORT (set by run).
	cmd := exec.Command(beamd, "run", "runapp", "--config", cfg, "--", testapp)
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	host := "runapp.turing." + testBaseDomain
	waitUntil(t, "run brings the tunnel up", 8*time.Second, func() bool {
		return strings.Contains(out.String(), "https://"+host)
	})

	// The wrapped app serves through the tunnel.
	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "beam-testapp") {
		t.Errorf("run-wrapped response: status=%d body=%q", resp.StatusCode, body)
	}

	// Ctrl-C → beamd exits, tearing down the tunnel and the wrapped command.
	_ = cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after SIGINT")
	}
	waitUntil(t, "route gone after run stop", 3*time.Second, func() bool {
		resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound
	})
}
