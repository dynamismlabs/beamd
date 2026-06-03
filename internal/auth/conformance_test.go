package auth

import (
	"reflect"
	"testing"

	"github.com/dynamismlabs/beamd/internal/beamdapi"
)

// TestConformance_VerifyToken guards the verify-token response the edge parses
// against the shared OpenAPI spec (internal/beamdapi). See the device-code
// conformance test for the regen workflow.
func TestConformance_VerifyToken(t *testing.T) {
	ours := beamdapi.JSONFields(verifyTokenResponse{})
	gen := beamdapi.JSONFields(beamdapi.VerifyTokenResponse{})
	if !reflect.DeepEqual(ours, gen) {
		t.Errorf("verify-token JSON field drift:\n  ours=%v\n  spec=%v", ours, gen)
	}
}
