package dns

import (
	"context"
	"testing"

	"github.com/libdns/libdns"
)

func TestOpen(t *testing.T) {
	if _, err := Open("stub", ""); err != nil {
		t.Errorf("Open stub: %v", err)
	}
	if _, err := Open("cloudflare", "tok"); err != nil {
		t.Errorf("Open cloudflare: %v", err)
	}
	if _, err := Open("cloudflare", ""); err == nil {
		t.Error("Open cloudflare without creds should fail")
	}
	if _, err := Open("totally-fake-provider", ""); err == nil {
		t.Error("unsupported provider should error")
	}
}

func TestProvisionSlug_StubWritesExpectedRecords(t *testing.T) {
	p := NewStubProvider()
	ctx := context.Background()

	if err := ProvisionSlug(ctx, p, "beam.example.com", "trey", "1.2.3.4", "2001:db8::1"); err != nil {
		t.Fatalf("ProvisionSlug: %v", err)
	}

	got := p.Records("beam.example.com")
	if len(got) != 4 {
		t.Fatalf("got %d records, want 4 (apex + wildcard for v4 + v6)", len(got))
	}

	type key struct{ name, typ string }
	want := map[key]bool{
		{"trey", "A"}:      false,
		{"*.trey", "A"}:    false,
		{"trey", "AAAA"}:   false,
		{"*.trey", "AAAA"}: false,
	}
	for _, r := range got {
		rr := r.RR()
		k := key{rr.Name, rr.Type}
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected record %+v", rr)
			continue
		}
		want[k] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing record name=%s type=%s", k.name, k.typ)
		}
	}
}

func TestProvisionSlug_Idempotent(t *testing.T) {
	p := NewStubProvider()
	ctx := context.Background()

	if err := ProvisionSlug(ctx, p, "beam.example.com", "trey", "1.2.3.4", ""); err != nil {
		t.Fatal(err)
	}
	first := len(p.Records("beam.example.com"))
	if err := ProvisionSlug(ctx, p, "beam.example.com", "trey", "1.2.3.4", ""); err != nil {
		t.Fatal(err)
	}
	second := len(p.Records("beam.example.com"))
	if first != second {
		t.Errorf("idempotent provision changed record count: %d → %d", first, second)
	}
}

func TestStubProvider_AppendThenDelete(t *testing.T) {
	p := NewStubProvider()
	ctx := context.Background()

	a := libdns.TXT{Name: "_acme-challenge.trey", Text: "challenge-1"}
	if _, err := p.AppendRecords(ctx, "beam.example.com", []libdns.Record{a}); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Records("beam.example.com")); got != 1 {
		t.Fatalf("after append got %d records", got)
	}
	if _, err := p.DeleteRecords(ctx, "beam.example.com", []libdns.Record{a}); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Records("beam.example.com")); got != 0 {
		t.Fatalf("after delete got %d records", got)
	}
}
