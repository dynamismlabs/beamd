package edge

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

func TestServeDoesNotLogReadyUntilEveryEnabledListenerBinds(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	tcpAddress := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatalf("release TCP address: %v", err)
	}

	cfg := &config.Server{
		BaseDomain:           "edge.test",
		ListenHTTPS:          tcpAddress,
		ListenQUIC:           "127.0.0.1:0",
		DisableQUIC:          false,
		DataDir:              t.TempDir(),
		MetricsToken:         "metrics",
		MaxStreamsPerSession: config.DefaultMaxStreamsPerSession,
		MaxStreamsTotal:      config.DefaultMaxStreamsTotal,
		MaxPreAuthSessions:   config.DefaultMaxPreAuthSessions,
		MaxSessionsTotal:     config.DefaultMaxSessionsTotal,
	}
	manager, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatalf("cert manager: %v", err)
	}
	e := New(cfg, "test", auth.NewMemoryStore(nil), manager)
	tcpWasBound := false
	e.listenQUIC = func(
		string,
		*tls.Config,
		string,
		func(error),
	) (tunnel.Listener, io.Closer, error) {
		conn, dialErr := net.DialTimeout("tcp", tcpAddress, time.Second)
		if dialErr != nil {
			return nil, nil, fmt.Errorf("TCP listener was not bound before QUIC setup: %w", dialErr)
		}
		tcpWasBound = true
		_ = conn.Close()
		return nil, nil, errors.New("synthetic QUIC bind failure")
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	err = e.Serve()
	if err == nil || !strings.Contains(err.Error(), "listen QUIC") {
		t.Fatalf("Serve error = %v, want QUIC bind failure", err)
	}
	if !tcpWasBound {
		t.Fatal("synthetic QUIC failure occurred before the TCP listener was bound")
	}
	if strings.Contains(logs.String(), "msg=ready") {
		t.Fatalf("ready was logged before the enabled QUIC listener bound:\n%s", logs.String())
	}
}
