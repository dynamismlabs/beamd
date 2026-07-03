package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request event must record what the client sent WITHOUT ever placing a raw
// control byte on the wire. net/http decodes a percent-encoded control char
// (e.g. a scanner's "%00") into a raw 0x00 in r.URL.Path; Postgres text can't
// store 0x00, and that one byte wedged reqlog ingest for the whole batch
// (ops/incident-reqlog-nul-wedge.md). requestTarget captures the ENCODED path,
// so "%00" stays "%00" and the query string (which carries secrets) is dropped.
func TestRequestTargetKeepsControlBytesEncoded(t *testing.T) {
	cases := []struct {
		name, target, want string
	}{
		{"nul in path", "/assets/%00/../../etc/passwd", "/assets/%00/../../etc/passwd"},
		{"assorted control bytes", "/a%01b%0dc%0a", "/a%01b%0dc%0a"},
		{"del byte", "/x%7Fy", "/x%7Fy"},
		{"query string dropped", "/probe?token=secret%00x", "/probe"},
		{"plain path unchanged", "/api/v1/things", "/api/v1/things"},
		{"encoded slash preserved", "/a%2Fb", "/a%2Fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)

			// Sanity: net/http really does decode "%00" into a raw byte on the
			// field we used to capture — the exact trap this fix avoids.
			if tc.name == "nul in path" && !strings.ContainsRune(r.URL.Path, 0) {
				t.Fatalf("precondition: expected r.URL.Path to hold a raw NUL for %q", tc.target)
			}

			got := requestTarget(r)
			if got != tc.want {
				t.Errorf("requestTarget(%q) = %q, want %q", tc.target, got, tc.want)
			}
			if i := strings.IndexFunc(got, func(ru rune) bool { return ru < 0x20 || ru == 0x7f }); i >= 0 {
				t.Errorf("requestTarget(%q) = %q holds a raw control byte at %d", tc.target, got, i)
			}
		})
	}
}
