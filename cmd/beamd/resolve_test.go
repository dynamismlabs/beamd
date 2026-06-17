package main

import (
	"path/filepath"
	"testing"

	"github.com/dynamismlabs/beamd/internal/config"
)

func TestResolveLabel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myapp")
	ctx := &tunnelContext{Cwd: dir, Global: &config.Global{}}

	// --as (literal) wins over everything.
	if l, err := resolveLabel("web-api", "dir", ctx, 3000); err != nil || l != "web-api" {
		t.Errorf("--as: got %q, %v", l, err)
	}
	// --from dir derives from cwd basename.
	if l, err := resolveLabel("", "dir", ctx, 3000); err != nil || l != "myapp" {
		t.Errorf("--from dir: got %q, %v", l, err)
	}
	// .beamd name: literal.
	ctx.Project = &config.Project{Name: "proj"}
	if l, err := resolveLabel("", "", ctx, 3000); err != nil || l != "proj" {
		t.Errorf(".beamd name: got %q, %v", l, err)
	}
	// .beamd from: derive.
	ctx.Project = &config.Project{From: "dir"}
	if l, err := resolveLabel("", "", ctx, 3000); err != nil || l != "myapp" {
		t.Errorf(".beamd from: got %q, %v", l, err)
	}
	// global default from: port.
	ctx.Project = nil
	ctx.Global = &config.Global{Defaults: config.NamingDefaults{From: "port"}}
	if l, err := resolveLabel("", "", ctx, 3000); err != nil || l != "3000" {
		t.Errorf("global from: got %q, %v", l, err)
	}
	// Nothing set → port.
	ctx.Global = &config.Global{}
	if l, err := resolveLabel("", "", ctx, 8080); err != nil || l != "8080" {
		t.Errorf("default: got %q, %v", l, err)
	}
	// Invalid --as errors.
	if _, err := resolveLabel("Bad_Name", "", ctx, 3000); err == nil {
		t.Error("invalid --as should error")
	}
}

func TestSelectAccount_Ladder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BEAMD_SERVER", "")
	mk := func(server string) *clientFlags {
		s, empty := server, ""
		return &clientFlags{server: &s, scope: &empty, config: &empty}
	}

	// flag wins, normalized to :443.
	if srv, src := selectAccount(mk("flag.test"), &config.Project{Server: "proj.test"}, &config.Global{Current: "cur.test:443"}); srv != "flag.test:443" || src != "flag" {
		t.Errorf("flag: got %q/%q", srv, src)
	}
	// env beats project + current.
	t.Setenv("BEAMD_SERVER", "env.test")
	if srv, src := selectAccount(mk(""), &config.Project{Server: "proj.test"}, &config.Global{Current: "cur.test:443"}); srv != "env.test:443" || src != "env" {
		t.Errorf("env: got %q/%q", srv, src)
	}
	t.Setenv("BEAMD_SERVER", "")
	// .beamd server beats current.
	if srv, src := selectAccount(mk(""), &config.Project{Server: "proj.test"}, &config.Global{Current: "cur.test:443"}); srv != "proj.test:443" || src != "project" {
		t.Errorf("project: got %q/%q", srv, src)
	}
	// Nothing + no accounts → empty. (Checked before any account exists.)
	if srv, src := selectAccount(mk(""), nil, &config.Global{}); srv != "" || src != "" {
		t.Errorf("none: got %q/%q", srv, src)
	}
	// global current is the fallback — but only when it names a real account.
	if err := config.SaveAccount(&config.Account{Server: "cur.test:443", Token: "T"}); err != nil {
		t.Fatal(err)
	}
	if srv, src := selectAccount(mk(""), nil, &config.Global{Current: "cur.test:443"}); srv != "cur.test:443" || src != "current" {
		t.Errorf("current: got %q/%q", srv, src)
	}
}

// TestSelectAccount_DanglingCurrentIgnored guards the self-heal: a `current`
// that names a non-existent account (old binary, removed account, hand-edited
// config) must not shadow a real login — it falls through the ladder rather
// than returning the ghost (which downstream turns into a nonsense "log into
// <ghost>" error).
func TestSelectAccount_DanglingCurrentIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BEAMD_SERVER", "")
	empty := ""
	cf := &clientFlags{server: &empty, scope: &empty, config: &empty}

	// One real account + a dangling current → fall through to the only account.
	if err := config.SaveAccount(&config.Account{Server: "real.test:443", Token: "T"}); err != nil {
		t.Fatal(err)
	}
	if srv, src := selectAccount(cf, nil, &config.Global{Current: "ghost.test:443"}); srv != "real.test:443" || src != "only" {
		t.Errorf("dangling current, one account: got %q/%q, want real.test:443/only", srv, src)
	}

	// Dangling current with no accounts at all → empty, never the ghost.
	t.Setenv("HOME", t.TempDir()) // fresh home, zero accounts
	if srv, src := selectAccount(cf, nil, &config.Global{Current: "ghost.test:443"}); srv != "" || src != "" {
		t.Errorf("dangling current, no accounts: got %q/%q, want \"\"/\"\"", srv, src)
	}
}

func TestSelectAccount_SingleAndAmbiguous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BEAMD_SERVER", "")
	empty := ""
	cf := &clientFlags{server: &empty, scope: &empty, config: &empty}

	// Exactly one account → use it without a flag.
	if err := config.SaveAccount(&config.Account{Server: "solo.test:443", Token: "T"}); err != nil {
		t.Fatal(err)
	}
	if srv, src := selectAccount(cf, nil, &config.Global{}); srv != "solo.test:443" || src != "only" {
		t.Errorf("single account: got %q/%q, want solo.test:443/only", srv, src)
	}

	// A second account, nothing selecting → ambiguous (no silent guess).
	if err := config.SaveAccount(&config.Account{Server: "two.test:443", Token: "T2"}); err != nil {
		t.Fatal(err)
	}
	if srv, src := selectAccount(cf, nil, &config.Global{}); srv != "" || src != "ambiguous" {
		t.Errorf("ambiguous: got %q/%q, want \"\"/ambiguous", srv, src)
	}
}
