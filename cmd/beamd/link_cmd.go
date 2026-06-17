package main

// `beamd link` writes a beamd.yaml in the current directory that pins this
// repo to an org (scope) and edge, so tunnels started here use them by default
// — without depending on the machine-wide current account. The file is safe to
// commit (just a hostname + a slug, never a token). Mirrors `vercel link` /
// `netlify link`. A --local variant writes the gitignored beamd.local.yaml
// overlay instead. See docs/identity-and-accounts.md.

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/naming"
)

func linkCmd(args []string) {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	scopeFlag := fs.String("scope", "", "org/scope to pin (default: pick interactively, or your default)")
	nameFlag := fs.String("name", "", "fixed tunnel label for this repo (default: none — derive from the port)")
	fromFlag := fs.String("from", "", "derive the label from one of: "+strings.Join(naming.DeriveSources, ", "))
	serverFlag := fs.String("server", "", "edge to pin (default: your current account's edge)")
	local := fs.Bool("local", false, "write beamd.local.yaml (gitignored overlay) instead of beamd.yaml")
	force := fs.Bool("force", false, "overwrite an existing file")
	yes := fs.Bool("yes", false, "non-interactive: take defaults, never prompt")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"scope": true, "name": true, "from": true, "server": true}))

	if *nameFlag != "" && *fromFlag != "" {
		fmt.Fprintln(os.Stderr, "link: pass at most one of --name / --from")
		os.Exit(2)
	}

	// Resolve the account to link against (must be logged in). A minimal
	// clientFlags lets --server/--scope flow through the normal ladder.
	noConfig := ""
	rc := resolveContext(&clientFlags{server: serverFlag, scope: scopeFlag, config: &noConfig})
	if rc.Account == nil {
		fmt.Fprintln(os.Stderr, orMsg(rc.authErr, "no account — run `beamd login` first"))
		os.Exit(2)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "link:", err)
		os.Exit(1)
	}
	fileName := config.ProjectFile
	if *local {
		fileName = config.ProjectLocalFile
	}
	path := filepath.Join(cwd, fileName)
	if _, err := os.Stat(path); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "%s already exists — use --force to overwrite\n", fileName)
		os.Exit(2)
	}

	// Org / scope.
	scope := strings.TrimSpace(*scopeFlag)
	if scope == "" {
		scope = chooseScope(rc.Account, *yes)
	}
	if scope != "" {
		if err := naming.ValidateLabel(scope); err != nil {
			fmt.Fprintf(os.Stderr, "invalid scope %q (must be a simple name like `acme`)\n", scope)
			os.Exit(2)
		}
	}

	// Naming: --name (literal) or --from (derive); otherwise optionally prompt.
	name := strings.TrimSpace(*nameFlag)
	from := strings.TrimSpace(*fromFlag)
	if name == "" && from == "" && !*yes && isInteractive() {
		r := bufio.NewReader(os.Stdin)
		base := filepath.Base(cwd)
		if in := prompt(r, fmt.Sprintf("fixed tunnel name (optional — blank uses the port; this folder is %q)", base), ""); in != "" {
			name = in
		}
	}
	if name != "" {
		if err := naming.ValidateLabel(name); err != nil {
			fmt.Fprintf(os.Stderr, "invalid name %q: %v\n", name, err)
			os.Exit(2)
		}
	}
	if from != "" && !contains(naming.DeriveSources, from) {
		fmt.Fprintf(os.Stderr, "invalid --from %q (use one of: %s)\n", from, strings.Join(naming.DeriveSources, ", "))
		os.Exit(2)
	}

	// Pin the edge by bare host (drop the cosmetic :443; normalizeServerAddr
	// re-adds it on read), so the committed file reads cleanly.
	server := strings.TrimSuffix(rc.Account.Server, ":443")

	content := renderProjectFile(server, scope, name, from, *local)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}

	fmt.Printf("✓ wrote %s\n", fileName)
	summary := "edge " + server
	if scope != "" {
		summary = "org=" + scope + ", " + summary
	}
	fmt.Printf("  tunnels in this repo now use %s\n", summary)
	switch {
	case *local:
		fmt.Println("  this is a personal overlay — add beamd.local.yaml to .gitignore")
	case scope == "" && len(rc.Account.Scopes) == 0:
		fmt.Println("  commit this file to share the edge with your team")
	default:
		fmt.Println("  commit this file so your team shares the same defaults")
	}
}

// chooseScope picks the org to pin: nothing for a self-hosted/OSS account (no
// org concept), the lone/default scope when there's no real choice or we're
// non-interactive, else an interactive picker over the account's cached scopes.
func chooseScope(a *config.Account, noninteractive bool) string {
	if len(a.Scopes) == 0 {
		return "" // self-hosted / OSS edge: tunnels route by the token, no org
	}
	def := a.DefaultScope
	if def == "" {
		def = a.Scopes[0].Slug
	}
	if len(a.Scopes) == 1 || noninteractive || !isInteractive() {
		return def
	}

	r := bufio.NewReader(os.Stdin)
	fmt.Println("Which org should this repo use? (* = your default)")
	for i, s := range a.Scopes {
		marker := " "
		if s.Slug == def {
			marker = "*"
		}
		fmt.Printf("  %s %d) %-22s %s\n", marker, i+1, s.Slug, orDash(s.Role))
	}
	for {
		in := prompt(r, fmt.Sprintf("org [1-%d or name]", len(a.Scopes)), def)
		if n, err := strconv.Atoi(in); err == nil {
			if n >= 1 && n <= len(a.Scopes) {
				return a.Scopes[n-1].Slug
			}
			fmt.Printf("  pick a number 1-%d\n", len(a.Scopes))
			continue
		}
		return in // a typed slug (validated by the caller)
	}
}

// renderProjectFile builds the committable YAML with a self-documenting header,
// emitting only the fields that are set.
func renderProjectFile(server, scope, name, from string, local bool) string {
	var b strings.Builder
	if local {
		b.WriteString("# beamd local overrides — DO NOT COMMIT (add to .gitignore).\n")
		b.WriteString("# Overlays beamd.yaml for your machine only, like .env.local.\n")
	} else {
		b.WriteString("# beamd project config — safe to commit (no secrets: just a hostname + slug).\n")
		b.WriteString("# Tunnels started in this repo use these defaults. Override per-command with\n")
		b.WriteString("# --server / --scope / --as, or locally with beamd.local.yaml (gitignored).\n")
	}
	fmt.Fprintf(&b, "server: %s\n", server)
	if scope != "" {
		fmt.Fprintf(&b, "scope: %s\n", scope)
	}
	if name != "" {
		fmt.Fprintf(&b, "name: %s\n", name)
	}
	if from != "" {
		fmt.Fprintf(&b, "from: %s\n", from)
	}
	return b.String()
}
