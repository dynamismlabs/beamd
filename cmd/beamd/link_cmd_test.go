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
	out := renderProjectFile("staging.beamd.run", "trey", "", "", false)
	for _, want := range []string{"safe to commit", "server: staging.beamd.run", "scope: trey"} {
		if !strings.Contains(out, want) {
			t.Errorf("committable file missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name:") || strings.Contains(out, "from:") {
		t.Errorf("unset name/from should be omitted, got:\n%s", out)
	}

	// name is emitted when set; from omitted.
	out = renderProjectFile("edge.example.com", "acme", "api", "", false)
	if !strings.Contains(out, "name: api") {
		t.Errorf("expected name: api in:\n%s", out)
	}

	// Local overlay carries a DO-NOT-COMMIT header.
	out = renderProjectFile("edge.example.com", "acme", "", "dir", true)
	if !strings.Contains(out, "DO NOT COMMIT") {
		t.Errorf("local file missing the do-not-commit header:\n%s", out)
	}
	if !strings.Contains(out, "from: dir") {
		t.Errorf("expected from: dir in:\n%s", out)
	}
}
