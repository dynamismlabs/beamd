package main

import (
	"strings"
	"testing"

	"github.com/dynamismlabs/beamd/internal/config"
)

func TestChooseScope(t *testing.T) {
	// No scopes (self-hosted / OSS) → no org to pin.
	if got := chooseScope(&config.Account{}, true); got != "" {
		t.Errorf("no scopes: got %q, want \"\"", got)
	}

	// Non-interactive with a default → the default.
	a := &config.Account{
		Scopes:       []config.ScopeRef{{Slug: "acme"}, {Slug: "trey"}},
		DefaultScope: "trey",
	}
	if got := chooseScope(a, true); got != "trey" {
		t.Errorf("with default: got %q, want trey", got)
	}

	// Non-interactive, no explicit default → first scope.
	b := &config.Account{Scopes: []config.ScopeRef{{Slug: "first"}, {Slug: "second"}}}
	if got := chooseScope(b, true); got != "first" {
		t.Errorf("no default: got %q, want first", got)
	}
}

func TestRenderProjectFile(t *testing.T) {
	// Committable file: header, server, scope; no empty keys.
	out := renderProjectFile("staging.beamd.run", "trey", "", "", nil, false)
	for _, want := range []string{"safe to commit", "server: staging.beamd.run", "scope: trey"} {
		if !strings.Contains(out, want) {
			t.Errorf("committable file missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name:") || strings.Contains(out, "from:") || strings.Contains(out, "services:") {
		t.Errorf("unset name/from/services should be omitted, got:\n%s", out)
	}

	// name is emitted when set; from omitted.
	out = renderProjectFile("edge.example.com", "acme", "api", "", nil, false)
	if !strings.Contains(out, "name: api") {
		t.Errorf("expected name: api in:\n%s", out)
	}

	// Local overlay carries a DO-NOT-COMMIT header.
	out = renderProjectFile("edge.example.com", "acme", "", "dir", nil, true)
	if !strings.Contains(out, "DO NOT COMMIT") {
		t.Errorf("local file missing the do-not-commit header:\n%s", out)
	}
	if !strings.Contains(out, "from: dir") {
		t.Errorf("expected from: dir in:\n%s", out)
	}

	// Services block: a header line plus one entry per service, sorted.
	out = renderProjectFile("edge.example.com", "acme", "", "", map[string]int{"web": 8080, "api": 3000}, false)
	if !strings.Contains(out, "services:") || !strings.Contains(out, "  api: 3000") || !strings.Contains(out, "  web: 8080") {
		t.Errorf("services block missing/incomplete in:\n%s", out)
	}
	if strings.Index(out, "api: 3000") > strings.Index(out, "web: 8080") {
		t.Errorf("services should be sorted (api before web) in:\n%s", out)
	}
}

func TestParseServices(t *testing.T) {
	// Empty → nil, no error.
	if m, err := parseServices("  "); err != nil || m != nil {
		t.Errorf("empty: got %v, %v; want nil, nil", m, err)
	}

	// Valid spec, tolerant of spaces.
	m, err := parseServices("api=3000, web =8080")
	if err != nil {
		t.Fatalf("valid spec errored: %v", err)
	}
	if m["api"] != 3000 || m["web"] != 8080 {
		t.Errorf("parsed %v, want api=3000 web=8080", m)
	}

	for _, bad := range []string{"api", "api=", "=3000", "api=notaport", "api=0", "api=70000", "Api_Bad=3000", "api=3000,api=3001"} {
		if _, err := parseServices(bad); err == nil {
			t.Errorf("parseServices(%q) = nil error, want an error", bad)
		}
	}
}
