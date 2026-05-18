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
	"fmt"
	"net/netip"
	"time"

	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

// Provider is the abstraction every conduit DNS adapter implements.
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

// ProvisionSlug upserts the A (and optional AAAA) records that make
// `<slug>.<base>` and `*.<slug>.<base>` resolve to the edge.
// Idempotent — re-running is a no-op if the records already match.
func ProvisionSlug(ctx context.Context, p Provider, baseDomain, slug, edgeIPv4, edgeIPv6 string) error {
	if slug == "" {
		return fmt.Errorf("slug is required")
	}
	if edgeIPv4 == "" && edgeIPv6 == "" {
		return fmt.Errorf("at least one of edge_ipv4 or edge_ipv6 must be set")
	}

	var recs []libdns.Record
	addAddress := func(name, ipStr string) error {
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
	if edgeIPv4 != "" {
		if err := addAddress(slug, edgeIPv4); err != nil {
			return err
		}
		if err := addAddress("*."+slug, edgeIPv4); err != nil {
			return err
		}
	}
	if edgeIPv6 != "" {
		if err := addAddress(slug, edgeIPv6); err != nil {
			return err
		}
		if err := addAddress("*."+slug, edgeIPv6); err != nil {
			return err
		}
	}

	_, err := p.SetRecords(ctx, baseDomain, recs)
	return err
}
