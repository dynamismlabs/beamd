package certs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// NewResolveHostAuthorizer returns a certmagic On-Demand decision function that
// permits issuing a cert for a hostname ONLY if the control plane confirms it's a
// verified custom host (a 200 from GET /api/internal/resolve-host?host=...). This
// is THE gate that stops On-Demand TLS from being abused to mint certs for
// arbitrary hostnames (url-model §8.2 / §12): a non-2xx — including the 404 the
// control plane returns for an unverified host — denies issuance, and a network
// error fails CLOSED (deny). `url` is the resolve-host endpoint; `secret` is the
// shared edge secret.
func NewResolveHostAuthorizer(endpoint, secret string) func(ctx context.Context, name string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(ctx context.Context, name string) error {
		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, endpoint+"?host="+url.QueryEscape(name), nil,
		)
		if err != nil {
			return err
		}
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := client.Do(req)
		if err != nil {
			// Fail closed: an unreachable control plane must not let arbitrary
			// hosts mint certs.
			return fmt.Errorf("resolve-host unreachable, denying cert for %q: %w", name, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil // verified host → allow issuance
		}
		return fmt.Errorf("host %q is not a verified custom domain (status %d)", name, resp.StatusCode)
	}
}
