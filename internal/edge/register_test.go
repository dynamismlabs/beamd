package edge

import (
	"io"
	"net"
	"testing"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/hashicorp/yamux"
)

// yamuxPair returns a yamux client session over an in-memory pipe, plus a
// cleanup. Keepalive is disabled so an unread peer doesn't churn during the test.
func yamuxPair(t *testing.T) (*yamux.Session, func()) {
	t.Helper()
	c1, c2 := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	s, err := yamux.Client(c1, cfg)
	if err != nil {
		t.Fatalf("yamux.Client: %v", err)
	}
	return s, func() { _ = s.Close(); _ = c1.Close(); _ = c2.Close() }
}

// A reconnecting tunnel whose new hostname set is a STRICT SUBSET of the dead
// session it reclaims (e.g. a rename alias dropped between two scope-hostnames
// fetches) must leave activeTunnels at the true live count. register reclaims the
// dead tunnel FULLY — evicting its leftover routes — so the dead session's later
// dropSession can't double-decrement a tunnel register already netted to zero.
// Regression: before the fix the stale alias route survived and dropSession drove
// the gauge to 0 with a live tunnel still serving.
func TestRegisterReclaimsSubsetHostsWithoutGaugeDrift(t *testing.T) {
	e := &Edge{
		cfg: &config.Server{
			BaseDomain: "base.test",
			URLShape:   "hyphen",
			// Cap of 1: the reconnect must reclaim the dead session's slot rather
			// than be rejected with over_limit. Before the reclaim was moved ahead
			// of the cap check, the dead session's still-counted name tripped this.
			MaxTunnelsPerToken: 1,
		},
		routes:   make(map[string]*Route),
		sessions: make(map[*Session]struct{}),
		metrics:  newMetrics(),
		// hostnames nil → register derives the single default host for the slug.
	}

	deadYx, deadClose := yamuxPair(t)
	defer deadClose()
	_ = deadYx.Close() // dead session: IsClosed() == true → reclaimable

	liveYx, liveClose := yamuxPair(t)
	defer liveClose()

	const (
		defaultHost = "api-acme.base.test"      // == naming.Hostname("api","acme",...)
		aliasHost   = "api-acmealias.base.test" // a retained rename alias, since dropped
	)

	dead := &Session{
		yamux: deadYx,
		slug:  "acme",
		names: map[string]struct{}{"api": {}},
		hosts: map[string][]string{"api": {defaultHost, aliasHost}},
	}
	live := &Session{
		yamux: liveYx,
		slug:  "acme",
		names: map[string]struct{}{},
		hosts: map[string][]string{},
	}
	e.sessions[dead] = struct{}{}
	e.sessions[live] = struct{}{}
	e.routes[defaultHost] = &Route{session: dead, name: "api"}
	e.routes[aliasHost] = &Route{session: dead, name: "api"}
	e.metrics.activeSessions.Store(2)
	e.metrics.activeTunnels.Store(1) // the dead session's one tunnel

	// The live session reconnects and re-registers "api". With the alias gone,
	// its host set is just the default host — a subset of what `dead` holds.
	if _, perr := e.register(live, "api"); perr != nil {
		t.Fatalf("register: %v", perr)
	}

	// Stale alias route reclaimed; the default host now points at the live session.
	if r := e.routes[aliasHost]; r != nil {
		t.Fatalf("stale alias route survived reclaim: %+v", r)
	}
	if r := e.routes[defaultHost]; r == nil || r.session != live {
		t.Fatalf("default host not reassigned to the live session: %+v", r)
	}
	if got := e.metrics.activeTunnels.Load(); got != 1 {
		t.Fatalf("after register: activeTunnels = %d, want 1", got)
	}

	// The dead session's dropSession now finds nothing of its own to decrement —
	// the gauge holds at the true live count (the bug drove it to 0 here).
	e.dropSession(dead)
	if got := e.metrics.activeTunnels.Load(); got != 1 {
		t.Fatalf("after dropSession: activeTunnels = %d, want 1 (one live tunnel)", got)
	}
}
