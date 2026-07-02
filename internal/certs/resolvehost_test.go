package certs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveHostAuthorizer(t *testing.T) {
	// Stub control plane: only "api.acme.com" is a verified host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("host") == "api.acme.com" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"slug":"acme","cert_mode":"on_demand"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // unverified → refuse
	}))
	defer srv.Close()

	authorize := NewResolveHostAuthorizer(srv.URL, "s3cret")

	// verified host → allowed (nil)
	if err := authorize(context.Background(), "api.acme.com"); err != nil {
		t.Errorf("verified host should be authorized, got %v", err)
	}
	// unverified host → refused
	if err := authorize(context.Background(), "evil.example"); err == nil {
		t.Errorf("unverified host must be refused (no cert), got nil")
	}
}

func TestResolveHostAuthorizerFailsClosed(t *testing.T) {
	// An unreachable control plane must DENY (fail closed) — never let an
	// arbitrary host mint a cert.
	authorize := NewResolveHostAuthorizer("http://127.0.0.1:0/resolve-host", "s")
	if err := authorize(context.Background(), "api.acme.com"); err == nil {
		t.Errorf("unreachable control plane must fail closed (deny), got nil")
	}
}

func TestResolveHostAuthorizerBadSecretDenies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	authorize := NewResolveHostAuthorizer(srv.URL, "wrong")
	if err := authorize(context.Background(), "api.acme.com"); err == nil {
		t.Errorf("a 401 must deny issuance, got nil")
	}
}

// A verified host the operator marked "delegated" (customer serves its own
// cert) must NOT get an edge-issued on-demand cert.
func TestResolveHostAuthorizer_DelegatedDenies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slug":"acme","cert_mode":"delegated"}`))
	}))
	defer srv.Close()

	authorize := NewResolveHostAuthorizer(srv.URL, "")
	if err := authorize(context.Background(), "api.acme.com"); err == nil {
		t.Error("a delegated host must be refused on-demand issuance, got nil")
	}
}

// A 2xx with no cert_mode field must still allow issuance (drop-in: works
// against a control plane that predates the field, no lockstep deploy).
func TestResolveHostAuthorizer_MissingCertModeAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slug":"acme"}`)) // no cert_mode
	}))
	defer srv.Close()

	authorize := NewResolveHostAuthorizer(srv.URL, "")
	if err := authorize(context.Background(), "api.acme.com"); err != nil {
		t.Errorf("missing cert_mode should allow issuance (drop-in), got %v", err)
	}
}
