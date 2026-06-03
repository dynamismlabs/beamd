package usage

import (
	"reflect"
	"testing"

	"github.com/dynamismlabs/beamd/internal/beamdapi"
)

// TestConformance_Usage guards the usage report body the reporter sends against
// the shared OpenAPI spec (internal/beamdapi). See the device-code conformance
// test for the regen workflow.
func TestConformance_Usage(t *testing.T) {
	cases := []struct {
		name      string
		ours, gen any
	}{
		{"UsageEvent", usageEvent{}, beamdapi.UsageEvent{}},
		{"UsageRequest", usageReportBody{}, beamdapi.UsageRequest{}},
	}
	for _, c := range cases {
		ours, gen := beamdapi.JSONFields(c.ours), beamdapi.JSONFields(c.gen)
		if !reflect.DeepEqual(ours, gen) {
			t.Errorf("%s JSON field drift:\n  ours=%v\n  spec=%v", c.name, ours, gen)
		}
	}
}
