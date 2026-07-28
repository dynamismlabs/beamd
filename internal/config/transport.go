package config

import (
	"fmt"
	"os"
)

const (
	TransportTCP  = "tcp"
	TransportAuto = "auto"
	TransportQUIC = "quic"

	TransportEnvVar = "BEAMD_TRANSPORT"

	AgentMaxStreams            int64 = 64
	MaxAgentYamuxExposureBytes int64 = 1 << 30
)

// ResolveTransport returns the effective client transport without mutating the
// profile/account value. BEAMD_TRANSPORT wins when present; otherwise an empty
// configured value uses the shipped tcp default. A present-empty environment
// value is invalid rather than another spelling of "use the default".
func ResolveTransport(configured string) (string, error) {
	effective := configured
	source := "transport"
	if v, ok := os.LookupEnv(TransportEnvVar); ok {
		if v == "" {
			return "", fmt.Errorf("%s is present but empty", TransportEnvVar)
		}
		effective = v
		source = TransportEnvVar
	}
	if effective == "" {
		effective = TransportTCP
	}
	switch effective {
	case TransportTCP, TransportAuto, TransportQUIC:
		return effective, nil
	default:
		return "", fmt.Errorf("%s=%q is not supported (use %q, %q, or %q)",
			source, effective, TransportTCP, TransportAuto, TransportQUIC)
	}
}

// ResolveClientTransport applies the product-specific credential default before
// using the global transport resolver: an exact hosted session with no explicit
// transport uses auto; token, missing, and unknown kinds retain tcp. Explicit
// Transport and BEAMD_TRANSPORT keep their normal precedence.
func ResolveClientTransport(client *Client) (string, error) {
	configured := ""
	if client != nil {
		configured = client.Transport
		if configured == "" && client.Kind == "session" {
			configured = TransportAuto
		}
	}
	return ResolveTransport(configured)
}

// ValidateAgentYamuxWindow validates the resolved process-wide yamux window
// against both its per-stream bounds and the agent's fixed 64-stream handler
// ceiling. The product is receive-flow-control exposure, not preallocated
// resident memory.
func ValidateAgentYamuxWindow(windowBytes int64) error {
	if err := validateYamuxWindow(windowBytes); err != nil {
		return err
	}
	exposure := windowBytes * AgentMaxStreams
	if exposure > MaxAgentYamuxExposureBytes {
		return fmt.Errorf(
			"agent yamux window exposure exceeds maximum: %s=%d * max_streams=%d = %d bytes, maximum %d bytes",
			YamuxWindowEnvVar, windowBytes, AgentMaxStreams, exposure,
			MaxAgentYamuxExposureBytes,
		)
	}
	return nil
}
