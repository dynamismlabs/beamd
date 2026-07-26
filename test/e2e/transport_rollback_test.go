package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/edge"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

// rollbackEdgeConfig uses explicit Part B capacities so the first edge can
// opt into QUIC. The production defaults deliberately remain TCP-only.
func rollbackEdgeConfig(addr, dataDir string, disableQUIC bool) *config.Server {
	return &config.Server{
		BaseDomain:           testBaseDomain,
		URLShape:             "subdomain",
		ListenHTTPS:          addr,
		ListenQUIC:           addr,
		DisableQUIC:          disableQUIC,
		ACMEEmail:            "test@example.com",
		DNSProvider:          "stub",
		TokenStore:           "memory:",
		MaxTunnelsPerToken:   25,
		MaxStreamsPerSession: config.DefaultMaxStreamsPerSession,
		MaxStreamsTotal:      config.DefaultMaxStreamsTotal,
		MaxPreAuthSessions:   config.DefaultMaxPreAuthSessions,
		MaxSessionsTotal:     config.DefaultMaxSessionsTotal,
		DataDir:              dataDir,
	}
}

func transportOverrideTestEnv(home, override string) []string {
	prefix := config.TransportEnvVar + "="
	base := envWithHome(home)
	env := make([]string, 0, len(base)+1)
	for _, item := range base {
		if len(item) >= len(prefix) && item[:len(prefix)] == prefix {
			continue
		}
		env = append(env, item)
	}
	if override != "" {
		env = append(env, prefix+override)
	}
	return env
}

func writeAutoTransportCLIConfig(t *testing.T, server string) (configPath, socketPath string) {
	t.Helper()
	socketFile, err := os.CreateTemp("/tmp", "beamd-transport-override-*.sock")
	if err != nil {
		t.Fatalf("create agent socket path: %v", err)
	}
	socketPath = socketFile.Name()
	_ = socketFile.Close()
	_ = os.Remove(socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	configPath = filepath.Join(t.TempDir(), "client.yaml")
	body := fmt.Sprintf(
		"server: %s\ntoken: T1\nagent_socket: %s\ninsecure_skip_verify: true\ntransport: auto\n",
		server,
		socketPath,
	)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	return configPath, socketPath
}

type transportOverrideStatus struct {
	AgentRunning        bool   `json:"agentRunning"`
	Healthy             bool   `json:"healthy"`
	Transport           string `json:"transport"`
	ConfiguredTransport string `json:"configuredTransport"`
}

func readTransportOverrideStatus(
	t *testing.T,
	env []string,
	beamd,
	configPath string,
) transportOverrideStatus {
	t.Helper()
	stdout, stderr, code := runBeamd(t, env, beamd, "status", "--json", "--config", configPath)
	if code != 0 {
		t.Fatalf("status exit = %d, stderr = %s", code, stderr)
	}
	var status transportOverrideStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("decode status %q: %v", stdout, err)
	}
	return status
}

func startRollbackEdge(
	t *testing.T,
	cfg *config.Server,
) (*edge.Edge, <-chan error) {
	t.Helper()
	mgr, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatalf("cert manager: %v", err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "turing"}), mgr)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- e.Serve()
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	waitForTCP(t, cfg.ListenHTTPS, 2*time.Second)
	select {
	case err := <-serveErr:
		t.Fatalf("edge stopped during startup: %v", err)
	default:
	}
	return e, serveErr
}

func stopRollbackEdge(t *testing.T, e *edge.Edge, serveErr <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := e.Shutdown(ctx)
	cancel()
	if err != nil {
		t.Fatalf("edge shutdown: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("edge serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("edge Serve did not return after shutdown")
	}
}

// TestTransportRollback_AutoAgentReconnectsOverTCP rehearses the production
// edge kill switch without changing the established agent's configuration:
//
//  1. an auto agent establishes QUIC and registers a working route;
//  2. the edge is stopped and restarted on the same public TCP address;
//  3. the replacement has QUIC disabled and intentionally unusable QUIC
//     listener/key inputs, proving the disabled path touches neither;
//  4. the same agent falls back to TCP, replays its registration, and reports
//     the selected transport through the local diagnostics API.
func TestTransportRollback_AutoAgentReconnectsOverTCP(t *testing.T) {
	const malformedKeyName = "quic-stateless-reset.key"
	const secondKeyName = "quic-token-generator.key"

	edgeAddr := freeListenAddr(t)
	backendPort := startDummyApp(t, "rollback")

	firstCfg := rollbackEdgeConfig(edgeAddr, t.TempDir(), false)
	firstEdge, firstServeErr := startRollbackEdge(t, firstCfg)

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
	c, err := client.Connect(connectCtx, edgeAddr, "T1", client.Options{
		HeartbeatInterval:  100 * time.Millisecond,
		RegisterTimeout:    2 * time.Second,
		ReconnectInitial:   50 * time.Millisecond,
		ReconnectMax:       200 * time.Millisecond,
		InsecureSkipVerify: true,
		Transport:          "auto",
	})
	cancelConnect()
	if err != nil {
		t.Fatalf("connect auto client: %v", err)
	}
	defer c.Close()
	if got := c.Transport(); got != tunnel.KindQUIC {
		t.Fatalf("initial selected transport = %q, want %q", got, tunnel.KindQUIC)
	}
	if _, err := c.Register("rollback", backendPort); err != nil {
		t.Fatalf("register: %v", err)
	}

	daemonClient := startDaemon(t, c)
	host := "rollback.turing." + testBaseDomain
	if err := getAndCheck(
		publicHTTPSClient(edgeAddr, host),
		"https://"+host+"/before",
		"rollback: GET /before\n",
	); err != nil {
		t.Fatalf("request over initial QUIC session: %v", err)
	}

	initialHealthCtx, cancelInitialHealth := context.WithTimeout(context.Background(), time.Second)
	initialHealth, err := daemonClient.Ping(initialHealthCtx)
	cancelInitialHealth()
	if err != nil {
		t.Fatalf("initial diagnostics: %v", err)
	}
	if initialHealth.Transport != string(tunnel.KindQUIC) ||
		initialHealth.ConfiguredTransport != "auto" {
		t.Fatalf("initial diagnostics = %+v, want active quic/configured auto", initialHealth)
	}

	stopRollbackEdge(t, firstEdge, firstServeErr)

	disabledDataDir := t.TempDir()
	malformedKeyPath := filepath.Join(disabledDataDir, malformedKeyName)
	malformedKey := []byte("must remain malformed while QUIC is disabled")
	if err := os.WriteFile(malformedKeyPath, malformedKey, 0o600); err != nil {
		t.Fatalf("write malformed QUIC key: %v", err)
	}

	replacementCfg := rollbackEdgeConfig(edgeAddr, disabledDataDir, true)
	// Either setting would make QUIC startup fail. Successful TCP startup with
	// both present proves the kill switch bypasses UDP validation/bind and key
	// loading rather than merely suppressing QUIC advertisement.
	replacementCfg.ListenQUIC = "not-a-valid-udp-listen-address"
	replacementEdge, _ := startRollbackEdge(t, replacementCfg)

	waitUntil(t, "auto agent to reconnect over TCP and replay its route", 10*time.Second, func() bool {
		return c.IsHealthy() &&
			c.Transport() == tunnel.KindYamux &&
			replacementEdge.RouteCount() == 1
	})

	if err := getAndCheck(
		publicHTTPSClient(edgeAddr, host),
		"https://"+host+"/after",
		"rollback: GET /after\n",
	); err != nil {
		t.Fatalf("request after rollback to TCP: %v", err)
	}

	healthCtx, cancelHealth := context.WithTimeout(context.Background(), time.Second)
	health, err := daemonClient.Ping(healthCtx)
	cancelHealth()
	if err != nil {
		t.Fatalf("post-rollback diagnostics: %v", err)
	}
	if !health.Healthy ||
		health.Transport != string(tunnel.KindYamux) ||
		health.ConfiguredTransport != "auto" ||
		health.FallbackCount == 0 ||
		health.ReconnectCount == 0 ||
		health.LastCloseReason != "shutdown" {
		t.Fatalf("post-rollback diagnostics = %+v, want healthy TCP fallback from unchanged auto", health)
	}
	if health.LastFallbackReason != "network" && health.LastFallbackReason != "timeout" {
		t.Fatalf("last fallback reason = %q, want fixed network or timeout category", health.LastFallbackReason)
	}

	gotMalformedKey, err := os.ReadFile(malformedKeyPath)
	if err != nil {
		t.Fatalf("read malformed QUIC key after disabled startup: %v", err)
	}
	if !bytes.Equal(gotMalformedKey, malformedKey) {
		t.Fatalf("disabled QUIC startup changed malformed key: got %q, want %q", gotMalformedKey, malformedKey)
	}
	if _, err := os.Stat(filepath.Join(disabledDataDir, secondKeyName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled QUIC startup touched second key: stat error = %v", err)
	}
}

// TestTransportRollback_LocalEnvironmentOverride rehearses the per-agent
// rollback control through the real CLI reload path. The profile remains
// `transport: auto`; BEAMD_TRANSPORT=tcp is resolved inside the new agent
// process at startup, exactly as it is in production.
func TestTransportRollback_LocalEnvironmentOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI and exercises a detached agent reload")
	}

	beamd, _ := buildBinaries(t)
	edgeAddr := freeListenAddr(t)
	backendPort := startDummyApp(t, "override")
	edgeCfg := rollbackEdgeConfig(edgeAddr, t.TempDir(), false)
	_, _ = startRollbackEdge(t, edgeCfg)

	configPath, socketPath := writeAutoTransportCLIConfig(t, edgeAddr)
	home := t.TempDir()
	t.Cleanup(func() { stopAgent(socketPath) })
	autoEnv := transportOverrideTestEnv(home, "")
	tcpEnv := transportOverrideTestEnv(home, config.TransportTCP)

	// Start the detached agent from the auto profile with no process override.
	stdout, stderr, code := runBeamd(t, autoEnv, beamd, "reload", "--config", configPath)
	if code != 0 {
		t.Fatalf("initial auto reload exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	initialStatus := readTransportOverrideStatus(t, autoEnv, beamd, configPath)
	if !initialStatus.AgentRunning ||
		!initialStatus.Healthy ||
		initialStatus.Transport != string(tunnel.KindQUIC) ||
		initialStatus.ConfiguredTransport != config.TransportAuto {
		t.Fatalf("initial auto status = %+v, want healthy QUIC agent configured auto", initialStatus)
	}

	// `beamd reload` stops that established agent and spawns a fresh process.
	// The child inherits BEAMD_TRANSPORT=tcp; agentCmd passes the auto profile
	// through config.ResolveTransport, so the override is applied at the same
	// lifecycle boundary operators use in production.
	stdout, stderr, code = runBeamd(t, tcpEnv, beamd, "reload", "--config", configPath)
	if code != 0 {
		t.Fatalf("TCP override reload exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	overrideStatus := readTransportOverrideStatus(t, tcpEnv, beamd, configPath)
	if !overrideStatus.AgentRunning ||
		!overrideStatus.Healthy ||
		overrideStatus.Transport != string(tunnel.KindYamux) ||
		overrideStatus.ConfiguredTransport != config.TransportTCP {
		t.Fatalf("override status = %+v, want healthy TCP agent configured by env", overrideStatus)
	}

	// Reload intentionally restarts process-local agent state, so register the
	// desired tunnel with the replacement just as an operator or supervisor
	// does after reload, then prove the selected TCP data path serves traffic.
	stdout, stderr, code = runBeamd(
		t,
		tcpEnv,
		beamd,
		"open",
		fmt.Sprintf("%d", backendPort),
		"--as",
		"override",
		"-d",
		"--json",
		"--config",
		configPath,
	)
	if code != 0 {
		t.Fatalf("open after TCP override exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	host := "override.turing." + testBaseDomain
	if err := getAndCheck(
		publicHTTPSClient(edgeAddr, host),
		"https://"+host+"/tcp",
		"override: GET /tcp\n",
	); err != nil {
		t.Fatalf("request through locally overridden agent: %v", err)
	}

	// A forced diagnostic connection still negotiates QUIC against the same
	// live edge. This distinguishes the local agent override from the global
	// edge kill switch and proves the rollback control is scoped correctly.
	stdout, stderr, code = runBeamd(
		t,
		tcpEnv,
		beamd,
		"check",
		"--transport",
		"quic",
		"--json",
		"--config",
		configPath,
	)
	if code != 0 {
		t.Fatalf("forced QUIC check exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	var check struct {
		OK        bool   `json:"ok"`
		Transport string `json:"transport"`
	}
	if err := json.Unmarshal([]byte(stdout), &check); err != nil {
		t.Fatalf("decode forced QUIC check %q: %v", stdout, err)
	}
	if !check.OK || check.Transport != string(tunnel.KindQUIC) {
		t.Fatalf("forced QUIC check = %+v, want available QUIC edge", check)
	}

	finalStatus := readTransportOverrideStatus(t, tcpEnv, beamd, configPath)
	if finalStatus.Transport != string(tunnel.KindYamux) ||
		finalStatus.ConfiguredTransport != config.TransportTCP {
		t.Fatalf("agent changed after forced QUIC check: %+v", finalStatus)
	}

	persistedProfile, err := config.LoadClient(configPath)
	if err != nil {
		t.Fatalf("reload auto profile: %v", err)
	}
	if persistedProfile.Transport != config.TransportAuto {
		t.Fatalf("profile transport changed to %q, want persisted auto", persistedProfile.Transport)
	}
}
