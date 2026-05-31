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

func TestSelectProfile_Ladder(t *testing.T) {
	t.Setenv("BEAMD_PROFILE", "")
	empty := ""
	mk := func(profile string) *clientFlags {
		p := profile
		return &clientFlags{profile: &p, config: &empty}
	}

	// flag wins.
	if n, src, _ := selectProfile(mk("work"), &config.Project{Profile: "ignored"}, &config.Global{Current: "cur"}); n != "work" || src != "flag" {
		t.Errorf("flag: got %q/%q", n, src)
	}
	// env beats project + current.
	t.Setenv("BEAMD_PROFILE", "envp")
	if n, src, _ := selectProfile(mk(""), &config.Project{Profile: "pp"}, &config.Global{Current: "cur"}); n != "envp" || src != "env" {
		t.Errorf("env: got %q/%q", n, src)
	}
	t.Setenv("BEAMD_PROFILE", "")
	// .beamd profile beats current.
	if n, src, _ := selectProfile(mk(""), &config.Project{Profile: "pp"}, &config.Global{Current: "cur"}); n != "pp" || src != "project" {
		t.Errorf("project: got %q/%q", n, src)
	}
	// global current is the fallback.
	if n, src, _ := selectProfile(mk(""), nil, &config.Global{Current: "cur"}); n != "cur" || src != "current" {
		t.Errorf("current: got %q/%q", n, src)
	}
	// Nothing → empty.
	if n, _, _ := selectProfile(mk(""), nil, &config.Global{}); n != "" {
		t.Errorf("none: got %q", n)
	}
}

func TestSelectProfile_ServerMatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BEAMD_PROFILE", "")
	if err := config.SaveProfile("acme", &config.Client{Server: "tunnel.acme.com:443", Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	empty := ""
	cf := &clientFlags{profile: &empty, config: &empty}

	// A committed `.beamd { server: tunnel.acme.com }` (no port) resolves to
	// the local profile whose server matches, whatever it's named.
	n, src, unmatched := selectProfile(cf, &config.Project{Server: "tunnel.acme.com"}, &config.Global{})
	if n != "acme" || src != "project-server" || unmatched != "" {
		t.Errorf("server match: got %q/%q/%q, want acme/project-server/\"\"", n, src, unmatched)
	}

	// No matching profile → signals the unmatched server for messaging.
	n, _, unmatched = selectProfile(cf, &config.Project{Server: "tunnel.nope.com"}, &config.Global{})
	if n != "" || unmatched != "tunnel.nope.com" {
		t.Errorf("server unmatched: got name=%q unmatched=%q", n, unmatched)
	}
}
