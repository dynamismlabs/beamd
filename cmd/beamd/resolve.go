package main

// Identity + naming resolution shared by every client command. One
// resolveContext() returns a concrete {server, token} credential + the
// resolved scope + agent socket + project/global context for naming —
// commands consume *that*, never an account/profile name.
//
// Two axes resolve the same way (CLI flag > project .beamd > default):
//   - which server (→ which account)   via selectAccount
//   - which scope (org, within a login) via resolveScope
// An explicit --config bypasses all of it (the automation path). See
// docs/identity-and-accounts.md.

import (
	"flag"
	"fmt"
	"os"

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
	Project     *config.Project // nearest .beamd (may be nil)
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
			ctx.authErr = "you have multiple accounts — pass --server <edge>, add a .beamd, or set a current one with `beamd login`"
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
	p, _, perr := config.DiscoverProject(ctx.Cwd)
	if perr != nil {
		fmt.Fprintln(os.Stderr, "read .beamd:", perr)
		os.Exit(1)
	}
	g, gerr := config.LoadGlobal()
	if gerr != nil {
		fmt.Fprintln(os.Stderr, "read ~/.beamd/config:", gerr)
		os.Exit(1)
	}
	ctx.Project = p
	ctx.Global = g
}

// selectAccount walks the server ladder: --server → BEAMD_SERVER → .beamd
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
		return normalizeServerAddr(global.Current), "current"
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

// resolveScope walks the scope ladder: --scope → .beamd scope: → the account's
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

// resolveLabel applies the naming ladder (§2): --as / --from → .beamd
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
	// Project .beamd: literal name beats a derive source.
	if ctx.Project != nil {
		if ctx.Project.Name != "" {
			if err := naming.ValidateLabel(ctx.Project.Name); err != nil {
				return "", fmt.Errorf(".beamd name %q: %w", ctx.Project.Name, err)
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
