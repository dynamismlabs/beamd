package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
)

// TestE2E_ScopeRequest drives a requested scope through the real edge + auth.
// The test edge is MemoryStore-backed (token → one slug) — i.e. a single-scope
// OSS/API-key credential: an empty or matching scope is honored, a different
// scope is rejected at hello so a tunnel is never misrouted.
func TestE2E_ScopeRequest(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	opts := func(scope string) client.Options {
		return client.Options{
			HeartbeatInterval:  200 * time.Millisecond,
			RegisterTimeout:    2 * time.Second,
			InsecureSkipVerify: true,
			Scope:              scope,
		}
	}

	// Empty scope → the token's slug (unchanged default behavior).
	if c := connectClientWithOpts(t, edgeAddr, "T1", opts("")); c.Slug() != "turing" {
		t.Errorf("empty scope: slug = %q, want turing", c.Slug())
	}
	// Matching scope → allowed, same slug.
	if c := connectClientWithOpts(t, edgeAddr, "T1", opts("turing")); c.Slug() != "turing" {
		t.Errorf("matching scope: slug = %q, want turing", c.Slug())
	}

	// A different scope → rejected at hello.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Connect(ctx, edgeAddr, "T1", opts("elsewhere")); err == nil {
		t.Fatal("connect with a non-matching scope should be rejected")
	} else if !strings.Contains(err.Error(), "bad_token") {
		t.Errorf("reject error = %q, want it to mention bad_token", err)
	}
}
