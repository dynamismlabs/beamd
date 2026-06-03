package devicecode

import (
	"reflect"
	"testing"

	"github.com/dynamismlabs/beamd/internal/beamdapi"
)

// TestConformance_DeviceCode guards the device-code login contract against the
// shared OpenAPI spec (internal/beamdapi). Regenerate beamdapi (`go generate
// ./internal/beamdapi`) when the web app's spec changes; this fails if a field
// is renamed/added/removed out from under the CLI's hand-written structs.
func TestConformance_DeviceCode(t *testing.T) {
	cases := []struct {
		name      string
		ours, gen any
	}{
		{"DeviceTokenResponse", TokenResponse{}, beamdapi.DeviceTokenResponse{}},
		{"ScopeRef", Scope{}, beamdapi.ScopeRef{}},
		{"BeamAuthDiscovery", Discovery{}, beamdapi.BeamAuthDiscovery{}},
		{"DeviceCodeResponse", DeviceCodeResponse{}, beamdapi.DeviceCodeResponse{}},
	}
	for _, c := range cases {
		ours, gen := beamdapi.JSONFields(c.ours), beamdapi.JSONFields(c.gen)
		if !reflect.DeepEqual(ours, gen) {
			t.Errorf("%s JSON field drift:\n  ours=%v\n  spec=%v", c.name, ours, gen)
		}
	}
}
