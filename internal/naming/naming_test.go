package naming

import (
	"strings"
	"testing"
)

func TestValidateLabel(t *testing.T) {
	good := []string{"a", "api", "web", "3001", "a-b", "a1", "abc-def-123", strings.Repeat("a", 63)}
	for _, s := range good {
		if err := ValidateLabel(s); err != nil {
			t.Errorf("ValidateLabel(%q) = %v, want nil", s, err)
		}
	}

	bad := []string{"", "A", "API", "Bad_Name", "-api", "api-", "api.web", "a..b", "/api", strings.Repeat("a", 64)}
	for _, s := range bad {
		if err := ValidateLabel(s); err == nil {
			t.Errorf("ValidateLabel(%q) = nil, want error", s)
		}
	}
}

func TestValidateSlug(t *testing.T) {
	// Slugs are hyphen-free (§7); names keep hyphens.
	good := []string{"a", "acme", "acme123", "dynamism", strings.Repeat("a", 63)}
	for _, s := range good {
		if err := ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"", "A", "acme-inc", "a-b", "-x", "x-", "ab.cd", strings.Repeat("a", 64)}
	for _, s := range bad {
		if err := ValidateSlug(s); err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want error", s)
		}
	}
}

func TestLabelFromPort(t *testing.T) {
	if got := LabelFromPort(3001); got != "3001" {
		t.Errorf("LabelFromPort(3001) = %q, want 3001", got)
	}
}

func TestHostname(t *testing.T) {
	cases := []struct {
		label, slug, base string
		shape             Shape
		want              string
	}{
		{"api", "acme", "beamd.sh", ShapeHyphen, "api-acme.beamd.sh"},
		{"pr-123-api", "acme", "beamd.sh", ShapeHyphen, "pr-123-api-acme.beamd.sh"},
		{"api", "acme", "beamd.sh", ShapeSubdomain, "api.acme.beamd.sh"},
		{"api", "acme", "beamd.sh", ShapeFlat, "api.beamd.sh"},
		// empty slug always collapses to flat
		{"api", "", "beamd.sh", ShapeHyphen, "api.beamd.sh"},
		{"api", "", "beamd.sh", ShapeSubdomain, "api.beamd.sh"},
	}
	for _, c := range cases {
		if got := Hostname(c.label, c.slug, c.base, c.shape); got != c.want {
			t.Errorf("Hostname(%q,%q,%q,%q) = %q, want %q", c.label, c.slug, c.base, c.shape, got, c.want)
		}
	}
}

// Golden vectors: the host format is rendered (Hostname) and parsed (SlugFromHost)
// here AND in beamd-web/src/lib/tunnel-url.ts. These pin the round-trip so the two
// can't drift (url-model §7, R2). Keep in sync with tunnel-url.test.ts.
func TestSlugFromHostRoundTrip(t *testing.T) {
	type vec struct {
		name, slug string
		shape      Shape
	}
	golden := []vec{
		{"api", "acme", ShapeHyphen},
		{"pr-123-api", "acme", ShapeHyphen}, // compound, hyphenated name
		{"feat-x-web", "dynamism", ShapeHyphen},
		{"api", "acme", ShapeSubdomain},
		{"pr-123-api", "acmeinc", ShapeSubdomain},
	}
	for _, v := range golden {
		host := Hostname(v.name, v.slug, "beamd.sh", v.shape)
		if got := SlugFromHost(host, "beamd.sh", v.shape); got != v.slug {
			t.Errorf("SlugFromHost(%q,%q) = %q, want %q", host, v.shape, got, v.slug)
		}
	}

	// non-base / flat / custom-domain hosts → "" (no slug)
	empties := []struct {
		host  string
		shape Shape
	}{
		{"api.beamd.sh", ShapeFlat},
		{"app.acme.com", ShapeHyphen},      // custom domain
		{"api-acme.other.sh", ShapeHyphen}, // outside base
	}
	for _, e := range empties {
		if got := SlugFromHost(e.host, "beamd.sh", e.shape); got != "" {
			t.Errorf("SlugFromHost(%q,%q) = %q, want \"\"", e.host, e.shape, got)
		}
	}

	// strips a port
	if got := SlugFromHost("api-acme.beamd.sh:443", "beamd.sh", ShapeHyphen); got != "acme" {
		t.Errorf("SlugFromHost with port = %q, want acme", got)
	}
}
