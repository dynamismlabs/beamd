package devicecode

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscover_OSSReturnsEmptyDiscovery(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/beam-auth", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d, err := Discover(context.Background(), http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d != nil {
		t.Errorf("expected nil discovery for empty body, got %+v", d)
	}
}

func TestDiscover_HostedReturnsURLs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/beam-auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Discovery{
			DeviceCodeURL:   "https://app.example.com/api/device/code",
			TokenURL:        "https://app.example.com/api/device/token",
			VerificationURI: "https://app.example.com/device",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d, err := Discover(context.Background(), http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil discovery")
	}
	if d.DeviceCodeURL == "" || d.TokenURL == "" {
		t.Errorf("missing URLs: %+v", d)
	}
}

func TestLogin_FullFlow(t *testing.T) {
	const finalToken = "tok-XYZ"
	const userCode = "WXYZ-7K9P"

	var polls atomic.Int64
	mux := http.NewServeMux()

	mux.HandleFunc("/api/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode: "dc-internal-7", UserCode: userCode,
			ExpiresIn: 30, Interval: 1,
		})
	})

	mux.HandleFunc("/api/device/token", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		if n < 2 {
			_ = json.NewEncoder(w).Encode(TokenResponse{Error: "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: finalToken})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	disc := &Discovery{
		DeviceCodeURL:   srv.URL + "/api/device/code",
		TokenURL:        srv.URL + "/api/device/token",
		VerificationURI: srv.URL + "/device",
	}

	out := &bytes.Buffer{}
	// Speed the test up: bypass the default 5s interval via Discovery (Interval=0 in
	// DeviceCodeResponse → default 5s). Use a custom hc with the http.Client; the
	// test runs fast because we poll once-pending, then succeed.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tok, err := Login(ctx, http.DefaultClient, disc, out)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok != finalToken {
		t.Errorf("token = %q, want %q", tok, finalToken)
	}
	if !strings.Contains(out.String(), userCode) {
		t.Errorf("user code not printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "logged in") {
		t.Errorf("missing 'logged in' confirmation:\n%s", out.String())
	}
}

func TestLogin_AccessDeniedFailsImmediately(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode: "dc", UserCode: "X-Y", ExpiresIn: 60, Interval: 1,
		})
	})
	mux.HandleFunc("/api/device/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(TokenResponse{Error: "access_denied"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	disc := &Discovery{
		DeviceCodeURL:   srv.URL + "/api/device/code",
		TokenURL:        srv.URL + "/api/device/token",
		VerificationURI: srv.URL + "/device",
	}
	out := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := Login(ctx, http.DefaultClient, disc, out)
	if err == nil {
		t.Fatal("expected error on access_denied")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("err = %v, want 'denied'", err)
	}
}

func TestNormalizeDiscoveryURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"tunnel.example.com:443", "https://tunnel.example.com:443/.well-known/beam-auth"},
		{"tunnel.example.com", "https://tunnel.example.com:443/.well-known/beam-auth"},
		{"https://tunnel.example.com", "https://tunnel.example.com/.well-known/beam-auth"},
		{"http://localhost:8443", "http://localhost:8443/.well-known/beam-auth"},
	}
	for _, c := range cases {
		if got := normalizeDiscoveryURL(c.in); got != c.want {
			t.Errorf("normalizeDiscoveryURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
