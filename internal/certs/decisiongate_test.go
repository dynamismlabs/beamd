package certs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Repeat handshakes for the same verified SNI must collapse to ONE outbound
// resolve-host call within the cache TTL — otherwise a peer spraying SNIs
// amplifies ClientHellos into control-plane load.
func TestDecisionGate_CachesPositive(t *testing.T) {
	var calls int
	g := newDecisionGate(func(context.Context, string) error { calls++; return nil })
	for i := 0; i < 3; i++ {
		if err := g.Decide(context.Background(), "api.acme.com"); err != nil {
			t.Fatalf("Decide: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("underlying called %d times, want 1 (cached)", calls)
	}
}

// A denied (unverified) name is negative-cached too.
func TestDecisionGate_CachesNegative(t *testing.T) {
	var calls int
	g := newDecisionGate(func(context.Context, string) error { calls++; return fmt.Errorf("unverified") })
	for i := 0; i < 3; i++ {
		if err := g.Decide(context.Background(), "evil.example"); err == nil {
			t.Fatal("unverified name should be denied")
		}
	}
	if calls != 1 {
		t.Errorf("underlying called %d times, want 1 (negative-cached)", calls)
	}
}

// authorizeName runs a Decide that authorizes the name (populating the verdict
// cache), as a real handshake would before an issuance attempt. recordFailure
// only cools down names that were authorized, so tests must authorize first.
func authorizeName(t *testing.T, g *decisionGate, name string) {
	t.Helper()
	if err := g.Decide(context.Background(), name); err != nil {
		t.Fatalf("authorize %q: %v", name, err)
	}
}

// After a genuine (authorized-then-failed) issuance the name is denied for the
// cooldown WITHOUT consulting the underlying authorizer — stops the ACME storm.
func TestDecisionGate_CooldownAfterFailure(t *testing.T) {
	var calls int
	g := newDecisionGate(func(context.Context, string) error { calls++; return nil })
	authorizeName(t, g, "api.acme.com")
	g.recordFailure("api.acme.com")

	calls = 0 // the cooldown must short-circuit the NEXT Decide before the authorizer
	if err := g.Decide(context.Background(), "api.acme.com"); err == nil {
		t.Error("name in cooldown must be denied")
	}
	if calls != 0 {
		t.Errorf("cooldown must short-circuit before the underlying call, got %d calls", calls)
	}
}

// A second failure during an active cooldown must NOT push the retry window
// out — otherwise continuous traffic to a failing domain starves its reissue
// forever.
func TestDecisionGate_CooldownDoesNotExtend(t *testing.T) {
	g := newDecisionGate(func(context.Context, string) error { return nil })
	authorizeName(t, g, "api.acme.com")
	g.recordFailure("api.acme.com")
	g.mu.Lock()
	first := g.lastFail["api.acme.com"]
	g.mu.Unlock()

	g.recordFailure("api.acme.com") // within cooldown → must be a no-op
	g.mu.Lock()
	second := g.lastFail["api.acme.com"]
	g.mu.Unlock()

	if !first.Equal(second) {
		t.Errorf("recordFailure extended an active cooldown: %v -> %v", first, second)
	}
}

// A successful issuance clears the cooldown.
func TestDecisionGate_SuccessClearsCooldown(t *testing.T) {
	g := newDecisionGate(func(context.Context, string) error { return nil })
	authorizeName(t, g, "api.acme.com")
	g.recordFailure("api.acme.com")
	g.recordSuccess("api.acme.com")

	if err := g.Decide(context.Background(), "api.acme.com"); err != nil {
		t.Errorf("cooldown should be cleared after success, got %v", err)
	}
}

// A DENIED (unverified) name must NOT enter the cooldown map. GetCertificate
// calls recordFailure on any on-demand error including denials, so this guard
// is what stops an attacker from growing lastFail via an SNI spray.
func TestDecisionGate_DeniedNameNotCooledDown(t *testing.T) {
	g := newDecisionGate(func(context.Context, string) error { return fmt.Errorf("unverified") })
	if err := g.Decide(context.Background(), "evil.example"); err == nil {
		t.Fatal("unverified name should be denied")
	}
	g.recordFailure("evil.example") // as GetCertificate does, on the denial

	g.mu.Lock()
	_, cooling := g.lastFail["evil.example"]
	n := len(g.lastFail)
	g.mu.Unlock()
	if cooling || n != 0 {
		t.Errorf("a denied name must not enter the cooldown map (lastFail size=%d)", n)
	}
}

// lastFail must stay bounded under a spray of DISTINCT denied SNIs — the
// persistent-DoS vector the authorized-only guard closes. verdicts is capped
// by eviction; lastFail must simply never accept a denied name.
func TestDecisionGate_LastFailBoundedUnderDeniedSpray(t *testing.T) {
	g := newDecisionGate(func(context.Context, string) error { return fmt.Errorf("unverified") })
	for i := 0; i < gateMaxEntries*2; i++ {
		name := fmt.Sprintf("a%d.evil.example", i)
		g.Decide(context.Background(), name)
		g.recordFailure(name) // as GetCertificate would, on each denial
	}
	g.mu.Lock()
	n := len(g.lastFail)
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("denied spray grew lastFail to %d entries — unbounded DoS vector", n)
	}
}

// The verdict cache stays bounded no matter how many distinct names are sprayed.
func TestDecisionGate_CacheStaysBounded(t *testing.T) {
	g := newDecisionGate(func(context.Context, string) error { return fmt.Errorf("unverified") })
	for i := 0; i < gateMaxEntries*2; i++ {
		g.Decide(context.Background(), fmt.Sprintf("h%d.example", i))
	}
	g.mu.Lock()
	n := len(g.verdicts)
	g.mu.Unlock()
	if n > gateMaxEntries {
		t.Errorf("verdict cache grew to %d, cap is %d", n, gateMaxEntries)
	}
}

// A cached verdict must EXPIRE so the gate re-consults the authorizer. Both
// directions are security-relevant: an expiring POSITIVE verdict means a domain
// flipped to delegated/de-verified stops being served a stale "allow" within
// posTTL; an expiring NEGATIVE verdict means a fixed domain isn't stuck denied.
// Neither was previously tested.
func TestDecisionGate_VerdictExpiresAndReauthorizes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		authErr error
		expire  func(*decisionGate)
	}{
		{"positive", nil, func(g *decisionGate) { g.posTTL = -1 }}, // -1ns → already past
		{"negative", fmt.Errorf("unverified"), func(g *decisionGate) { g.negTTL = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			g := newDecisionGate(func(context.Context, string) error { calls++; return tc.authErr })
			tc.expire(g)
			for i := 0; i < 3; i++ {
				_ = g.Decide(context.Background(), "api.acme.com")
			}
			if calls != 3 {
				t.Errorf("an expired verdict must re-consult the authorizer each handshake, got %d calls", calls)
			}
		})
	}
}

// The gate sits on the TLS-handshake hot path (highly concurrent) and Decide
// deliberately drops the lock around the network call, then re-acquires — the
// shape where a future edit reintroduces a race. Drive every entry point from
// many goroutines under -race; the maps must stay bounded, and — the security
// invariant — only authorized names may ever enter lastFail.
func TestDecisionGate_ConcurrentAccessIsRaceFree(t *testing.T) {
	g := newDecisionGate(func(_ context.Context, name string) error {
		if strings.HasPrefix(name, "ok") {
			return nil // only the 8 shared "ok" names authorize
		}
		return fmt.Errorf("unverified")
	})
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				name := fmt.Sprintf("ok%d.example", (w+i)%8)
				if i%3 == 0 {
					name = fmt.Sprintf("bad%d.example", w*200+i) // distinct denied spray
				}
				_ = g.Decide(context.Background(), name)
				g.recordFailure(name)
				if i%50 == 0 {
					g.recordSuccess(name)
				}
			}
		}(w)
	}
	wg.Wait()

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.verdicts) > gateMaxEntries {
		t.Errorf("verdicts unbounded: %d > cap %d", len(g.verdicts), gateMaxEntries)
	}
	if len(g.lastFail) > 8 {
		t.Errorf("lastFail = %d; only the 8 authorized names may ever cool down (denials must never enter)", len(g.lastFail))
	}
}

// Once the cooldown window elapses, Decide must STOP denying so a fixed domain
// can retry — the "retry once per window" property that not-extending exists
// for. (CooldownDoesNotExtend checks the timestamp isn't pushed out; this checks
// the elapsed window actually re-allows.)
func TestDecisionGate_CooldownElapsesAllowsRetry(t *testing.T) {
	g := newDecisionGate(func(context.Context, string) error { return nil })
	g.failCooldown = -1 // any recorded failure is already "elapsed"
	authorizeName(t, g, "api.acme.com")
	g.recordFailure("api.acme.com")
	if err := g.Decide(context.Background(), "api.acme.com"); err != nil {
		t.Errorf("after the cooldown elapses the name must be allowed again, got %v", err)
	}
}
