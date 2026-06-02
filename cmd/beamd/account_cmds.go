package main

// Account + scope management: `beamd accounts`, `beamd orgs`, `beamd whoami`,
// `beamd default`, `beamd logout`. Login creates/updates accounts (see
// loginCmd); these list, inspect, set the default scope, and remove them.
// Replaces the old `beamd use` / `beamd profiles` (see
// docs/identity-and-accounts.md).

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/naming"
)

// defaultCmd shows or sets the current account's default scope — a set-once
// preference, not a sticky mode. `beamd default` prints it; `beamd default
// acme` sets it. Overridden per-command by --scope and per-repo by .beamd.
func defaultCmd(args []string) {
	fs := flag.NewFlagSet("default", flag.ExitOnError)
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)
	if rc.Account == nil {
		fmt.Fprintln(os.Stderr, orMsg(rc.authErr, "no account — run `beamd login`"))
		os.Exit(2)
	}

	if fs.NArg() == 0 {
		fmt.Println(orDash(rc.Account.DefaultScope))
		return
	}
	scope := fs.Arg(0)
	if err := naming.ValidateLabel(scope); err != nil {
		fmt.Fprintf(os.Stderr, "invalid scope %q (must be a simple name like `acme`)\n", scope)
		os.Exit(2)
	}
	rc.Account.DefaultScope = scope
	if err := config.SaveAccount(rc.Account); err != nil {
		fmt.Fprintln(os.Stderr, "save account:", err)
		os.Exit(1)
	}
	fmt.Printf("default scope for %s is now %q\n", rc.Account.Server, scope)
}

// accountsCmd lists servers logged into, marking the current one.
func accountsCmd(args []string) {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON array and nothing else")
	_ = fs.Parse(args)

	accts, err := config.ListAccounts()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list accounts:", err)
		os.Exit(1)
	}
	g, _ := config.LoadGlobal()

	type row struct {
		Server       string `json:"server"`
		Kind         string `json:"kind"`
		DefaultScope string `json:"defaultScope,omitempty"`
		Current      bool   `json:"current"`
	}
	rows := []row{}
	for _, a := range accts {
		rows = append(rows, row{Server: a.Server, Kind: accountKind(a), DefaultScope: a.DefaultScope, Current: a.Server == g.Current})
	}

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(rows)
		return
	}
	if len(rows) == 0 {
		fmt.Println("(no accounts — run `beamd login`)")
		return
	}
	for _, r := range rows {
		marker := " "
		if r.Current {
			marker = "*"
		}
		fmt.Printf("%s %-30s %s\n", marker, r.Server, r.Kind)
	}
}

// orgsCmd lists the orgs/scopes the current account can act in (hosted). An
// OSS account has no org concept; say so plainly.
func orgsCmd(args []string) {
	fs := flag.NewFlagSet("orgs", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON array and nothing else")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)
	if rc.Account == nil {
		fmt.Fprintln(os.Stderr, orMsg(rc.authErr, "no account — run `beamd login`"))
		os.Exit(2)
	}

	if *jsonOut {
		scopes := rc.Account.Scopes
		if scopes == nil {
			scopes = []config.ScopeRef{}
		}
		_ = json.NewEncoder(os.Stdout).Encode(scopes)
		return
	}
	if len(rc.Account.Scopes) == 0 {
		fmt.Printf("%s has no org concept (self-hosted) — tunnels route by your token\n", rc.Account.Server)
		return
	}
	def := rc.Account.DefaultScope
	for _, s := range rc.Account.Scopes {
		marker := " "
		if s.Slug == def {
			marker = "*"
		}
		fmt.Printf("%s %-22s %s\n", marker, s.Slug, orDash(s.Role))
	}
}

// whoamiCmd reports the resolved account + scope for the current context.
func whoamiCmd(args []string) {
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON object and nothing else")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)
	if rc.Account == nil {
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(struct {
				LoggedIn bool `json:"loggedIn"`
			}{false})
			return
		}
		fmt.Println("not logged in — run `beamd login`")
		return
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Server string `json:"server"`
			Kind   string `json:"kind"`
			Scope  string `json:"scope"`
		}{rc.Account.Server, accountKind(rc.Account), rc.Scope})
		return
	}
	fmt.Printf("server: %s\n", rc.Account.Server)
	fmt.Printf("kind:   %s\n", accountKind(rc.Account))
	fmt.Printf("scope:  %s\n", orDash(rc.Scope))
}

// logoutCmd removes an account (default: current). If the removed account was
// current, it repoints current at another account (or clears it).
func logoutCmd(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	serverFlag := fs.String("server", "", "account (edge) to remove (default: current)")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"server": true}))

	g, err := config.LoadGlobal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load global config:", err)
		os.Exit(1)
	}

	server := normalizeServerAddr(*serverFlag)
	if server == "" {
		server = g.Current
	}
	if server == "" {
		if accts, _ := config.ListAccounts(); len(accts) == 1 {
			server = accts[0].Server
		}
	}
	if server == "" {
		fmt.Fprintln(os.Stderr, "logout: no account specified and no current account")
		os.Exit(2)
	}
	if !config.AccountExists(server) {
		fmt.Fprintf(os.Stderr, "not logged into %q\n", server)
		os.Exit(2)
	}
	if err := config.DeleteAccount(server); err != nil {
		fmt.Fprintln(os.Stderr, "remove account:", err)
		os.Exit(1)
	}

	if g.Current == server {
		remaining, _ := config.ListAccounts()
		if len(remaining) > 0 {
			g.Current = remaining[0].Server
		} else {
			g.Current = ""
		}
		_ = config.SaveGlobal(g)
	}
	fmt.Printf("logged out of %s\n", server)
	if g.Current != "" && g.Current != server {
		fmt.Printf("current account is now %s\n", g.Current)
	}
}

// accountKind returns the account's kind, defaulting to "token" for accounts
// saved before the field existed / OSS edges.
func accountKind(a *config.Account) string {
	if a.Kind == "" {
		return "token"
	}
	return a.Kind
}

// orMsg returns primary if non-empty, else fallback.
func orMsg(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
