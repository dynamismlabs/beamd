package edge

import "testing"

func TestEffectiveSessionStreamCapacity(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		advertised int
		want       int
	}{
		{name: "old agent uses legacy ceiling", configured: 128, advertised: 0, want: 64},
		{name: "negative uses legacy ceiling", configured: 128, advertised: -1, want: 64},
		{name: "current agent uses configured ceiling", configured: 128, advertised: 128, want: 128},
		{name: "lower client ceiling is honored", configured: 128, advertised: 96, want: 96},
		{name: "client cannot raise edge ceiling", configured: 128, advertised: 1024, want: 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveSessionStreamCapacity(tt.configured, tt.advertised); got != tt.want {
				t.Fatalf("effectiveSessionStreamCapacity(%d, %d) = %d, want %d",
					tt.configured, tt.advertised, got, tt.want)
			}
		})
	}
}
