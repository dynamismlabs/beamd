package main

// Identity + naming resolution shared by every client command. One
// resolveContext() returns a concrete {server, token} credential + the
// resolved scope + agent socket + project/global context for naming —
// commands consume *that*, never an account/profile name.
//
// Two axes resolve the same way (CLI flag > project beamd.yaml > default):
//   - which server (→ which account)   via selectAccount
//   - which scope (org, within a login) via resolveScope
// An explicit --config bypasses all of it (the automation path). See
// docs/identity-and-accounts.md.

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/naming"
)

// clientFlags are the identity-selection flags common to client commands.
// --config bypasses accounts entirely (automation); otherwise --server picks
// which account (edge) and --scope picks the org within it.
type clientFlags struct {
	server *string
	scope  *string
	config *string
}

func addClientFlags(fs *flag.FlagSet) *clientFlags {
	cf := &clientFlags{}
	cf.server = fs.String("server", "", "edge server to target (default: your current account)")
	cf.scope = fs.String("scope", "", "org/scope for the tunnel (hosted; default: your default scope)")
	cf.config = fs.String("config", "", "explicit client config path (bypasses accounts; the automation path)")
	return cf
}

// clientFlagValueNames are the value-consuming flags to hoist so they can
// appear after positionals (e.g. `beamd open 3000 --server x`).
func clientFlagValueNames(extra ...string) map[string]bool {
	m := map[string]bool{"server": true, "scope": true, "config": true}
	for _, e := range extra {
		m[e] = true
	}
	return m
}

// tunnelContext is the resolved identity + context. Client is nil when no
// usable account was found; commands that need auth call mustAuth().
type tunnelContext struct {
	Account     *config.Account // the selected account (nil on --config or unresolved)
	Client      *config.Client  // {server, token} credential — nil if unresolved
	Server      string          // resolved server (for display/messaging)
	Scope       string          // resolved requested scope ("" = personal/default)
	ConfigPath  string          // path to hand the detached agent (account file or --config)
	AgentSocket string          // this account's detached-agent socket
	Project     *config.Project // nearest beamd.yaml (may be nil)
	ProjectDir  string          // dir the project file was found in ("" if none)
	Global      *config.Global  // global config (current + naming defaults)
	Cwd         string
	authErr     string // why Client is nil/unusable, as an actionable message
}

// resolveContext runs the full identity ladder. It never exits for an
// unresolved account (so read-only commands can operate on an absent agent);
// commands needing credentials call mustAuth().
func resolveContext(cf *clientFlags) *tunnelContext {
	ctx := &tunnelContext{}
	ctx.Cwd, _ = os.Getwd()

	// Explicit --config short-circuits the whole ladder (automation/Flow).
	if *cf.config != "" {
		c, err := config.LoadClient(*cf.config)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load config:", err)
			os.Exit(1)
		}
		c.Server = normalizeServerAddr(c.Server) // tolerate a port-less server:
		ctx.Client = c
		ctx.Server = c.Server
		ctx.ConfigPath = *cf.config
		ctx.AgentSocket = c.AgentSocket
		ctx.Scope = *cf.scope // honored if set; an API key's scope is fixed regardless
		ctx.loadProjectAndGlobal()
		return ctx
	}

	ctx.loadProjectAndGlobal()

	server, source := selectAccount(cf, ctx.Project, ctx.Global)
	ctx.Server = server

	if server == "" {
		if source == "ambiguous" {
			ctx.authErr = "you have multiple accounts — pass --server <edge>, run `beamd link`, or set a current one with `beamd login`"
		} else {
			ctx.authErr = "no account configured — run `beamd login`"
		}
		ctx.Scope = *cf.scope
		return ctx
	}

	if !config.AccountExists(server) {
		if source == "project" {
			ctx.authErr = fmt.Sprintf(
				"this project tunnels through %q, but you're not logged into it — run `beamd login --server %s`",
				server, server)
		} else {
			ctx.authErr = fmt.Sprintf("not logged into %q — run `beamd login --server %s`", server, server)
		}
		ctx.AgentSocket, _ = config.AgentSocketFor(server) // best-effort for read-only cmds
		ctx.Scope = *cf.scope
		return ctx
	}

	a, err := config.LoadAccount(server)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load account:", err)
		os.Exit(1)
	}
	a.Server = normalizeServerAddr(a.Server) // tolerate a port-less server:
	ctx.Account = a
	ctx.Client = a.Client()
	ctx.ConfigPath, _ = config.AccountPath(server)
	ctx.AgentSocket, _ = config.AgentSocketFor(server)
	ctx.Scope = resolveScope(cf, ctx.Project, a)
	return ctx
}

// loadProjectAndGlobal populates ctx.Project + ctx.Global, surfacing a parse
// error in either file rather than silently falling back.
func (ctx *tunnelContext) loadProjectAndGlobal() {
	p, dir, perr := config.DiscoverProject(ctx.Cwd)
	if perr != nil {
		fmt.Fprintln(os.Stderr, "read beamd.yaml:", perr)
		os.Exit(1)
	}
	g, gerr := config.LoadGlobal()
	if gerr != nil {
		fmt.Fprintln(os.Stderr, "read ~/.beamd/config:", gerr)
		os.Exit(1)
	}
	ctx.Project = p
	ctx.ProjectDir = dir
	ctx.Global = g
}

// selectAccount walks the server ladder: --server → BEAMD_SERVER → beamd.yaml
// server: → global current → the only account. Returns the chosen server
// (normalized) and where it came from. "" with source "ambiguous" means
// multiple accounts and nothing selected one.
func selectAccount(cf *clientFlags, project *config.Project, global *config.Global) (server, source string) {
	if *cf.server != "" {
		return normalizeServerAddr(*cf.server), "flag"
	}
	if e := os.Getenv("BEAMD_SERVER"); e != "" {
		return normalizeServerAddr(e), "env"
	}
	if project != nil && project.Server != "" {
		return normalizeServerAddr(project.Server), "project"
	}
	if global != nil && global.Current != "" {
		// Honor the saved current only if it still names a real account. A
		// dangling pointer (left by an old binary, a removed account, a
		// hand-edited config) must not shadow a valid single login or surface a
		// nonsense "log into <ghost>" error — fall through the ladder instead.
		// (flag/env/project are left to error on a missing account: those are
		// explicit per-command intent, so "not logged into X" is the right hint.)
		if cur := normalizeServerAddr(global.Current); config.AccountExists(cur) {
			return cur, "current"
		}
	}
	accts, _ := config.ListAccounts()
	switch len(accts) {
	case 0:
		return "", ""
	case 1:
		return normalizeServerAddr(accts[0].Server), "only"
	default:
		return "", "ambiguous"
	}
}

// resolveScope walks the scope ladder: --scope → beamd.yaml scope: → the account's
// default scope → personal (""). An empty result means "the server picks"
// (personal for a session, the fixed slug for an OSS/key account).
func resolveScope(cf *clientFlags, project *config.Project, a *config.Account) string {
	if *cf.scope != "" {
		return *cf.scope
	}
	if project != nil && project.Scope != "" {
		return project.Scope
	}
	if a != nil && a.DefaultScope != "" {
		return a.DefaultScope
	}
	return ""
}

// mustAuth returns the resolved Client, or exits with the recorded,
// actionable reason if no usable identity was found.
func (ctx *tunnelContext) mustAuth() *config.Client {
	if ctx.Client == nil || ctx.Client.Server == "" || ctx.Client.Token == "" {
		msg := ctx.authErr
		if msg == "" {
			msg = "missing server or token — run `beamd login` first"
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(2)
	}
	return ctx.Client
}

// resolveLabel applies the naming ladder (§2): --as / --from → beamd.yaml
// name:/from: → global defaults → port. Returns a concrete, validated label
// (never empty; defaults to the port number).
func resolveLabel(asFlag, fromFlag string, ctx *tunnelContext, port int) (string, error) {
	// Explicit flags win; --as (literal) beats --from (derive).
	if asFlag != "" {
		if err := naming.ValidateLabel(asFlag); err != nil {
			return "", fmt.Errorf("--as %q: %w", asFlag, err)
		}
		return asFlag, nil
	}
	if fromFlag != "" {
		return naming.DeriveLabel(fromFlag, port, ctx.Cwd)
	}
	// Project beamd.yaml: literal name beats a derive source.
	if ctx.Project != nil {
		if ctx.Project.Name != "" {
			if err := naming.ValidateLabel(ctx.Project.Name); err != nil {
				return "", fmt.Errorf("beamd.yaml name %q: %w", ctx.Project.Name, err)
			}
			return ctx.Project.Name, nil
		}
		if ctx.Project.From != "" {
			return naming.DeriveLabel(ctx.Project.From, port, ctx.Cwd)
		}
	}
	// Global defaults.
	if ctx.Global != nil {
		if ctx.Global.Defaults.Name != "" {
			if err := naming.ValidateLabel(ctx.Global.Defaults.Name); err != nil {
				return "", fmt.Errorf("global default name %q: %w", ctx.Global.Defaults.Name, err)
			}
			return ctx.Global.Defaults.Name, nil
		}
		if ctx.Global.Defaults.From != "" {
			return naming.DeriveLabel(ctx.Global.Defaults.From, port, ctx.Cwd)
		}
	}
	// Built-in default: the port number.
	return naming.LabelFromPort(port), nil
}

// resolveOpenTarget maps an `open` argument to a concrete local port plus a
// default label. An argument matching a beamd.yaml `services:` name resolves to
// that service's port and returns the name as serviceLabel (so `beamd open api`
// lands on api-<slug>.<base>); otherwise the argument must be a port number and
// serviceLabel is "" (the caller then walks the normal naming ladder).
func resolveOpenTarget(arg string, p *config.Project) (port int, serviceLabel string, err error) {
	if p != nil {
		if pt, ok := p.Services[arg]; ok {
			if verr := naming.ValidateLabel(arg); verr != nil {
				return 0, "", fmt.Errorf("service %q in %s is not a valid subdomain label: %w", arg, config.ProjectFile, verr)
			}
			if pt < 1 || pt > 65535 {
				return 0, "", fmt.Errorf("service %q in %s has an invalid port %d", arg, config.ProjectFile, pt)
			}
			return pt, arg, nil
		}
	}
	if n, aerr := strconv.Atoi(arg); aerr == nil {
		if n < 1 || n > 65535 {
			return 0, "", fmt.Errorf("invalid port: %s", arg)
		}
		return n, "", nil
	}
	if names := serviceNames(p); len(names) > 0 {
		return 0, "", fmt.Errorf("%q is not a port or a service — %s defines: %s", arg, config.ProjectFile, strings.Join(names, ", "))
	}
	return 0, "", fmt.Errorf("invalid port %q (or define it as a service in %s)", arg, config.ProjectFile)
}

// soleService returns the single defined service when the repo defines exactly
// one, so a bare `beamd open` can target it without an argument.
func soleService(p *config.Project) (name string, port int, ok bool) {
	if p == nil || len(p.Services) != 1 {
		return "", 0, false
	}
	for n, pt := range p.Services {
		return n, pt, true
	}
	return "", 0, false
}

// serviceNames returns the repo's service names, sorted, for stable display.
func serviceNames(p *config.Project) []string {
	if p == nil || len(p.Services) == 0 {
		return nil
	}
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const openUsageMsg = "usage: beamd open <port|service> [--as name | --from src] [-d] [--json]"

// chooseOpenArg picks the `open` target from the parsed positional args and the
// project: the single positional if given, else the repo's sole service when no
// arg is passed. It returns the chosen argument, or a non-empty errMsg the
// caller should print before exiting (usage, or a "pick one of N services"
// hint). Pure, so the branching is unit-testable without the command machinery.
func chooseOpenArg(nargs int, arg0 string, p *config.Project) (arg, errMsg string) {
	switch {
	case nargs == 1:
		return arg0, ""
	case nargs > 1:
		return "", openUsageMsg
	}
	// No positional: fall back to the repo's sole service, if any.
	if name, _, ok := soleService(p); ok {
		return name, ""
	}
	if names := serviceNames(p); len(names) > 1 {
		return "", fmt.Sprintf("%s defines multiple services — pick one: beamd open <%s>", config.ProjectFile, strings.Join(names, "|"))
	}
	return "", openUsageMsg
}

// effectiveLabel resolves what to feed the naming ladder as the `--as` value: a
// matched service contributes its own name as the label, but only when the user
// didn't override naming explicitly with --as or --from. Precedence:
// --as > --from(derive) > service name > (the rest of the ladder).
func effectiveLabel(asFlag, fromFlag, serviceLabel string) string {
	if asFlag == "" && fromFlag == "" && serviceLabel != "" {
		return serviceLabel
	}
	return asFlag
}
