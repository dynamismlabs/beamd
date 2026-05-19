// Package devicecode implements the beam client's device-code login
// dance against a hosted web app. RFC 8628-shaped: the CLI requests a
// device + user code, prints instructions, polls until the user
// approves it via the browser, then returns the issued bearer token.
//
// The web app is the canonical source of truth for device-code state;
// beam's role here is purely client-side. Server-side endpoints live
// in the operator's Next.js/whatever app, not in this repo.
package devicecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Discovery is the response body of `/.well-known/beam-auth` on the
// beamd edge. Empty fields → device-code login is not offered (OSS).
type Discovery struct {
	DeviceCodeURL   string `json:"device_code_url,omitempty"`
	TokenURL        string `json:"token_url,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
}

// DeviceCodeResponse is what the web app's device-code endpoint returns.
// Shape mirrors RFC 8628 §3.2.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri,omitempty"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval,omitempty"`
}

// TokenResponse is what the web app's token-poll endpoint returns.
// One of AccessToken (success) or Error (pending / denied / expired).
// Error codes mirror RFC 8628 §3.5.
type TokenResponse struct {
	AccessToken string `json:"access_token,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Discover fetches `/.well-known/beam-auth` on the beamd server.
// Returns (nil, nil) when the server doesn't advertise device-code
// endpoints (i.e. OSS deployments where the CLI should require --token).
func Discover(ctx context.Context, hc *http.Client, serverAddr string) (*Discovery, error) {
	url := normalizeDiscoveryURL(serverAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %s", url, resp.Status)
	}
	var d Discovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode discovery: %w", err)
	}
	if d.DeviceCodeURL == "" || d.TokenURL == "" {
		return nil, nil
	}
	return &d, nil
}

// Login executes the device-code dance and returns the issued token.
// Prints user-facing instructions on `out`. Honors ctx cancellation.
func Login(ctx context.Context, hc *http.Client, disc *Discovery, out io.Writer) (string, error) {
	dc, err := requestDeviceCode(ctx, hc, disc.DeviceCodeURL)
	if err != nil {
		return "", err
	}

	verifyURI := disc.VerificationURI
	if dc.VerificationURI != "" {
		verifyURI = dc.VerificationURI
	}
	if verifyURI == "" {
		return "", fmt.Errorf("server did not return a verification URI")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Open this URL in your browser:")
	fmt.Fprintf(out, "  %s\n", verifyURI)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "And enter this code:")
	fmt.Fprintf(out, "  %s\n", dc.UserCode)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Waiting for confirmation...")

	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expires := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		expires = time.Now().Add(10 * time.Minute)
	}

	for time.Now().Before(expires) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}

		tok, err := pollToken(ctx, hc, disc.TokenURL, dc.DeviceCode)
		if err != nil {
			// Network-level error — keep polling but log to out.
			fmt.Fprintf(out, "  (poll error: %v — retrying)\n", err)
			continue
		}
		if tok.AccessToken != "" {
			fmt.Fprintln(out, "✓ logged in")
			return tok.AccessToken, nil
		}
		switch tok.Error {
		case "", "authorization_pending":
			// keep polling
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return "", fmt.Errorf("login denied")
		case "expired_token":
			return "", fmt.Errorf("device code expired before approval; rerun login")
		default:
			// Unknown — keep polling but surface it.
			fmt.Fprintf(out, "  (server: %s — continuing)\n", tok.Error)
		}
	}
	return "", fmt.Errorf("device code expired before approval")
}

func requestDeviceCode(ctx context.Context, hc *http.Client, url string) (*DeviceCodeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST %s returned %s", url, resp.Status)
	}
	var dc DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, fmt.Errorf("decode device-code response: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, fmt.Errorf("device-code response missing required fields")
	}
	return &dc, nil
}

func pollToken(ctx context.Context, hc *http.Client, url, deviceCode string) (*TokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"device_code": deviceCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &tok, nil
}

// normalizeDiscoveryURL accepts either `host:port` or a full URL and
// returns the full discovery URL.
func normalizeDiscoveryURL(serverAddr string) string {
	if strings.HasPrefix(serverAddr, "http://") || strings.HasPrefix(serverAddr, "https://") {
		return strings.TrimRight(serverAddr, "/") + "/.well-known/beam-auth"
	}
	if !strings.Contains(serverAddr, ":") {
		serverAddr = serverAddr + ":443"
	}
	return "https://" + serverAddr + "/.well-known/beam-auth"
}
