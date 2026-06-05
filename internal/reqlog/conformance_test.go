package reqlog

import (
	"reflect"
	"testing"

	"github.com/dynamismlabs/beamd/internal/beamdapi"
)

// TestConformance guards the hand-written wire structs against the OpenAPI-
// generated types by comparing JSON field-name sets (omitempty stripped). Names
// must match exactly; types/optionality are manual care (request-events-spec §6).
func TestConformance(t *testing.T) {
	cases := []struct {
		name      string
		ours, gen any
	}{
		{"RequestEvent", RequestEvent{}, beamdapi.RequestEvent{}},
	}
	for _, c := range cases {
		ours, gen := beamdapi.JSONFields(c.ours), beamdapi.JSONFields(c.gen)
		if !reflect.DeepEqual(ours, gen) {
			t.Errorf("%s JSON field drift:\n  ours=%v\n  spec=%v", c.name, ours, gen)
		}
	}
}
