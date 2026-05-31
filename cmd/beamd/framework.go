package main

// Make `beamd run -- <framework>` work with any dev server, not just ones
// that honor $PORT. Three problems, three fixes (full rationale in the
// profiles-and-naming spec, "run makes any framework reachable"):
//
//  1. A tunnel surfaces a remote Host; Vite/Next reject it → inject the
//     allowed-hosts env keyed to the tunnel domain.
//  2. Some frameworks ignore $PORT → inject --port (+ --strictPort/--host).
//  3. Project-local binaries aren't on PATH → run via sh with
//     node_modules/.bin prepended so `beamd run -- vite` resolves.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// frameworksNeedingPort maps a command basename to whether it supports
// --strictPort (so it hard-fails instead of drifting off the port we
// tunnel). These ignore the $PORT env var.
var frameworksNeedingPort = map[string]struct{ strictPort bool }{
	"vite":         {true},
	"vp":           {true}, // vite-plus
	"react-router": {true},
	"rsbuild":      {false},
	"astro":        {false},
	"ng":           {false}, // angular
	"react-native": {false},
	"expo":         {false},
}

// packageRunners maps a runner basename to the subcommands that run a
// package (empty = the runner's first non-flag arg is the binary).
var packageRunners = map[string][]string{
	"npx":  {},
	"bunx": {},
	"pnpx": {},
	"yarn": {"dlx", "exec"},
	"pnpm": {"dlx", "exec"},
}

// findFrameworkBasename returns the framework basename inside args, looking
// past package runners (npx/bunx/yarn dlx/…) and their flags. "" if none.
func findFrameworkBasename(args []string) string {
	if len(args) == 0 {
		return ""
	}
	first := filepath.Base(args[0])
	if _, ok := frameworksNeedingPort[first]; ok {
		return first
	}
	subs, isRunner := packageRunners[first]
	if !isRunner {
		return ""
	}
	i := 1
	if len(subs) > 0 {
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			i++
		}
		if i >= len(args) {
			return ""
		}
		if !contains(subs, args[i]) {
			// Not a known subcommand — maybe an implicit bin (`yarn vite`).
			name := filepath.Base(args[i])
			if _, ok := frameworksNeedingPort[name]; ok {
				return name
			}
			return ""
		}
		i++
	}
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		i++
	}
	if i >= len(args) {
		return ""
	}
	name := filepath.Base(args[i])
	if _, ok := frameworksNeedingPort[name]; ok {
		return name
	}
	return ""
}

// injectFrameworkFlags appends --port (and --strictPort/--host where apt)
// to args when they invoke a framework that ignores $PORT. Idempotent: it
// skips a flag the user already supplied. Returns the (possibly extended)
// args and whether anything was injected.
func injectFrameworkFlags(args []string, port int) ([]string, bool) {
	basename := findFrameworkBasename(args)
	if basename == "" {
		return args, false
	}
	fw := frameworksNeedingPort[basename]
	changed := false
	if !hasFlag(args, "port") {
		args = append(args, "--port", fmt.Sprintf("%d", port))
		if fw.strictPort {
			args = append(args, "--strictPort")
		}
		changed = true
	}
	if !hasFlag(args, "host") {
		// Expo's --host is a connection mode (lan|localhost|tunnel), not a
		// bind address; use localhost there. Everyone else binds IPv4 loopback
		// so the client's 127.0.0.1 dial reaches them.
		host := "127.0.0.1"
		if basename == "expo" {
			host = "localhost"
		}
		args = append(args, "--host", host)
		changed = true
	}
	return args, changed
}

// childEnv builds the environment for the wrapped command: the parent env
// with PORT/HOST set, BEAMD_URL (the public URL, so the app can
// self-reference), the Vite allowed-hosts opener keyed to the tunnel domain,
// and node_modules/.bin prepended to PATH.
func childEnv(port int, publicURL, baseSuffix, cwd string) []string {
	out := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") {
			continue // replaced below with the augmented value
		}
		out = append(out, kv)
	}
	out = append(out,
		"PATH="+augmentPath(cwd),
		fmt.Sprintf("PORT=%d", port),
		"HOST=127.0.0.1",
		"BEAMD_URL="+publicURL,
		// Vite reads this to allow the tunnel host; baseSuffix is ".<slug>.<base>".
		"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS="+baseSuffix,
	)
	return out
}

// augmentPath prepends node_modules/.bin from cwd up to the filesystem root
// (covers monorepos) to the existing PATH, so project-local CLIs resolve.
func augmentPath(cwd string) string {
	base := os.Getenv("PATH")
	var bins []string
	dir := cwd
	for {
		bin := filepath.Join(dir, "node_modules", ".bin")
		if fi, err := os.Stat(bin); err == nil && fi.IsDir() {
			bins = append(bins, bin)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if len(bins) == 0 {
		return base
	}
	return strings.Join(bins, string(os.PathListSeparator)) + string(os.PathListSeparator) + base
}

// nextAllowedOriginsHint returns a one-line hint when the command invokes the
// Next.js dev server (whose allowed origins are config-only, not env), or ""
// otherwise. Matches the `next` binary by basename so unrelated args
// containing the substring "next" don't trigger it.
func nextAllowedOriginsHint(args []string, host string) string {
	isNext := false
	for _, a := range args {
		if filepath.Base(a) == "next" {
			isNext = true
			break
		}
	}
	if !isNext {
		return ""
	}
	return fmt.Sprintf(
		"hint: Next.js dev rejects cross-origin hosts — add to next.config.js:\n"+
			"      allowedDevOrigins: [%q]", host)
}

// hasFlag reports whether args already contains --<name> in either the
// space-separated (`--port 3000`) or equals (`--port=3000`) form, so
// injection stays idempotent against a flag the user already passed.
func hasFlag(args []string, name string) bool {
	pfx := "--" + name
	for _, a := range args {
		if a == pfx || strings.HasPrefix(a, pfx+"=") {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
