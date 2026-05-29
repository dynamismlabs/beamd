package auth

import (
	"encoding/json"
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
	slug, ok := s.Resolve("T1")
	if !ok || slug != "turing" {
		t.Errorf("Resolve(T1) = (%q, %v); want (turing, true)", slug, ok)
	}
	if slug, ok := s.Resolve("garbage"); ok {
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
	if _, ok := s.Resolve("T1"); ok {
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
		slug, ok := s.Resolve("T1")
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
		if _, ok := s.Resolve("missing"); ok {
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
		if _, ok := s.Resolve("T1"); ok {
			t.Fatal("500 should not authorize")
		}
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 verify calls (no caching on 5xx), got %d", got)
	}
}
