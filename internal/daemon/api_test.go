package daemon

import (
	"encoding/json"
	"testing"
)

func TestHealthzResponseTransportJSONContract(t *testing.T) {
	tests := []struct {
		name string
		in   HealthzResponse
		want string
	}{
		{
			name: "connected with diagnostics",
			in: HealthzResponse{
				Status:              "ok",
				Slug:                "acme",
				Healthy:             true,
				Transport:           "quic",
				ConfiguredTransport: "auto",
				FallbackCount:       2,
				LastFallbackReason:  "timeout",
				ReconnectCount:      3,
				LastCloseReason:     "network",
			},
			want: `{"status":"ok","slug":"acme","healthy":true,"transport":"quic","configuredTransport":"auto","fallbackCount":2,"lastFallbackReason":"timeout","reconnectCount":3,"lastCloseReason":"network"}`,
		},
		{
			name: "disconnected omits optional values",
			in: HealthzResponse{
				Status:              "ok",
				Slug:                "acme",
				ConfiguredTransport: "tcp",
			},
			want: `{"status":"ok","slug":"acme","healthy":false,"configuredTransport":"tcp","fallbackCount":0,"reconnectCount":0}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("JSON = %s, want %s", got, test.want)
			}
		})
	}
}
