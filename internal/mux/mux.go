// Package mux centralizes the yamux configuration used by both the edge and
// client. The per-stream receive window belongs to the receiver, so the edge and
// agent set it independently and their values need NOT match — the edge window
// governs downloads (agent → edge), the agent window governs uploads
// (edge → agent). Every other framing value is shared (transport-performance-spec
// §8.1).
//
// PRD §5: one TLS conn carrying many logical streams. yamux supplies the
// multiplexing layer; tuning happens here.
package mux

import (
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// DefaultStreamWindow is beamd's tuned yamux per-stream receive window (4 MiB),
// up from yamux's 256 KiB library default. A stream moves roughly one window per
// round trip, so the 256 KiB default capped any solo transfer at 256 KiB / RTT;
// 4 MiB lifts that ceiling (transport-performance-spec §8.1 / defect A1). It is
// the compiled default the environment resolver applies when the variable is
// absent, and the floor mux uses if a caller passes 0.
const DefaultStreamWindow = 4 << 20 // 4 MiB

// Config builds the shared yamux config. windowBytes sets MaxStreamWindowSize —
// the per-stream receive window this side advertises to the peer. It is the ONLY
// value Part A changes; everything else stays at the current beamd defaults. A
// zero value falls back to DefaultStreamWindow so the config is always valid for
// yamux (the process resolver normally passes an already-validated value).
func Config(windowBytes uint32) *yamux.Config {
	if windowBytes == 0 {
		windowBytes = DefaultStreamWindow
	}
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.MaxStreamWindowSize = windowBytes
	cfg.LogOutput = io.Discard // we route through slog ourselves
	return cfg
}

func Server(conn net.Conn, windowBytes uint32) (*yamux.Session, error) {
	return yamux.Server(conn, Config(windowBytes))
}

func Client(conn net.Conn, windowBytes uint32) (*yamux.Session, error) {
	return yamux.Client(conn, Config(windowBytes))
}
