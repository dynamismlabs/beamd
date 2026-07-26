package edge

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dynamismlabs/beamd/internal/tunnel"
)

func TestTransportMetricsExposeExactFixedLabelSeries(t *testing.T) {
	m := newMetrics()
	m.configure(64, 128, 4<<20)

	m.setListener(tunnel.KindYamux, true)
	m.addSessionState(tunnel.KindYamux, "preauth", 2)
	m.addSessionState(tunnel.KindYamux, "authenticated", 3)
	m.addSessionState(tunnel.KindQUIC, "preauth", 4)
	m.addSessionState(tunnel.KindQUIC, "authenticated", 5)
	m.recordSessionCreated(tunnel.KindYamux)
	m.recordSessionCreated(tunnel.KindQUIC)
	m.recordSessionCreated(tunnel.KindQUIC)
	m.addStream(tunnel.KindYamux, 6)
	m.addStream(tunnel.KindQUIC, 7)

	m.recordHandshakeError(tunnel.KindYamux, "timeout")
	m.recordHandshakeError(tunnel.KindYamux, "timeout")
	m.recordHandshakeError(tunnel.KindYamux, "raw dynamic error")
	m.recordHandshakeError(tunnel.KindQUIC, "tls")
	m.recordHandshakeError(tunnel.KindQUIC, "protocol")

	m.recordSessionClose(tunnel.KindYamux, "normal")
	m.recordSessionClose(tunnel.KindYamux, "shutdown")
	m.recordSessionClose(tunnel.KindQUIC, "idle")
	m.recordSessionClose(tunnel.KindQUIC, "protocol")
	m.recordSessionClose(tunnel.KindQUIC, "network")
	m.recordSessionClose(tunnel.KindQUIC, "raw dynamic error")

	m.recordStreamOpenError(tunnel.KindYamux, "timeout")
	m.recordStreamOpenError(tunnel.KindYamux, "closed")
	m.recordStreamOpenError(tunnel.KindYamux, "closed")
	m.recordStreamOpenError(tunnel.KindQUIC, "raw dynamic error")

	for _, scope := range capacityScopeLabels {
		m.recordCapacityRejection(scope)
	}
	m.recordCapacityRejection("raw_dynamic_scope")

	var exposition bytes.Buffer
	m.writeText(&exposition, 0)

	var got []string
	for _, line := range strings.Split(exposition.String(), "\n") {
		if strings.HasPrefix(line, "beam_transport_") ||
			strings.HasPrefix(line, "beam_yamux_") {
			got = append(got, line)
		}
	}

	want := []string{
		`beam_transport_listener_up{transport="tcp"} 1`,
		`beam_transport_listener_up{transport="quic"} 0`,
		`beam_transport_sessions_active{transport="tcp",state="preauth"} 2`,
		`beam_transport_sessions_active{transport="tcp",state="authenticated"} 3`,
		`beam_transport_sessions_active{transport="quic",state="preauth"} 4`,
		`beam_transport_sessions_active{transport="quic",state="authenticated"} 5`,
		`beam_transport_sessions_total{transport="tcp"} 1`,
		`beam_transport_sessions_total{transport="quic"} 2`,
		`beam_transport_streams_active{transport="tcp"} 6`,
		`beam_transport_streams_active{transport="quic"} 7`,
		`beam_transport_handshake_errors_total{transport="tcp",reason="timeout"} 2`,
		`beam_transport_handshake_errors_total{transport="tcp",reason="tls"} 0`,
		`beam_transport_handshake_errors_total{transport="tcp",reason="protocol"} 0`,
		`beam_transport_handshake_errors_total{transport="tcp",reason="other"} 1`,
		`beam_transport_handshake_errors_total{transport="quic",reason="timeout"} 0`,
		`beam_transport_handshake_errors_total{transport="quic",reason="tls"} 1`,
		`beam_transport_handshake_errors_total{transport="quic",reason="protocol"} 1`,
		`beam_transport_handshake_errors_total{transport="quic",reason="other"} 0`,
		`beam_transport_session_closes_total{transport="tcp",reason="normal"} 1`,
		`beam_transport_session_closes_total{transport="tcp",reason="shutdown"} 1`,
		`beam_transport_session_closes_total{transport="tcp",reason="idle"} 0`,
		`beam_transport_session_closes_total{transport="tcp",reason="protocol"} 0`,
		`beam_transport_session_closes_total{transport="tcp",reason="network"} 0`,
		`beam_transport_session_closes_total{transport="tcp",reason="other"} 0`,
		`beam_transport_session_closes_total{transport="quic",reason="normal"} 0`,
		`beam_transport_session_closes_total{transport="quic",reason="shutdown"} 0`,
		`beam_transport_session_closes_total{transport="quic",reason="idle"} 1`,
		`beam_transport_session_closes_total{transport="quic",reason="protocol"} 1`,
		`beam_transport_session_closes_total{transport="quic",reason="network"} 1`,
		`beam_transport_session_closes_total{transport="quic",reason="other"} 1`,
		`beam_transport_stream_open_errors_total{transport="tcp",reason="timeout"} 1`,
		`beam_transport_stream_open_errors_total{transport="tcp",reason="closed"} 2`,
		`beam_transport_stream_open_errors_total{transport="tcp",reason="other"} 0`,
		`beam_transport_stream_open_errors_total{transport="quic",reason="timeout"} 0`,
		`beam_transport_stream_open_errors_total{transport="quic",reason="closed"} 0`,
		`beam_transport_stream_open_errors_total{transport="quic",reason="other"} 1`,
		`beam_transport_capacity_rejections_total{scope="tls_handshake"} 1`,
		`beam_transport_capacity_rejections_total{scope="preauth_session"} 1`,
		`beam_transport_capacity_rejections_total{scope="authenticated_session"} 1`,
		`beam_transport_capacity_rejections_total{scope="session_stream"} 1`,
		`beam_transport_capacity_rejections_total{scope="global_stream"} 1`,
		`beam_transport_stream_capacity{scope="session"} 64`,
		`beam_transport_stream_capacity{scope="global"} 128`,
		`beam_yamux_stream_window_bytes 4194304`,
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("transport metric series mismatch\n--- got ---\n%s\n--- want ---\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
