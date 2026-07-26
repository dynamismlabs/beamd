package config

import (
	"os"
	"strings"
	"testing"
)

func clearTransportEnv(t *testing.T) {
	t.Helper()
	t.Setenv(TransportEnvVar, "placeholder")
	if err := os.Unsetenv(TransportEnvVar); err != nil {
		t.Fatalf("unset %s: %v", TransportEnvVar, err)
	}
}

func TestResolveTransport_DefaultAndConfiguredModes(t *testing.T) {
	clearTransportEnv(t)
	cases := map[string]string{
		"":            TransportTCP,
		TransportTCP:  TransportTCP,
		TransportAuto: TransportAuto,
		TransportQUIC: TransportQUIC,
	}
	for configured, want := range cases {
		got, err := ResolveTransport(configured)
		if err != nil {
			t.Fatalf("ResolveTransport(%q): %v", configured, err)
		}
		if got != want {
			t.Errorf("ResolveTransport(%q) = %q, want %q", configured, got, want)
		}
	}
}

func TestResolveTransport_EnvironmentPrecedence(t *testing.T) {
	t.Setenv(TransportEnvVar, TransportQUIC)
	got, err := ResolveTransport(TransportTCP)
	if err != nil {
		t.Fatalf("ResolveTransport: %v", err)
	}
	if got != TransportQUIC {
		t.Errorf("effective transport = %q, want environment override quic", got)
	}
}

func TestResolveTransport_InvalidValuesFail(t *testing.T) {
	clearTransportEnv(t)
	if _, err := ResolveTransport("udp"); err == nil {
		t.Fatal("unsupported configured transport should fail")
	}

	for name, value := range map[string]string{
		"present-empty": "",
		"unsupported":   "udp",
		"wrong-case":    "QUIC",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(TransportEnvVar, value)
			_, err := ResolveTransport(TransportTCP)
			if err == nil {
				t.Fatalf("ResolveTransport error = %v, want strict invalid-env failure", err)
			}
			if !strings.Contains(err.Error(), TransportEnvVar) {
				t.Fatalf("ResolveTransport error = %v, want %s source", err, TransportEnvVar)
			}
		})
	}
}

func TestValidateAgentYamuxWindow(t *testing.T) {
	for _, window := range []int64{
		MinYamuxStreamWindowBytes,
		DefaultYamuxStreamWindowBytes,
		MaxYamuxStreamWindowBytes,
	} {
		if err := ValidateAgentYamuxWindow(window); err != nil {
			t.Errorf("window %d should pass: %v", window, err)
		}
	}
	if got := MaxYamuxStreamWindowBytes * AgentMaxStreams; got != MaxAgentYamuxExposureBytes {
		t.Fatalf("documented maximum product = %d, want %d", got, MaxAgentYamuxExposureBytes)
	}
	if err := ValidateAgentYamuxWindow(MinYamuxStreamWindowBytes - 1); err == nil {
		t.Fatal("below-minimum window should fail")
	}
}
