//go:build darwin || linux

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
)

const suspendedClientHelperEnv = "BEAMD_SUSPENDED_CLIENT_HELPER"

// TestClient_SuspendResumeReconnectsAndReplays models a laptop sleep without
// suspending the edge: a real child client is stopped past the application
// heartbeat timeout, then resumed. The existing forced-transport CI matrix
// runs this once over TCP and once over QUIC.
func TestClient_SuspendResumeReconnectsAndReplays(t *testing.T) {
	if os.Getenv(suspendedClientHelperEnv) == "1" {
		runSuspendedClientHelper(t)
		return
	}
	if testing.Short() {
		t.Skip("spawns and suspends a helper process")
	}

	port := startDummyApp(t, "wake")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	e.SetHeartbeatTimeout(time.Second)

	var output safeBuffer
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestClient_SuspendResumeReconnectsAndReplays$",
		"-test.timeout=20s",
	)
	cmd.Env = append(os.Environ(),
		suspendedClientHelperEnv+"=1",
		"BEAMD_SUSPEND_EDGE="+edgeAddr,
		"BEAMD_SUSPEND_PORT="+strconv.Itoa(port),
	)
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	waitUntil(t, "helper route", 4*time.Second, func() bool {
		return e.SessionCount() == 1 && e.RouteCount() == 1
	})
	firstSessions := e.SessionsCreatedTotal()

	// An idle but scheduled client must remain connected for several watchdog
	// windows while its application heartbeats continue.
	time.Sleep(3 * time.Second)
	if got := e.SessionsCreatedTotal(); got != firstSessions {
		t.Fatalf(
			"idle helper reconnected before suspension: sessions %d -> %d\n%s",
			firstSessions,
			got,
			output.String(),
		)
	}
	if e.SessionCount() != 1 || e.RouteCount() != 1 {
		t.Fatalf(
			"idle helper lost liveness before suspension: sessions=%d routes=%d\n%s",
			e.SessionCount(),
			e.RouteCount(),
			output.String(),
		)
	}

	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP helper: %v", err)
	}
	waitUntil(t, "edge heartbeat reap while helper is stopped", 5*time.Second, func() bool {
		return e.SessionCount() == 0 && e.RouteCount() == 0
	})

	transport := e2eTransport(t)
	metricsClient := publicHTTPSClient(edgeAddr, testBaseDomain)
	t.Cleanup(metricsClient.CloseIdleConnections)
	idleMetric := fmt.Sprintf(
		`beam_transport_session_closes_total{transport=%q,reason="idle"}`,
		transport,
	)
	protocolMetric := fmt.Sprintf(
		`beam_transport_session_closes_total{transport=%q,reason="protocol"}`,
		transport,
	)
	waitUntil(t, "heartbeat close to be recorded as idle", 2*time.Second, func() bool {
		resp := getMetrics(t, metricsClient)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return err == nil && metricValue(string(body), idleMetric) == 1
	})
	resp := getMetrics(t, metricsClient)
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read metrics: %v", readErr)
	}
	if got := metricValue(string(body), protocolMetric); got != 0 {
		t.Fatalf("%s = %d, want 0 after heartbeat timeout", protocolMetric, got)
	}

	resumedAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT helper: %v", err)
	}
	waitUntil(t, "helper reconnect and route replay after resume", 4*time.Second, func() bool {
		return e.SessionsCreatedTotal() > firstSessions &&
			e.SessionCount() == 1 &&
			e.RouteCount() == 1
	})

	host := "wake.turing." + testBaseDomain
	publicClient := publicHTTPSClient(edgeAddr, host)
	t.Cleanup(publicClient.CloseIdleConnections)
	checkResponse(
		t,
		publicClient,
		"https://"+host+"/after-wake",
		"wake: GET /after-wake\n",
	)
	t.Logf(
		"%s session resumed and replayed its route in %v",
		transport,
		time.Since(resumedAt),
	)
}

func runSuspendedClientHelper(t *testing.T) {
	edgeAddr := os.Getenv("BEAMD_SUSPEND_EDGE")
	port, err := strconv.Atoi(os.Getenv("BEAMD_SUSPEND_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	c, err := client.Connect(ctx, edgeAddr, "T1", client.Options{
		HeartbeatInterval:  100 * time.Millisecond,
		RegisterTimeout:    2 * time.Second,
		ReconnectInitial:   20 * time.Millisecond,
		ReconnectMax:       100 * time.Millisecond,
		InsecureSkipVerify: true,
		Transport:          e2eTransport(t),
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Register("wake", port); err != nil {
		t.Fatal(err)
	}
	fmt.Println("helper ready")
	select {}
}
