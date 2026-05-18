// Package mux centralizes the yamux configuration used by both the edge
// and client. Both sides MUST use identical config to stay in sync.
//
// PRD §5: one TLS conn carrying many logical streams. yamux supplies
// the multiplexing layer; tuning happens here.
package mux

import (
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

func Config() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard // we route through slog ourselves
	return cfg
}

func Server(conn net.Conn) (*yamux.Session, error) { return yamux.Server(conn, Config()) }
func Client(conn net.Conn) (*yamux.Session, error) { return yamux.Client(conn, Config()) }
