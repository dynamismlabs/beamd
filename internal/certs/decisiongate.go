package certs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// decisionGate wraps the on-demand issuance authorizer (the resolve-host check)
// with two protections certmagic does not provide:
//
//   - A TTL verdict cache. certmagic calls the DecisionFunc on every handshake
//     for a novel SNI, so without this an unauthenticated peer spraying distinct
//     SNIs turns cheap TLS ClientHellos into a flood of outbound resolve-host
//     calls against the control plane. (SEC-4)
//   - A short per-name cooldown after an issuance FAILURE. certmagic otherwise
//     re-attempts ACME on every handshake for a verified-but-failing domain,
//     burning the ACME account's shared rate budget and starving the apex/
//     wildcard renewals. (SEC-3)
//
// It owns all of its state (its own mutex, not the MagicManager's) so it can be
// constructed before the certmagic config that closes over it.
type decisionGate struct {
	authorize func(ctx context.Context, name string) error // underlying resolve-host check

	posTTL       time.Duration
	negTTL       time.Duration
	failCooldown time.Duration
	maxEntries   int

	mu       sync.Mutex
	verdicts map[string]gateVerdict
	lastFail map[string]time.Time
}

// gateVerdict is a cached authorization result. A nil err means the name was
// authorized for issuance; a non-nil err means it was denied.
type gateVerdict struct {
	err     error
	expires time.Time
}

const (
	gateDefaultPosTTL       = 60 * time.Second // re-verify a live custom domain at most once a minute
	gateDefaultNegTTL       = 30 * time.Second // a spray of the same bogus SNI collapses to 1 call / 30s
	gateDefaultFailCooldown = 2 * time.Minute  // short: bounds the delay before a fixed domain reissues
	gateMaxEntries          = 4096
)

func newDecisionGate(authorize func(ctx context.Context, name string) error) *decisionGate {
	return &decisionGate{
		authorize:    authorize,
		posTTL:       gateDefaultPosTTL,
		negTTL:       gateDefaultNegTTL,
		failCooldown: gateDefaultFailCooldown,
		maxEntries:   gateMaxEntries,
		verdicts:     make(map[string]gateVerdict),
		lastFail:     make(map[string]time.Time),
	}
}

// Decide is the certmagic DecisionFunc. It denies during a post-failure
// cooldown, otherwise returns a cached verdict, otherwise consults the
// underlying authorizer and caches the outcome.
func (g *decisionGate) Decide(ctx context.Context, name string) error {
	g.mu.Lock()
	if t, ok := g.lastFail[name]; ok && time.Since(t) < g.failCooldown {
		g.mu.Unlock()
		return fmt.Errorf("on-demand issuance for %q is cooling down after a recent failure", name)
	}
	if v, ok := g.verdicts[name]; ok && time.Now().Before(v.expires) {
		err := v.err
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()

	// Cache miss: consult the control plane (network call, outside the lock).
	err := g.authorize(ctx, name)

	g.mu.Lock()
	ttl := g.posTTL
	if err != nil {
		ttl = g.negTTL
	}
	if len(g.verdicts) >= g.maxEntries {
		g.evictLocked()
	}
	g.verdicts[name] = gateVerdict{err: err, expires: time.Now().Add(ttl)}
	g.mu.Unlock()
	return err
}

// recordFailure starts a cooldown after a failed on-demand issuance for name.
//
// It records ONLY names the gate actually AUTHORIZED (a nil verdict) — i.e. a
// genuine ACME failure. A DENIED name must never be recorded, for two reasons:
// (1) a denial never reached an ACME order (the DecisionFunc gates it) and its
// repeat lookups are already collapsed by the negative verdict cache, so a
// cooldown buys nothing; (2) more importantly, GetCertificate calls this on any
// on-demand error including denials, so recording denials would let an
// unauthenticated peer spraying distinct off-base SNIs grow lastFail without
// bound — a persistent version of the very DoS SEC-4 closes. Only
// control-plane-verified domains are ever authorized, so keying off the verdict
// keeps lastFail operator-bounded and attacker-proof.
//
// It does NOT extend an already-active cooldown: a verified-but-failing domain
// then retries once per window even under continuous traffic, instead of the
// window being pushed out forever by the traffic hitting it.
func (g *decisionGate) recordFailure(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.verdicts[name]; !ok || v.err != nil {
		return // never authorized (or evicted) → not a genuine issuance failure
	}
	if t, ok := g.lastFail[name]; ok && time.Since(t) < g.failCooldown {
		return // don't extend an active cooldown
	}
	// Reclaim entries whose cooldown has elapsed (inert — Decide ignores them),
	// so lastFail stays ≈ the set of currently-cooling domains.
	for k, t := range g.lastFail {
		if time.Since(t) >= g.failCooldown {
			delete(g.lastFail, k)
		}
	}
	g.lastFail[name] = time.Now()
}

// recordSuccess clears any cooldown for name after a successful issuance.
func (g *decisionGate) recordSuccess(name string) {
	g.mu.Lock()
	delete(g.lastFail, name)
	g.mu.Unlock()
}

// evictLocked frees room in a full verdict cache: drop expired entries first,
// then arbitrary entries down to half capacity. Caller holds g.mu. A dropped
// entry only costs one re-authorization.
func (g *decisionGate) evictLocked() {
	now := time.Now()
	for k, v := range g.verdicts {
		if now.After(v.expires) {
			delete(g.verdicts, k)
		}
	}
	for k := range g.verdicts {
		if len(g.verdicts) <= g.maxEntries/2 {
			break
		}
		delete(g.verdicts, k)
	}
}
