// Package dns abstracts DNS provider operations behind the libdns
// interface. PRD §5: provider is pluggable; the OSS binary compiles in
// several common ones, operator picks via `dns_provider:` config.
//
// M4 ships Cloudflare (the reference target) and a `stub` provider for
// tests. Adding Route53/DigitalOcean/etc. is one import + one switch
// case in Open() — see PRD §17.
package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

// Provider is the abstraction every beam DNS adapter implements.
// We deliberately use libdns's existing interfaces — provider authors
// only need to satisfy libdns to plug in.
type Provider interface {
	libdns.RecordGetter
	libdns.RecordAppender
	libdns.RecordSetter
	libdns.RecordDeleter
}

// Open instantiates the configured provider. `creds` semantics vary
// by provider (Cloudflare: API token with Zone.DNS:Write).
func Open(name, creds string) (Provider, error) {
	switch name {
	case "cloudflare":
		if creds == "" {
			return nil, fmt.Errorf("cloudflare provider requires creds (Zone.DNS:Write API token)")
		}
		return &cloudflare.Provider{APIToken: creds}, nil
	case "stub":
		return NewStubProvider(), nil
	// Add new providers here. PRD §5 / TASKS M4: route53, digitalocean,
	// hetzner, gcloud, gandi to follow once the Cloudflare path is real.
	default:
		return nil, fmt.Errorf("unsupported dns provider %q (see README for compiled-in list)", name)
	}
}

// ResolveZone returns the registered DNS zone that contains fqdn.
//
// libdns providers expect the `zone` argument to SetRecords/AppendRecords
// to be an actual registered zone (e.g. `dynami.sm`), with record names
// relative to it. When `base_domain` is a subdomain of the real zone
// (e.g. `tunnel.dynami.sm`), passing it straight through makes
// Cloudflare answer "0 zones". This resolves the true zone so beamd
// works whether base_domain is an apex or any-depth subdomain.
//
// For Cloudflare it lists the zones the API token can see and returns
// the longest one that is a suffix of fqdn. For the stub provider (and
// any unknown provider) it returns fqdn unchanged — the apex assumption.
func ResolveZone(ctx context.Context, provider, creds, fqdn string) (string, error) {
	fqdn = strings.TrimSuffix(fqdn, ".")
	switch provider {
	case "cloudflare":
		zones, err := cloudflareZones(ctx, creds)
		if err != nil {
			return "", err
		}
		best := ""
		for _, z := range zones {
			if fqdn == z || strings.HasSuffix(fqdn, "."+z) {
				if len(z) > len(best) {
					best = z
				}
			}
		}
		if best == "" {
			return "", fmt.Errorf("no Cloudflare zone contains %q; token sees %d zone(s): %v", fqdn, len(zones), zones)
		}
		return best, nil
	default:
		// stub / unknown: assume base_domain is itself the zone.
		return fqdn, nil
	}
}

// cloudflareZones lists every zone name the API token can read, paging
// through results.
func cloudflareZones(ctx context.Context, token string) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?per_page=50&page=%d", page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list cloudflare zones: %w", err)
		}
		var body struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
			Result []struct {
				Name string `json:"name"`
			} `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode cloudflare zones response: %w", err)
		}
		if !body.Success {
			msg := "unknown error"
			if len(body.Errors) > 0 {
				msg = body.Errors[0].Message
			}
			return nil, fmt.Errorf("cloudflare zones list failed: %s", msg)
		}
		for _, z := range body.Result {
			names = append(names, strings.TrimSuffix(z.Name, "."))
		}
		if body.ResultInfo.TotalPages <= body.ResultInfo.Page {
			break
		}
	}
	return names, nil
}

// relName returns the record name for `label` relative to `zone`, given
// that the public hostnames live under `baseDomain`. Examples:
//
//	zone=dynami.sm baseDomain=tunnel.dynami.sm label=turing   → turing.tunnel
//	zone=dynami.sm baseDomain=tunnel.dynami.sm label=*.turing → *.turing.tunnel
//	zone=beam.example.com baseDomain=beam.example.com label=turing → turing
func relName(zone, baseDomain, label string) (string, error) {
	zone = strings.TrimSuffix(zone, ".")
	baseDomain = strings.TrimSuffix(baseDomain, ".")
	if baseDomain == zone {
		return label, nil
	}
	if !strings.HasSuffix(baseDomain, "."+zone) {
		return "", fmt.Errorf("base_domain %q is not within zone %q", baseDomain, zone)
	}
	sub := strings.TrimSuffix(baseDomain, "."+zone)
	return label + "." + sub, nil
}

// ProvisionSlug upserts the A (and optional AAAA) records that make a
// developer's tunnels resolve to the edge. `zone` is the registered DNS zone
// (from ResolveZone); record names are written relative to it so subdomain
// base_domains work. Idempotent — re-running is a no-op if the records match.
//
// Namespaced (slug set): writes `<slug>.<base>` + `*.<slug>.<base>`.
// Flat (slug ""): writes just `*.<base>` — the base apex already resolves to
// the edge (it's the edge's own A record), and flat tunnels live directly at
// `<name>.<base>`.
func ProvisionSlug(ctx context.Context, p Provider, zone, baseDomain, slug, edgeIPv4, edgeIPv6 string) error {
	if edgeIPv4 == "" && edgeIPv6 == "" {
		return fmt.Errorf("at least one of edge_ipv4 or edge_ipv6 must be set")
	}

	labels := []string{"*"} // flat: the base wildcard
	if slug != "" {
		labels = []string{slug, "*." + slug}
	}

	var recs []libdns.Record
	addAddress := func(label, ipStr string) error {
		name, err := relName(zone, baseDomain, label)
		if err != nil {
			return err
		}
		addr, err := netip.ParseAddr(ipStr)
		if err != nil {
			return fmt.Errorf("parse %q: %w", ipStr, err)
		}
		recs = append(recs, libdns.Address{
			Name: name,
			TTL:  60 * time.Second,
			IP:   addr,
		})
		return nil
	}
	for _, label := range labels {
		if edgeIPv4 != "" {
			if err := addAddress(label, edgeIPv4); err != nil {
				return err
			}
		}
		if edgeIPv6 != "" {
			if err := addAddress(label, edgeIPv6); err != nil {
				return err
			}
		}
	}

	_, err := p.SetRecords(ctx, zone, recs)
	return err
}
