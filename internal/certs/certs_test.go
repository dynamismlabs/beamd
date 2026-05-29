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
		{"api.turing.beam.example.com", "turing", true},
		{"web.hopper.beam.example.com", "hopper", true},
		{"turing.beam.example.com", "", false}, // no app label
		{"beam.example.com", "", false},      // apex
		{"api.example.org", "", false},          // wrong base
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := extractSlug(c.sni, base)
		if got != c.want || ok != c.ok {
			t.Errorf("extractSlug(%q) = (%q, %v); want (%q, %v)", c.sni, got, ok, c.want, c.ok)
		}
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
