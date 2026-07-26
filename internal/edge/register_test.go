package edge

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

type registerTransport struct {
	mu        sync.Mutex
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
}

func newRegisterTransport() *registerTransport {
	return &registerTransport{done: make(chan struct{})}
}

func (s *registerTransport) Kind() tunnel.Kind { return tunnel.KindYamux }

func (s *registerTransport) OpenStream(context.Context) (tunnel.Stream, error) {
	return nil, errors.New("not implemented by register test transport")
}

func (s *registerTransport) AcceptStream(context.Context) (tunnel.Stream, error) {
	return nil, errors.New("not implemented by register test transport")
}

func (s *registerTransport) Done() <-chan struct{} { return s.done }

func (s *registerTransport) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *registerTransport) CloseInfo() tunnel.CloseInfo {
	return tunnel.CloseInfo{CodeValid: true, Code: tunnel.CloseNormal, Reason: "test"}
}

func (s *registerTransport) CloseWithError(tunnel.ErrorCode, string) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

func (s *registerTransport) LocalAddr() net.Addr  { return nil }
func (s *registerTransport) RemoteAddr() net.Addr { return nil }

var _ tunnel.Session = (*registerTransport)(nil)

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
		routes:    make(map[string]*Route),
		sessions:  make(map[*Session]struct{}),
		metrics:   newMetrics(),
		authSlots: make(chan struct{}, 2),
		// hostnames nil → register derives the single default host for the slug.
	}

	deadTransport := newRegisterTransport()
	_ = deadTransport.CloseWithError(tunnel.CloseNormal, "dead")
	liveTransport := newRegisterTransport()
	t.Cleanup(func() {
		_ = liveTransport.CloseWithError(tunnel.CloseNormal, "cleanup")
	})

	const (
		defaultHost = "api-acme.base.test"      // == naming.Hostname("api","acme",...)
		aliasHost   = "api-acmealias.base.test" // a retained rename alias, since dropped
	)

	dead := &Session{
		transport: deadTransport,
		kind:      tunnel.KindYamux,
		slug:      "acme",
		names:     map[string]struct{}{"api": {}},
		hosts:     map[string][]string{"api": {defaultHost, aliasHost}},
	}
	live := &Session{
		transport: liveTransport,
		kind:      tunnel.KindYamux,
		slug:      "acme",
		names:     map[string]struct{}{},
		hosts:     map[string][]string{},
	}
	// dropSession releases the authenticated-session lease held by the dead
	// session. Seed exactly that lease so this focused registration regression
	// test exercises the real cleanup path without constructing an edge server.
	e.authSlots <- struct{}{}
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
