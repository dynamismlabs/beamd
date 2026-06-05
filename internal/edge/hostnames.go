package edge

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dynamismlabs/beamd/internal/naming"
)

// scopeHostnames mirrors the control plane's
// GET /api/internal/scope-hostnames response (beamdapi.ScopeHostnamesResponse).
type scopeHostnames struct {
	PrimarySlug string        `json:"primary_slug"`
	Slugs       []string      `json:"slugs"`
	Shape       string        `json:"shape"`
	Domains     []scopeDomain `json:"domains"`
	PrimaryHost string        `json:"primary_host"`
}

type scopeDomain struct {
	Domain   string `json:"domain"`
	Primary  bool   `json:"primary"`
	CertMode string `json:"cert_mode"`
}

// hostnamesClient fetches + caches a scope's hostname set so a tunnel can be
// registered under all of them (retained rename aliases + verified custom
// domains, url-model §4). nil when the edge isn't in hosted mode (no HTTP token
// store) — registration then falls back to the single default host.
type hostnamesClient struct {
	url    string
	secret string
	ttl    time.Duration
	client *http.Client

	mu    sync.Mutex
	cache map[string]hostnamesEntry
}

type hostnamesEntry struct {
	val *scopeHostnames
	at  time.Time
}

// newHostnamesClient derives the scope-hostnames endpoint from the verify-token
// token-store URL (same control plane + same shared secret). Returns nil for a
// non-HTTP store (self-host), where there are no aliases/custom domains.
func newHostnamesClient(tokenStore string) *hostnamesClient {
	if !strings.HasPrefix(tokenStore, "http://") &&
		!strings.HasPrefix(tokenStore, "https://") {
		return nil
	}
	u := strings.Replace(tokenStore, "/verify-token", "/scope-hostnames", 1)
	if u == tokenStore {
		if i := strings.LastIndex(tokenStore, "/"); i >= 0 {
			u = tokenStore[:i] + "/scope-hostnames"
		}
	}
	return &hostnamesClient{
		url:    u,
		secret: os.Getenv("BEAMD_AUTH_VERIFY_SECRET"),
		ttl:    5 * time.Minute,
		client: &http.Client{Timeout: 5 * time.Second},
		cache:  make(map[string]hostnamesEntry),
	}
}

// get returns the scope's hostnames (cached ~5min), or nil on any failure — the
// caller then falls back to the single default host (fail-soft).
func (c *hostnamesClient) get(slug string) *scopeHostnames {
	c.mu.Lock()
	if e, ok := c.cache[slug]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.val
	}
	c.mu.Unlock()

	val := c.fetch(slug)
	if val != nil {
		c.mu.Lock()
		c.cache[slug] = hostnamesEntry{val: val, at: time.Now()}
		c.mu.Unlock()
	}
	return val
}

func (c *hostnamesClient) fetch(slug string) *scopeHostnames {
	req, err := http.NewRequest(http.MethodGet, c.url+"?slug="+url.QueryEscape(slug), nil)
	if err != nil {
		return nil
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var sh scopeHostnames
	if err := json.NewDecoder(resp.Body).Decode(&sh); err != nil {
		return nil
	}
	return &sh
}

// hostsForTunnel returns every hostname a tunnel `name` (in scope `slug`) should
// answer on, and the primary host to render back to the client. In hosted mode
// it includes the default-shape host for every bound slug (primary + retained
// aliases) plus `<name>.<domain>` for each verified custom domain; the primary is
// the custom primary domain if one exists, else the default-shape primary-slug
// host. Without a hostnames client (self-host) or on fetch failure it's just the
// single default host.
func (e *Edge) hostsForTunnel(name, slug string) (hosts []string, primary string) {
	base := e.cfg.BaseDomain
	shape := e.cfg.Shape()
	defaultHost := naming.Hostname(name, slug, base, shape)

	if e.hostnames == nil {
		return []string{defaultHost}, defaultHost
	}
	sh := e.hostnames.get(slug)
	if sh == nil {
		return []string{defaultHost}, defaultHost
	}

	seen := make(map[string]bool)
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}

	// Default-shape host for every bound slug (primary + retained aliases).
	for _, s := range sh.Slugs {
		add(naming.Hostname(name, s, base, shape))
	}
	if len(hosts) == 0 {
		add(defaultHost)
	}

	// Custom-domain hosts (wildcard: `<name>.<domain>`).
	var primaryDomain string
	for _, d := range sh.Domains {
		add(name + "." + d.Domain)
		if d.Primary {
			primaryDomain = d.Domain
		}
	}

	if primaryDomain != "" {
		primary = name + "." + primaryDomain
	} else {
		ps := sh.PrimarySlug
		if ps == "" {
			ps = slug
		}
		primary = naming.Hostname(name, ps, base, shape)
	}
	if primary == "" {
		primary = defaultHost
	}
	return hosts, primary
}
