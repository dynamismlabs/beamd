package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/dynamismlabs/beamd/internal/tunnel"
)

// yamux per-stream receive-window bounds (transport-performance-spec §8.1). The
// default lifts yamux's 256 KiB library ceiling to 4 MiB; the range is what the
// resolver accepts. Values are base-10 bytes. The default is sourced from the
// transport adapter so there is one numeric truth. The 16 MiB max bounds any
// single stream's exposure; Part B admission limits bound aggregate exposure.
const (
	DefaultYamuxStreamWindowBytes int64 = tunnel.DefaultStreamWindow // 4 MiB
	MinYamuxStreamWindowBytes     int64 = 256 << 10                  // 256 KiB (yamux's own floor)
	MaxYamuxStreamWindowBytes     int64 = 16 << 20                   // 16 MiB
)

// YamuxWindowEnvVar is the sole external knob for the Part A stream window. It is
// process-wide, not an identity/account property, and is never persisted to YAML,
// profiles, or accounts (§8.1 / §11.1).
const YamuxWindowEnvVar = "BEAMD_YAMUX_STREAM_WINDOW_BYTES"

// validateYamuxWindow rejects a window outside the accepted inclusive range.
func validateYamuxWindow(n int64) error {
	if n < MinYamuxStreamWindowBytes || n > MaxYamuxStreamWindowBytes {
		return fmt.Errorf("%s: %d is out of range [%d, %d]",
			YamuxWindowEnvVar, n, MinYamuxStreamWindowBytes, MaxYamuxStreamWindowBytes)
	}
	return nil
}

// ResolveYamuxWindow reads BEAMD_YAMUX_STREAM_WINDOW_BYTES once and returns the
// per-stream receive window in bytes. An ABSENT variable yields the 4 MiB
// default. A PRESENT value that is empty, non-integer, negative, overflowing, or
// outside [256 KiB, 16 MiB] is an error — the caller must fail startup rather
// than silently retain the default (§8.1). Call this once per process at edge or
// agent startup and thread the value into internal/tunnel; never read the
// environment per session.
func ResolveYamuxWindow() (int64, error) {
	v, ok := os.LookupEnv(YamuxWindowEnvVar)
	if !ok {
		return DefaultYamuxStreamWindowBytes, nil
	}
	// A present variable must be a valid in-range value; "" (present-empty),
	// "0x10", trailing junk, and overflow all fail here via ParseInt.
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: not a base-10 integer in bytes: %w", YamuxWindowEnvVar, v, err)
	}
	if err := validateYamuxWindow(n); err != nil {
		return 0, err
	}
	return n, nil
}
