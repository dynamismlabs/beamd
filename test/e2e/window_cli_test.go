package e2e

// Command-level coverage for the Part A yamux stream window: it is resolved from
// BEAMD_YAMUX_STREAM_WINDOW_BYTES once per process, so these tests drive the REAL
// binary to prove (a) a present-invalid value fails startup on the edge and
// client paths, and (b) the resolved value propagates and is logged at edge and
// detached-agent startup. The in-process test elsewhere injects the value via
// Options and so cannot exercise the environment resolver or the command layer.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeServerConfig writes a minimal, valid edge config. serve resolves the
// window right after loading it (before binding anything), so it is enough for
// both the invalid-startup and the ready-log tests.
func writeServerConfig(t *testing.T, listenAddr string) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`base_domain: cli.example.com
listen_https: %q
acme_email: ops@example.com
acme_ca: "off"
dns_provider: stub
token_store: "memory:"
data_dir: %q
`, listenAddr, dir)
	p := filepath.Join(dir, "beamd.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCLI_YamuxWindow_InvalidEnvFailsStartup(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	beamd, _ := buildBinaries(t)

	// Client path: `check` resolves the window while building the connect options,
	// before dialing — so a bad value fails fast without needing a live edge.
	t.Run("check", func(t *testing.T) {
		cfg, _ := writeCLIConfig(t, "127.0.0.1:1", "T1")
		env := append(envWithHome(t.TempDir()), YamuxWindowEnv("not-a-number"))
		_, errOut, code := runBeamd(t, env, beamd, "check", "--config", cfg)
		if code == 0 {
			t.Fatal("check with an invalid window env should exit non-zero")
		}
		if !strings.Contains(errOut, "BEAMD_YAMUX_STREAM_WINDOW_BYTES") {
			t.Errorf("check stderr = %q, want it to name the bad env var", errOut)
		}
	})

	// Edge path: `serve` resolves the window right after loading config, before
	// binding any listener.
	t.Run("serve", func(t *testing.T) {
		scfg := writeServerConfig(t, "127.0.0.1:0")
		env := append(envWithHome(t.TempDir()), YamuxWindowEnv("100")) // below the 256 KiB min
		out, errOut, code := runBeamd(t, env, beamd, "serve", "--config", scfg)
		if code == 0 {
			t.Fatal("serve with an out-of-range window env should exit non-zero")
		}
		if !strings.Contains(out+errOut, "BEAMD_YAMUX_STREAM_WINDOW_BYTES") {
			t.Errorf("serve output = %q / %q, want it to name the bad env var", out, errOut)
		}
	})
}

func TestCLI_YamuxWindow_EdgeStartupLogAndPropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary + starts the edge")
	}
	beamd, _ := buildBinaries(t)
	addr := freeListenAddr(t)
	scfg := writeServerConfig(t, addr)
	env := append(envWithHome(t.TempDir()), YamuxWindowEnv("8388608")) // 8 MiB, a non-default in-range value

	var out safeBuffer
	cmd := exec.Command(beamd, "serve", "--config", scfg)
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// The resolved window must reach the edge and appear in its structured ready
	// log (proves both propagation into `serve` and the required startup log).
	waitUntil(t, "edge ready log carries the resolved window", 8*time.Second, func() bool {
		s := out.String()
		return strings.Contains(s, `"msg":"ready"`) && strings.Contains(s, `"yamux_stream_window_bytes":8388608`)
	})
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

func TestCLI_YamuxWindow_AgentStartupLogAndPropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries + spawns processes")
	}
	beamd, _ := buildBinaries(t)
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	cfg, socket := writeCLIConfig(t, edgeAddr, "T1")
	home := t.TempDir()
	env := append(envWithHome(home), YamuxWindowEnv("8388608"))
	t.Cleanup(func() { stopAgent(socket) })

	// The agent logs to <home>/.beamd/agent.log, but O_CREATE won't make the
	// parent dir — in real use ~/.beamd already exists (login writes there), so
	// mirror that here or the agent silently falls back to (discarded) stderr.
	if err := os.MkdirAll(filepath.Join(home, ".beamd"), 0o700); err != nil {
		t.Fatal(err)
	}

	// `open -d` spawns the detached agent, which inherits the environment and
	// resolves + logs the window at startup. Blocks until registered, so by the
	// time it returns the agent has connected and logged.
	_, errOut, code := runBeamd(t, env, beamd, "open", strconv.Itoa(port), "--as", "api", "-d", "--config", cfg)
	if code != 0 {
		t.Fatalf("open -d exit=%d stderr=%s", code, errOut)
	}

	logPath := filepath.Join(home, ".beamd", "agent.log")
	waitUntil(t, "agent startup log carries the resolved window", 5*time.Second, func() bool {
		b, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(b), `"yamux_stream_window_bytes":8388608`)
	})
}

// YamuxWindowEnv builds the KEY=VALUE env entry for the window override, keeping
// the variable name in one place across these tests.
func YamuxWindowEnv(v string) string {
	return "BEAMD_YAMUX_STREAM_WINDOW_BYTES=" + v
}
