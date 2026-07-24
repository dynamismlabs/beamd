package config

import (
	"os"
	"testing"
)

// clearWindowEnv makes BEAMD_YAMUX_STREAM_WINDOW_BYTES absent for the test body
// while registering restoration of the original value via t.Setenv's cleanup.
func clearWindowEnv(t *testing.T) {
	t.Helper()
	t.Setenv(YamuxWindowEnvVar, "placeholder") // registers cleanup to restore the original
	if err := os.Unsetenv(YamuxWindowEnvVar); err != nil {
		t.Fatalf("unset %s: %v", YamuxWindowEnvVar, err)
	}
}

func TestResolveYamuxWindow_AbsentUsesDefault(t *testing.T) {
	clearWindowEnv(t)
	if DefaultYamuxStreamWindowBytes != 4194304 {
		t.Fatalf("DefaultYamuxStreamWindowBytes = %d, want 4194304 (4 MiB)", DefaultYamuxStreamWindowBytes)
	}
	got, err := ResolveYamuxWindow()
	if err != nil {
		t.Fatalf("absent env: unexpected error: %v", err)
	}
	if got != DefaultYamuxStreamWindowBytes {
		t.Errorf("absent env resolved to %d, want default %d", got, DefaultYamuxStreamWindowBytes)
	}
}

func TestResolveYamuxWindow_ValidValues(t *testing.T) {
	// Assert the EXACT parsed value, so a resolver that returned 4 MiB for every
	// input (ignoring the env) would fail here.
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"262144", 262144},     // exact min
		{"16777216", 16777216}, // exact max
		{"8388608", 8388608},   // 8 MiB
		{"4194304", 4194304},   // 4 MiB (equals the default, but must still be parsed)
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Setenv(YamuxWindowEnvVar, tc.in)
			got, err := ResolveYamuxWindow()
			if err != nil {
				t.Fatalf("value %s: unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("value %s resolved to %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveYamuxWindow_PresentInvalidFails(t *testing.T) {
	// Every present-but-invalid form must be a startup error, never defaulted.
	cases := map[string]string{
		"empty":         "",
		"non-integer":   "four-mib",
		"trailing-junk": "4194304x",
		"hex":           "0x400000",
		"negative":      "-1",
		"zero":          "0",
		"overflow":      "99999999999999999999999999",
		"below-min":     "262143",
		"above-max":     "16777217",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(YamuxWindowEnvVar, val)
			if got, err := ResolveYamuxWindow(); err == nil {
				t.Fatalf("value %q (%s): expected an error, got %d", val, name, got)
			}
		})
	}
}
