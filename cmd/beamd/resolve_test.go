package main

import (
	"os"
	"path/filepath"
	"strings"
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
	// beamd.yaml name: literal.
	ctx.Project = &config.Project{Name: "proj"}
	if l, err := resolveLabel("", "", ctx, 3000); err != nil || l != "proj" {
		t.Errorf("beamd.yaml name: got %q, %v", l, err)
	}
	// beamd.yaml from: derive.
	ctx.Project = &config.Project{From: "dir"}
	if l, err := resolveLabel("", "", ctx, 3000); err != nil || l != "myapp" {
		t.Errorf("beamd.yaml from: got %q, %v", l, err)
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
	// beamd.yaml server beats current.
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

func TestResolveContextCarriesKindForAccountAndExplicitConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BEAMD_SERVER", "")
	t.Setenv(config.TransportEnvVar, "placeholder")
	if err := os.Unsetenv(config.TransportEnvVar); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	if err := config.SaveAccount(&config.Account{
		Server: "hosted.example.com:443",
		Token:  "session-token",
		Kind:   "session",
	}); err != nil {
		t.Fatal(err)
	}
	empty := ""
	accountFlags := &clientFlags{server: &empty, scope: &empty, config: &empty}
	accountCtx := resolveContext(accountFlags)
	if accountCtx.Client == nil || accountCtx.Client.Kind != "session" {
		t.Fatalf("account client = %+v, want session kind", accountCtx.Client)
	}
	if got := mustTransport(accountCtx.Client); got != config.TransportAuto {
		t.Errorf("session account transport = %q, want auto", got)
	}

	explicitPath := filepath.Join(home, "standalone.yaml")
	if err := config.SaveClient(&config.Client{
		Server: "self-hosted.example.com:443",
		Token:  "api-key",
	}, explicitPath); err != nil {
		t.Fatal(err)
	}
	explicitFlags := &clientFlags{server: &empty, scope: &empty, config: &explicitPath}
	explicitCtx := resolveContext(explicitFlags)
	if explicitCtx.Client == nil || explicitCtx.Client.Kind != "" {
		t.Fatalf("explicit client = %+v, want missing kind", explicitCtx.Client)
	}
	if got := mustTransport(explicitCtx.Client); got != config.TransportTCP {
		t.Errorf("standalone config transport = %q, want tcp", got)
	}
}

func TestResolveOpenTarget(t *testing.T) {
	p := &config.Project{Services: map[string]int{"api": 3000, "web": 8080}}

	// A service name → its port, with the name as the label.
	if port, label, err := resolveOpenTarget("api", p); err != nil || port != 3000 || label != "api" {
		t.Errorf("service api: got (%d, %q, %v), want (3000, api, nil)", port, label, err)
	}
	// A bare port → that port, no service label (caller walks the naming ladder).
	if port, label, err := resolveOpenTarget("4001", p); err != nil || port != 4001 || label != "" {
		t.Errorf("port 4001: got (%d, %q, %v), want (4001, \"\", nil)", port, label, err)
	}
	// nil project + port still works.
	if port, _, err := resolveOpenTarget("3000", nil); err != nil || port != 3000 {
		t.Errorf("nil project port: got (%d, %v)", port, err)
	}
	// Unknown name with services → error that lists the services.
	if _, _, err := resolveOpenTarget("nope", p); err == nil || !strings.Contains(err.Error(), "api") {
		t.Errorf("unknown service: err = %v, want one listing api/web", err)
	}
	// Unknown name with no services → an invalid-port error.
	if _, _, err := resolveOpenTarget("nope", &config.Project{}); err == nil {
		t.Error("unknown target, no services: want an error")
	}
	// Out-of-range port number.
	if _, _, err := resolveOpenTarget("70000", nil); err == nil {
		t.Error("port 70000: want an error")
	}
}

func TestChooseOpenArg(t *testing.T) {
	one := &config.Project{Services: map[string]int{"api": 3000}}
	many := &config.Project{Services: map[string]int{"api": 3000, "web": 8080}}

	// A single positional passes straight through (port or service name).
	if arg, msg := chooseOpenArg(1, "3000", many); arg != "3000" || msg != "" {
		t.Errorf("one positional: got (%q, %q), want (3000, \"\")", arg, msg)
	}
	if arg, msg := chooseOpenArg(1, "api", many); arg != "api" || msg != "" {
		t.Errorf("named positional: got (%q, %q), want (api, \"\")", arg, msg)
	}
	// Too many args → usage error.
	if arg, msg := chooseOpenArg(2, "a", many); arg != "" || msg != openUsageMsg {
		t.Errorf("two args: got (%q, %q), want (\"\", usage)", arg, msg)
	}
	// No arg + a sole service → that service.
	if arg, msg := chooseOpenArg(0, "", one); arg != "api" || msg != "" {
		t.Errorf("bare + sole service: got (%q, %q), want (api, \"\")", arg, msg)
	}
	// No arg + multiple services → a pick-one hint listing them.
	if arg, msg := chooseOpenArg(0, "", many); arg != "" || !strings.Contains(msg, "pick one") || !strings.Contains(msg, "api|web") {
		t.Errorf("bare + many services: got (%q, %q), want pick-one hint", arg, msg)
	}
	// No arg + no services → usage.
	if arg, msg := chooseOpenArg(0, "", nil); arg != "" || msg != openUsageMsg {
		t.Errorf("bare + no project: got (%q, %q), want (\"\", usage)", arg, msg)
	}
}

func TestEffectiveLabel(t *testing.T) {
	cases := []struct{ as, from, svc, want string }{
		{"", "", "api", "api"},    // service name becomes the label
		{"foo", "", "api", "foo"}, // --as wins over the service name
		{"", "branch", "api", ""}, // --from set → no service label (ladder derives)
		{"", "", "", ""},          // plain port → empty (ladder → port)
		{"foo", "", "", "foo"},    // --as with no service
	}
	for _, c := range cases {
		if got := effectiveLabel(c.as, c.from, c.svc); got != c.want {
			t.Errorf("effectiveLabel(%q,%q,%q) = %q, want %q", c.as, c.from, c.svc, got, c.want)
		}
	}
}

func TestSoleService(t *testing.T) {
	if _, _, ok := soleService(nil); ok {
		t.Error("nil project: ok=true, want false")
	}
	if _, _, ok := soleService(&config.Project{}); ok {
		t.Error("no services: ok=true, want false")
	}
	if n, port, ok := soleService(&config.Project{Services: map[string]int{"api": 3000}}); !ok || n != "api" || port != 3000 {
		t.Errorf("one service: got (%q, %d, %v), want (api, 3000, true)", n, port, ok)
	}
	if _, _, ok := soleService(&config.Project{Services: map[string]int{"api": 3000, "web": 8080}}); ok {
		t.Error("two services: ok=true, want false (ambiguous)")
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
