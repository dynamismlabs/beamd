package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPStore_ValidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "bad secret", http.StatusUnauthorized)
			return
		}
		var body struct{ Token string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Token == "T1" {
			_ = json.NewEncoder(w).Encode(map[string]string{"slug": "turing"})
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "s3cret")
	slug, ok := s.Resolve("T1", "")
	if !ok || slug != "turing" {
		t.Errorf("Resolve(T1) = (%q, %v); want (turing, true)", slug, ok)
	}
	if slug, ok := s.Resolve("garbage", ""); ok {
		t.Errorf("Resolve(garbage) = (%q, %v); want (_, false)", slug, ok)
	}
}

func TestHTTPStore_BadSecretRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "bad secret", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"slug": "turing"})
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "wrong")
	if _, ok := s.Resolve("T1", ""); ok {
		t.Error("wrong secret should fail Resolve")
	}
}

func TestHTTPStore_CachesPositiveResult(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"slug": "turing"})
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	for i := 0; i < 5; i++ {
		slug, ok := s.Resolve("T1", "")
		if !ok || slug != "turing" {
			t.Fatalf("iter %d: Resolve = (%q, %v)", i, slug, ok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 verify call (cached), got %d", got)
	}
}

func TestHTTPStore_CachesNegativeResult(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	s.SetTTLs(time.Hour, time.Hour) // long enough to verify reuse
	for i := 0; i < 3; i++ {
		if _, ok := s.Resolve("missing", ""); ok {
			t.Fatalf("Resolve(missing) should be false")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 verify call (neg-cached), got %d", got)
	}
}

func TestHTTPStore_TransientErrorNotCached(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	for i := 0; i < 3; i++ {
		if _, ok := s.Resolve("T1", ""); ok {
			t.Fatal("500 should not authorize")
		}
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 verify calls (no caching on 5xx), got %d", got)
	}
}

// A user-session response carries a scope SET: an empty request resolves to
// the first (default/personal) scope, a member scope is allowed, a non-member
// is rejected — and one verify call serves all of them (cached set).
func TestHTTPStore_SessionScopeSet(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": "session",
			"user": "trey@example.com",
			"scopes": []map[string]string{
				{"slug": "trey", "role": "owner"},
				{"slug": "acme", "role": "member"},
			},
		})
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")

	if slug, ok := s.Resolve("SESS", ""); !ok || slug != "trey" {
		t.Errorf("Resolve(SESS, \"\") = (%q, %v); want (trey, true) — default is first scope", slug, ok)
	}
	if slug, ok := s.Resolve("SESS", "acme"); !ok || slug != "acme" {
		t.Errorf("Resolve(SESS, acme) = (%q, %v); want (acme, true)", slug, ok)
	}
	if _, ok := s.Resolve("SESS", "other"); ok {
		t.Error("Resolve(SESS, other) should reject — not a member of that scope")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 verify call for the session (set cached), got %d", got)
	}
}

// A session with NO scopes must reject. This is the edge-side guarantee the
// hosted "verify your email to claim a username" gate relies on: an
// authenticated user who hasn't claimed a scope (no personal workspace) can't
// open tunnels, because verify-token returns an empty scope set.
func TestHTTPStore_SessionWithNoScopesRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": "session", "user": "trey@example.com", "scopes": []map[string]string{},
		})
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	if _, ok := s.Resolve("SESS", ""); ok {
		t.Error("session with empty scopes must reject (no claimed scope → no tunnels)")
	}
	if _, ok := s.Resolve("SESS", "anything"); ok {
		t.Error("session with empty scopes must reject for any requested scope")
	}
}

// A bare {slug} (no kind) stays back-compatible: it's a single-scope key.
func TestHTTPStore_BareSlugIsSingleScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"slug": "turing"})
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	if slug, ok := s.Resolve("T1", ""); !ok || slug != "turing" {
		t.Errorf("bare slug, empty scope = (%q, %v); want (turing, true)", slug, ok)
	}
	if slug, ok := s.Resolve("T1", "turing"); !ok || slug != "turing" {
		t.Errorf("bare slug, matching scope = (%q, %v); want (turing, true)", slug, ok)
	}
	if _, ok := s.Resolve("T1", "elsewhere"); ok {
		t.Error("bare slug, mismatched scope should reject")
	}
}

// The cache is keyed by attacker-supplied tokens pre-auth, so it must stay
// bounded no matter how many distinct tokens get sprayed at the edge.
func TestHTTPStore_CacheStaysBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	// Long TTLs so nothing expires during the test — forcing the hard-cap
	// eviction path, not just the expired sweep.
	s.SetTTLs(time.Hour, time.Hour)

	for i := 0; i < httpStoreCacheMax*2; i++ {
		s.Resolve(fmt.Sprintf("sprayed-token-%d", i), "")
	}

	s.mu.Lock()
	n := len(s.cache)
	s.mu.Unlock()
	if n > httpStoreCacheMax {
		t.Errorf("cache grew to %d entries, cap is %d", n, httpStoreCacheMax)
	}
}

// Expired entries are preferred for eviction so live results survive a spray.
func TestHTTPStore_EvictionPrefersExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"slug": "turing"})
	}))
	defer srv.Close()

	s := NewHTTPStore(srv.URL, "")
	s.SetTTLs(time.Hour, time.Hour)
	if _, ok := s.Resolve("live-token", ""); !ok {
		t.Fatal("live token should resolve")
	}

	// Fill the rest of the cache with entries that are already expired.
	s.mu.Lock()
	for i := 0; i < httpStoreCacheMax; i++ {
		s.cache[fmt.Sprintf("expired-%d", i)] = httpStoreCacheEntry{
			result:  httpStoreResult{ok: false},
			expires: time.Now().Add(-time.Minute),
		}
	}
	s.mu.Unlock()

	// The insert that overflows the cap must sweep the expired spray, not the
	// live entry.
	s.Resolve("one-more", "")

	s.mu.Lock()
	_, liveSurvived := s.cache["live-token"]
	n := len(s.cache)
	s.mu.Unlock()
	if !liveSurvived {
		t.Error("live entry was evicted while expired entries existed")
	}
	if n > httpStoreCacheMax {
		t.Errorf("cache size %d exceeds cap %d after eviction", n, httpStoreCacheMax)
	}
}
