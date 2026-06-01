package certs

import (
	"crypto/tls"
	"testing"
)

func TestExtractSlug(t *testing.T) {
	base := "beam.example.com"
	cases := []struct {
		sni  string
		want string
		ok   bool
	}{
		{"api.turing.beam.example.com", "turing", true}, // namespaced
		{"web.hopper.beam.example.com", "hopper", true}, // namespaced
		{"hello.beam.example.com", "", true},            // flat: one label → *.<base>
		{"turing.beam.example.com", "", true},           // flat (also a slug's bare apex)
		{"beam.example.com", "", false},                 // apex itself
		{"a.b.turing.beam.example.com", "", false},      // nested too deep
		{"api.example.org", "", false},                  // wrong base
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := extractSlug(c.sni, base)
		if got != c.want || ok != c.ok {
			t.Errorf("extractSlug(%q) = (%q, %v); want (%q, %v)", c.sni, got, ok, c.want, c.ok)
		}
	}
}

func TestCertNamesFor(t *testing.T) {
	base := "beam.example.com"
	if got := certNamesFor("turing", base); len(got) != 2 ||
		got[0] != "*.turing.beam.example.com" || got[1] != "turing.beam.example.com" {
		t.Errorf("namespaced certNamesFor = %v", got)
	}
	if got := certNamesFor("", base); len(got) != 1 || got[0] != "*.beam.example.com" {
		t.Errorf("flat certNamesFor = %v, want [*.beam.example.com]", got)
	}
}

// A flat SNI (<name>.<base>) gets a real per-namespace cert, not the fallback.
func TestSelfSignedManager_FlatIssuesWildcard(t *testing.T) {
	m, err := NewSelfSignedManager("beam.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "hello.beam.example.com"}); err != nil {
		t.Fatal(err)
	}
	if m.IssuanceCount() != 1 {
		t.Errorf("flat SNI should issue a cert; issuance = %d, want 1", m.IssuanceCount())
	}
	// A second flat tunnel reuses the same *.<base> cert — no new issuance.
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "world.beam.example.com"}); err != nil {
		t.Fatal(err)
	}
	if m.IssuanceCount() != 1 {
		t.Errorf("second flat tunnel reused cert? issuance = %d, want 1", m.IssuanceCount())
	}
}

func TestSelfSignedManager_ReuseAndDistinctSlugs(t *testing.T) {
	m, err := NewSelfSignedManager("beam.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// First handshake under slug "turing" → issuance #1.
	c1, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.turing.beam.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if m.IssuanceCount() != 1 {
		t.Errorf("issuance after first slug = %d, want 1", m.IssuanceCount())
	}

	// Different app under same slug → same cert, no new issuance.
	c2, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "web.turing.beam.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if m.IssuanceCount() != 1 {
		t.Errorf("issuance after second app under same slug = %d, want 1", m.IssuanceCount())
	}
	if c1 != c2 {
		t.Errorf("expected same cert pointer for two apps under same slug")
	}

	// Different slug → second issuance.
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.hopper.beam.example.com"}); err != nil {
		t.Fatal(err)
	}
	if m.IssuanceCount() != 2 {
		t.Errorf("issuance after distinct slug = %d, want 2", m.IssuanceCount())
	}
}

func TestSelfSignedManager_FallbackForOffDomain(t *testing.T) {
	m, err := NewSelfSignedManager("beam.example.com")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("GetCertificate(localhost): %v", err)
	}
	if c == nil {
		t.Fatal("expected fallback cert, got nil")
	}
	if m.IssuanceCount() != 0 {
		t.Errorf("fallback should not increment issuance count; got %d", m.IssuanceCount())
	}
}

func TestSelfSignedManager_PreWarm(t *testing.T) {
	m, err := NewSelfSignedManager("beam.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PreWarm("turing"); err != nil {
		t.Fatalf("PreWarm: %v", err)
	}
	if m.IssuanceCount() != 1 {
		t.Errorf("issuance after PreWarm = %d, want 1", m.IssuanceCount())
	}
	// Subsequent handshake for that slug uses the pre-warmed cert.
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.turing.beam.example.com"}); err != nil {
		t.Fatal(err)
	}
	if m.IssuanceCount() != 1 {
		t.Errorf("issuance after handshake-using-prewarmed = %d, want 1", m.IssuanceCount())
	}
}
