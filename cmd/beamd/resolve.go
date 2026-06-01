package main

// Identity + naming resolution shared by every client command. One
// resolveContext() returns a concrete {server, token} + agent socket + the
// project/global context for naming — commands consume *that*, never a
// profile name, so the personal→shared (profile→server) expansion in the
// spec is additive (see "Evolving personal → shared").

import (
	"flag"
	"fmt"
	"os"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/naming"
)

// clientFlags are the identity-selection flags common to client commands.
// An explicit --config bypasses profiles entirely (the automation path);
// otherwise -p/--profile selects among ~/.beamd/profiles.
type clientFlags struct {
	profile *string
	config  *string
}

func addClientFlags(fs *flag.FlagSet) *clientFlags {
	cf := &clientFlags{}
	cf.profile = fs.String("profile", "", "profile to use (default: the current profile)")
	fs.StringVar(cf.profile, "p", "", "shorthand for --profile")
	cf.config = fs.String("config", "", "explicit client config path (bypasses profiles; the automation path)")
	return cf
}

// clientFlagValueNames are the value-consuming flags to hoist so they can
// appear after positionals (e.g. `beamd open 3000 -p work`).
func clientFlagValueNames(extra ...string) map[string]bool {
	m := map[string]bool{"profile": true, "p": true, "config": true}
	for _, e := range extra {
		m[e] = true
	}
	return m
}

// tunnelContext is the resolved identity + context. Client is nil when no
// usable profile was found; commands that need auth call mustAuth().
type tunnelContext struct {
	Client      *config.Client  // {server, token} — nil if unresolved
	Profile     string          // selected profile name ("" when --config was used)
	ConfigPath  string          // path to hand the detached agent (profile file or --config)
	AgentSocket string          // this profile's detached-agent socket
	Project     *config.Project // nearest .beamd (may be nil)
	Global      *config.Global  // global config (current + naming defaults)
	Cwd         string
	authErr     string // why Client is nil/unusable, as an actionable message
}

// resolveContext runs the full identity ladder. It never exits for an
// unresolved profile (so read-only commands can operate on an absent agent);
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
		ctx.ConfigPath = *cf.config
		ctx.AgentSocket = c.AgentSocket
		ctx.loadProjectAndGlobal()
		return ctx
	}

	ctx.loadProjectAndGlobal()

	name, source, serverUnmatched := selectProfile(cf, ctx.Project, ctx.Global)
	ctx.Profile = name

	if name == "" {
		if serverUnmatched != "" {
			ctx.authErr = fmt.Sprintf(
				"this project tunnels through %q, but you're not logged into it — run `beamd login`",
				serverUnmatched)
		} else {
			ctx.authErr = "no profile configured — run `beamd login`"
		}
		return ctx
	}

	// A profile name becomes a filename under ~/.beamd; reject anything that
	// isn't a clean label so `-p ../x` can't escape the profiles dir.
	if err := naming.ValidateLabel(name); err != nil {
		ctx.authErr = fmt.Sprintf("invalid profile name %q (must be a simple name like `work`)", name)
		return ctx
	}

	pp, _ := config.ProfilePath(name)
	ctx.ConfigPath = pp
	if !config.ProfileExists(name) {
		if source == "project" {
			ctx.authErr = fmt.Sprintf(
				"this project uses profile %q, which isn't set up here — run `beamd login --profile %s`",
				name, name)
		} else {
			ctx.authErr = fmt.Sprintf(
				"profile %q not found — run `beamd login --profile %s` (or `beamd profiles` to list)",
				name, name)
		}
		ctx.AgentSocket, _ = config.AgentSocketFor(name) // best-effort for read-only cmds
		return ctx
	}

	c, err := config.LoadProfile(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load profile:", err)
		os.Exit(1)
	}
	c.Server = normalizeServerAddr(c.Server) // tolerate a port-less server:
	ctx.Client = c
	ctx.AgentSocket, _ = config.AgentSocketFor(name)
	return ctx
}

// loadProjectAndGlobal populates ctx.Project + ctx.Global, surfacing a parse
// error in either file rather than silently falling back (a corrupt config is
// a user error worth reporting, not hiding behind "no profile configured").
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

// selectProfile walks the profile ladder: -p/--profile → BEAMD_PROFILE →
// .beamd profile: → .beamd server: (matched to a local profile) →
// global current. Returns the chosen name, where it came from (for
// messaging), and — when a .beamd server: matched nothing — that server.
func selectProfile(cf *clientFlags, project *config.Project, global *config.Global) (name, source, serverUnmatched string) {
	if *cf.profile != "" {
		return *cf.profile, "flag", ""
	}
	if e := os.Getenv("BEAMD_PROFILE"); e != "" {
		return e, "env", ""
	}
	if project != nil && project.Profile != "" {
		return project.Profile, "project", ""
	}
	if project != nil && project.Server != "" {
		if n := findProfileByServer(project.Server); n != "" {
			return n, "project-server", ""
		}
		return "", "project-server", project.Server
	}
	if global != nil && global.Current != "" {
		return global.Current, "current", ""
	}
	return "", "", ""
}

// findProfileByServer returns the name of the local profile whose server
// matches `server` (so a committed `.beamd { server: … }` resolves for any
// teammate regardless of what they named their profile). "" if none match.
func findProfileByServer(server string) string {
	want := normalizeServerAddr(server)
	names, _ := config.ListProfiles()
	for _, n := range names {
		c, err := config.LoadProfile(n)
		if err != nil {
			continue
		}
		if normalizeServerAddr(c.Server) == want {
			return n
		}
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
