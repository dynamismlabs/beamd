package main

// Client-side subcommands of the single `beamd` binary: login, up, down,
// list, and the internal `agent` background worker. The edge/server
// subcommands (serve, init, add-developer, provision-dev) live in
// main.go; both roles ship in one binary.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/daemon"
	"github.com/dynamismlabs/beamd/internal/devicecode"
	"github.com/dynamismlabs/beamd/internal/naming"
)

func defaultConfigPath() string {
	p, err := config.DefaultClientPath()
	if err != nil {
		return "" // caller will get a clearer error from LoadClient
	}
	return p
}

// normalizeServerAddr tolerates a pasted scheme/trailing slash and adds the
// default :443 when no port is given, so users can type just the host.
func normalizeServerAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimSuffix(addr, "/")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443") // no port → default to 443
	}
	return addr
}

// isInteractive reports whether stdin is a terminal, so we can prompt the
// user — vs a pipe/redirect/CI, where we must not block on a prompt.
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// DefaultHost is the hosted control plane the CLI logs in against — the *control
// plane* (the web app), NOT an edge (see docs/identity-and-accounts.md). A bare
// `beamd login` device-code logs in here; the login then assigns the actual edge
// (paid vs free live on different domains).
//
// It defaults to production in code so the published CLI Just Works with no build
// flag. Point it at a staging / self-hosted control plane at runtime with the
// BEAMD_DEFAULT_HOST env var (like `GH_HOST` for the gh CLI) — same published
// binary, different environment. A `-X main.DefaultHost=…` build override still
// works for special builds. (Self-hosters can also ignore all this and use
// `--server <edge> --token`.)
var DefaultHost = "beamd.ai"

// controlPlaneHost is the effective hosted control plane: the BEAMD_DEFAULT_HOST
// env override if set, else the baked-in default. Lets the same published binary
// target staging/dev without a special build.
func controlPlaneHost() string {
	if h := os.Getenv("BEAMD_DEFAULT_HOST"); h != "" {
		return h
	}
	return DefaultHost
}

func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "beamd edge address (default: the hosted service; required for self-host)")
	token := fs.String("token", "", "bearer token / API key (copy-paste); omit for device-code login")
	scope := fs.String("scope", "", "default scope for this account (hosted; default: personal)")
	insecure := fs.Bool("insecure", false, "skip TLS verification for the discovery + device-code calls (dev/self-signed setups)")
	configPath := fs.String("config", "", "write to an explicit config path instead of an account (automation)")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"server": true, "token": true, "scope": true, "config": true}))

	// Hosted default: a bare `beamd login` (no server, no token) targets the
	// control plane (production by default, or BEAMD_DEFAULT_HOST) and does
	// browser/device-code login. (A pasted --token comes with its own edge, so it
	// always needs --server.)
	if *server == "" && *token == "" {
		*server = controlPlaneHost()
	}
	if *server == "" && isInteractive() {
		r := bufio.NewReader(os.Stdin)
		fmt.Println("Connect this machine to a beamd edge.")
		*server = prompt(r, "edge address (e.g. tunnel.example.com)", "")
	}
	*server = normalizeServerAddr(*server)
	if *server == "" {
		fmt.Fprintln(os.Stderr, "login: --server is required for a self-hosted edge")
		os.Exit(2)
	}

	// The account is keyed by the edge tunnels flow through. For self-host/OSS
	// (or a pasted token) that's --server; for hosted device-code login the
	// edge is whatever the login assigns (paid vs free live on different
	// domains), so acctServer may switch to it below.
	acctServer := *server
	kind := "token"
	var scopes []config.ScopeRef

	if *token == "" {
		res, err := deviceCodeLogin(*server, *insecure)
		if err != nil {
			fmt.Fprintln(os.Stderr, "login:", err)
			os.Exit(1)
		}
		*token = res.Token
		kind = "session"
		if res.Edge != "" {
			acctServer = normalizeServerAddr(res.Edge)
		}
		for _, s := range res.Scopes {
			scopes = append(scopes, config.ScopeRef{Slug: s.Slug, Role: s.Role})
		}
	}

	// Explicit --config writes a standalone {server, token} config (the
	// automation path), bypassing the account store entirely.
	if *configPath != "" {
		cfg := &config.Client{Server: acctServer, Token: *token, InsecureSkipVerify: *insecure}
		if err := config.SaveClient(cfg, *configPath); err != nil {
			fmt.Fprintln(os.Stderr, "save config:", err)
			os.Exit(1)
		}
		fmt.Println("logged in")
		return
	}

	// Account path: keyed by the (possibly login-assigned) edge. The first
	// account created becomes current.
	defaultScope := *scope
	if defaultScope == "" && len(scopes) > 0 {
		defaultScope = scopes[0].Slug // personal/first as the standing default
	}
	transport, err := transportForAccountRefresh(acctServer)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load existing account:", err)
		os.Exit(1)
	}
	acct := &config.Account{
		Server:             acctServer,
		Token:              *token,
		Transport:          transport,
		Kind:               kind,
		InsecureSkipVerify: *insecure,
		Scopes:             scopes,
		DefaultScope:       defaultScope,
	}
	if err := config.SaveAccount(acct); err != nil {
		fmt.Fprintln(os.Stderr, "save account:", err)
		os.Exit(1)
	}
	g, err := config.LoadGlobal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load global config:", err)
		os.Exit(1)
	}
	// Adopt this account as the current default when there isn't a usable one
	// already — none set, or one left dangling (names an account that no longer
	// exists). A valid current for a *different* account is preserved, so a
	// multi-account user's choice isn't hijacked by a fresh login.
	if g.Current == "" || !config.AccountExists(normalizeServerAddr(g.Current)) {
		g.Current = acctServer
		if err := config.SaveGlobal(g); err != nil {
			fmt.Fprintln(os.Stderr, "save global config:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("logged in (%s)\n", acctServer)
	if g.Current != acctServer {
		fmt.Printf("current account is still %s — pass `--server %s` to target this one\n", g.Current, acctServer)
	}
}

// transportForAccountRefresh keeps an operator/user's explicit transport pin
// when login refreshes the credential for the same edge. An omitted transport
// remains omitted so the account kind continues to select the product default.
func transportForAccountRefresh(server string) (string, error) {
	if !config.AccountExists(server) {
		return "", nil
	}
	existing, err := config.LoadAccount(server)
	if err != nil {
		return "", err
	}
	return existing.Transport, nil
}

// deviceCodeLogin runs the no-token login flow: ask the control plane for its
// discovery payload, then do the device-code dance against whatever web app it
// points at. Returns the issued session + the assigned edge + scope set.
func deviceCodeLogin(server string, insecure bool) (*devicecode.Result, error) {
	hc := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	disc, err := devicecode.Discover(ctx, hc, server)
	if err != nil {
		return nil, fmt.Errorf(
			"discovery failed: %w\n  → pass --token <T> instead, or --insecure if the server's apex cert is self-signed",
			err,
		)
	}
	if disc == nil {
		return nil, fmt.Errorf(
			"this server does not advertise device-code login.\n  → pass --token <T> instead (your operator can issue one with `beamd add-developer`)",
		)
	}
	return devicecode.Login(ctx, hc, disc, os.Stderr)
}

// hoistFlags reorders args so flag tokens come before positional args.
// Go's flag package stops parsing at the first non-flag arg, so without
// this `beamd up 3001 --as hello` (the documented form) would treat
// `--as hello` as positionals. valueFlags names the flags that consume a
// following value token (so we move the value along with the flag).
func hoistFlags(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a != "-" && strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(a, "=") && valueFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// openResult is the JSON shape emitted by `beamd open --json` (both modes).
type openResult struct {
	URL        string `json:"url"`
	Name       string `json:"name"`
	Port       int    `json:"port"`
	Slug       string `json:"slug"`
	BaseDomain string `json:"baseDomain"`
}

func openCmd(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	asFlag := fs.String("as", "", "literal subdomain label (defaults to the port number)")
	fromFlag := fs.String("from", "", "derive the label from: port | dir | repo | branch | worktree")
	detach := fs.Bool("detach", false, "run in the background agent and return immediately (the path automation uses)")
	fs.BoolVar(detach, "d", false, "shorthand for --detach")
	jsonOut := fs.Bool("json", false, "print exactly one JSON object describing the tunnel, and nothing else")
	insecure := fs.Bool("insecure", false, "skip edge TLS verification (self-signed dev edges only)")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames("as", "from")))

	ctx := resolveContext(cf)

	// Target a port number, a beamd.yaml `services:` name, or — with no arg —
	// the repo's sole service.
	arg, errMsg := chooseOpenArg(fs.NArg(), fs.Arg(0), ctx.Project)
	if errMsg != "" {
		fmt.Fprintln(os.Stderr, errMsg)
		os.Exit(2)
	}

	port, svcLabel, terr := resolveOpenTarget(arg, ctx.Project)
	if terr != nil {
		fmt.Fprintln(os.Stderr, terr)
		os.Exit(2)
	}

	cfg := ctx.mustAuth()
	ins := *insecure || cfg.InsecureSkipVerify

	// A named service contributes its own name as the label unless --as/--from
	// override; then the normal naming ladder runs.
	label, err := resolveLabel(effectiveLabel(*asFlag, *fromFlag, svcLabel), *fromFlag, ctx, port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "name:", err)
		os.Exit(2)
	}

	if *detach {
		openDetached(ctx, port, label, *jsonOut, ins)
		return
	}
	openForeground(cfg, port, label, ctx.Scope, *jsonOut, ins)
}

// openForeground holds the tunnel in *this* process (the default, like
// ngrok): it opens its own connection to the edge, registers, prints the
// URL, and blocks until interrupted — then tears the tunnel down. No
// agent is involved. `label` is the already-resolved tunnel name.
func openForeground(cfg *config.Client, port int, label, scope string, jsonOut, insecure bool) {
	quietClientLogs()

	c, url, err := dialAndRegister(cfg, port, label, scope, insecure)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open failed:", err)
		os.Exit(1)
	}
	defer c.Close()

	if jsonOut {
		printOpenResult(openResult{URL: url, Name: label, Port: port, Slug: c.Slug(), BaseDomain: c.BaseDomain()})
	} else {
		fmt.Printf("%s  →  http://127.0.0.1:%d\n\ntunnel live — press Ctrl-C to stop\n", url, port)
	}

	// Block until interrupted. The client keeps the tunnel alive across
	// network blips on its own (reconnect + replay); we only exit on a
	// signal from the user.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	if !jsonOut {
		fmt.Fprintln(os.Stderr, "\nshutting down…")
	}
	_ = c.Close()
}

// openDetached hands the tunnel to the selected profile's background agent
// and returns immediately. This is the path automation (e.g. Flow) uses.
func openDetached(tc *tunnelContext, port int, label string, jsonOut, insecure bool) {
	lc := ensureAgent(tc.ConfigPath, tc.AgentSocket, tc.Scope, insecure)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := lc.Open(ctx, port, label, tc.Scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open failed:", err)
		os.Exit(1)
	}
	if jsonOut {
		printOpenResult(openResult{URL: resp.URL, Name: resp.Name, Port: resp.Port, Slug: resp.Slug, BaseDomain: resp.BaseDomain})
	} else {
		fmt.Println(resp.URL)
	}
}

// printOpenResult writes exactly one JSON object (one line, trailing
// newline) to stdout — nothing else, so callers can pipe into `jq`.
func printOpenResult(r openResult) {
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(r)
}

// quietClientLogs silences the client's INFO chatter (connect/replay)
// so foreground output is just the URL + status; WARN and above (e.g.
// reconnect failures) still surface on stderr.
func quietClientLogs() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

// mustYamuxWindow resolves BEAMD_YAMUX_STREAM_WINDOW_BYTES once for this CLI
// process, exiting with a clear message on a present-invalid value (§8.1). Every
// foreground client entry point (open, run, check) funnels through it so they use
// the same resolver as the edge and the detached agent.
func mustYamuxWindow() int64 {
	w, err := config.ResolveYamuxWindow()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if err := config.ValidateAgentYamuxWindow(w); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	return w
}

// mustTransport resolves the process-level BEAMD_TRANSPORT override over the
// selected account/explicit config and exits on an invalid value. The same
// resolver supplies the hosted session versus self-hosted token default for
// foreground, detached-agent, and check paths.
func mustTransport(cfg *config.Client) string {
	mode, err := config.ResolveClientTransport(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	return mode
}

// dialAndRegister opens a foreground connection to the edge and registers
// name→port, returning the live client and its public URL. The caller
// owns closing the client.
func dialAndRegister(cfg *config.Client, port int, name, scope string, insecure bool) (*client.Client, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	c, err := client.Connect(ctx, cfg.Server, cfg.Token, client.Options{
		InsecureSkipVerify:     insecure,
		Scope:                  scope,
		YamuxStreamWindowBytes: mustYamuxWindow(),
		Transport:              mustTransport(cfg),
	})
	cancel()
	if err != nil {
		return nil, "", fmt.Errorf("connect to edge: %w", err)
	}
	url, err := c.Register(name, port)
	if err != nil {
		_ = c.Close()
		return nil, "", err
	}
	return c, url, nil
}

// splitRunArgs splits `run` args on the first "--": the part before is
// run's own flags + optional name, the part after is the command to execute.
// ok is false when there's no "--" or nothing follows it.
func splitRunArgs(args []string) (runArgs, cmdArgs []string, ok bool) {
	for i, a := range args {
		if a == "--" {
			if i == len(args)-1 {
				return nil, nil, false
			}
			return args[:i], args[i+1:], true
		}
	}
	return nil, nil, false
}

// runCmd wraps a command as a tunnel: it resolves the edge + name like
// `open` (account, beamd.yaml, --as/--from), connects to the edge *first*
// (fail fast on a bad token before booting the dev server), then runs the
// command with $PORT/$HOST/$BEAMD_URL set and any framework flags injected,
// waits for it to listen, registers, and cleans up (tunnel + the command's
// whole process group) on Ctrl-C or when the command exits. The name is
// optional — it falls through the naming ladder when omitted.
//
// Usage: beamd run [name] [--as L|--from S] [-p prof] [--port N] [--json] -- <cmd...>
func runCmd(args []string) {
	runArgs, cmdArgs, ok := splitRunArgs(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: beamd run [name] [--as label | --from src] [-p profile] [--port N] [--json] -- <command> [args...]")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	asFlag := fs.String("as", "", "literal subdomain label")
	fromFlag := fs.String("from", "", "derive the label from: port | dir | repo | branch | worktree")
	portFlag := fs.Int("port", 0, "local port to expose (0 = pick a free one and set $PORT)")
	jsonOut := fs.Bool("json", false, "print one JSON object describing the tunnel, and nothing else")
	insecure := fs.Bool("insecure", false, "skip edge TLS verification (self-signed dev edges only)")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(runArgs, clientFlagValueNames("as", "from", "port")))

	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd run [name] ... -- <command> [args...]")
		os.Exit(2)
	}

	ctx := resolveContext(cf)
	cfg := ctx.mustAuth() // fail fast on a bad/absent profile

	port := *portFlag
	if port == 0 {
		port = freePort()
	}

	// A positional name is an explicit literal (like --as); otherwise walk
	// the naming ladder (--as/--from → beamd.yaml → global → port).
	var label string
	if fs.NArg() == 1 {
		label = fs.Arg(0)
		if err := naming.ValidateLabel(label); err != nil {
			fmt.Fprintln(os.Stderr, "invalid name:", err)
			os.Exit(2)
		}
	} else {
		l, err := resolveLabel(*asFlag, *fromFlag, ctx, port)
		if err != nil {
			fmt.Fprintln(os.Stderr, "name:", err)
			os.Exit(2)
		}
		label = l
	}

	quietClientLogs()

	// Connect BEFORE spawning the child, so a bad token / unreachable edge
	// fails immediately rather than after the dev server boots. Slug + base
	// are known right after Connect, so the child can be handed its public
	// URL up front (BEAMD_URL, allowed-hosts) even though we register later.
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 30*time.Second)
	c, err := client.Connect(dialCtx, cfg.Server, cfg.Token, client.Options{
		InsecureSkipVerify:     *insecure || cfg.InsecureSkipVerify,
		Scope:                  ctx.Scope,
		YamuxStreamWindowBytes: mustYamuxWindow(),
		Transport:              mustTransport(cfg),
	})
	cancelDial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "run: connect to edge:", err)
		os.Exit(1)
	}
	defer c.Close()

	// Preview URL handed to the child up front (BEAMD_URL, allowed-hosts) since env
	// is immutable once it starts. Built from the edge's real URL shape (sent in
	// hello_ok; empty → hyphen for older edges), so self-host subdomain/flat
	// deployments get the right origin instead of a guessed hyphen host. The edge
	// still returns the authoritative host on register — which can additionally
	// carry a custom-domain primary that isn't knowable before the child starts.
	host := naming.Hostname(label, c.Slug(), c.BaseDomain(), naming.ParseShape(c.Shape()))
	publicURL := "https://" + host
	// Vite/Next allowed-hosts: the wildcard parent of the tunnel host —
	// `.<slug>.<base>` when namespaced, `.<base>` when flat. Derive it from
	// the host (drop the first label) so it's correct either way.
	baseSuffix := host
	if i := strings.IndexByte(host, '.'); i >= 0 {
		baseSuffix = host[i:]
	}

	// Make any framework reachable: inject --port/--host for the $PORT-ignorers.
	cmdArgs, _ = injectFrameworkFlags(cmdArgs, port)
	if hint := nextAllowedOriginsHint(cmdArgs, host); hint != "" && !*jsonOut {
		fmt.Fprintln(os.Stderr, hint)
	}

	// Run via sh so node_modules/.bin (on the augmented PATH) resolves the
	// command; its own process group lets us tear down the whole tree.
	child := exec.Command("sh", append([]string{"-c", `exec "$0" "$@"`}, cmdArgs...)...)
	child.Env = childEnv(port, publicURL, baseSuffix, ctx.Cwd)
	child.Stdin = os.Stdin
	child.Stderr = os.Stderr
	if *jsonOut {
		child.Stdout = os.Stderr // keep our stdout pure for the JSON object
	} else {
		child.Stdout = os.Stdout
	}
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "run: start command:", err)
		os.Exit(1)
	}

	childExited := make(chan struct{})
	var childErr error
	go func() { childErr = child.Wait(); close(childExited) }()

	// Install the teardown handler *now*, before the (up-to-30s) wait — so a
	// Ctrl-C during startup still tears down the child instead of orphaning
	// the dev server. The child is in its own process group, so the terminal's
	// SIGINT reaches beamd, not the child; we forward it explicitly.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		if !*jsonOut {
			fmt.Fprintln(os.Stderr, "\nshutting down…")
		}
		_ = c.Close()
		killChild(child, childExited)
		os.Exit(0)
	}()

	// Wait for the child to bind $PORT, then register — so the URL only goes
	// live once the backend is actually up.
	if !waitListening(port, 30*time.Second, childExited) {
		select {
		case <-childExited:
			fmt.Fprintf(os.Stderr, "run: %q exited before listening on port %d: %v\n", cmdArgs[0], port, childErr)
		default:
			fmt.Fprintf(os.Stderr, "run: %q didn't listen on port %d within 30s\n", cmdArgs[0], port)
			killChild(child, childExited)
		}
		os.Exit(1)
	}

	url, err := c.Register(label, port)
	if err != nil {
		killChild(child, childExited)
		fmt.Fprintln(os.Stderr, "run: register tunnel:", err)
		os.Exit(1)
	}

	if *jsonOut {
		printOpenResult(openResult{URL: url, Name: label, Port: port, Slug: c.Slug(), BaseDomain: c.BaseDomain()})
	} else {
		fmt.Printf("%s  →  http://127.0.0.1:%d  (running: %s)\n\ntunnel live — Ctrl-C to stop\n", url, port, strings.Join(cmdArgs, " "))
	}

	// Block until the command exits on its own (Ctrl-C is handled by the
	// goroutine above, which exits the process).
	<-childExited
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "\ncommand exited: %v\n", childErr)
	}
	_ = c.Close()
	if ee, ok := childErr.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
}

// freePort grabs an ephemeral TCP port and releases it, returning the
// number so the wrapped command can bind it via $PORT.
func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "run: pick free port:", err)
		os.Exit(1)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitListening polls until something accepts TCP on port, returning true
// once it does, or false on timeout or the child exiting first.
func waitListening(port int, timeout time.Duration, childExited <-chan struct{}) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-childExited:
			return false
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// killChild signals the command's process group, escalating to SIGKILL if
// it doesn't exit promptly.
func killChild(cmd *exec.Cmd, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid // negative = the whole process group
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}
}

// listCmd shows detached tunnels held by the background agent. It never
// spawns an agent: if none is running there are no detached tunnels, so
// the answer is an empty list. (Foreground tunnels live in their own
// process and are not listed here.)
func listCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON array of tunnels and nothing else")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)

	items := []daemon.ListItem{}
	if rc.AgentSocket != "" && daemon.IsRunning(rc.AgentSocket) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got, err := daemon.NewLocalClient(rc.AgentSocket).List(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "list failed:", err)
			os.Exit(1)
		}
		if got != nil {
			items = got
		}
	}

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(items)
		return
	}
	if len(items) == 0 {
		fmt.Println("(no tunnels)")
		return
	}
	for _, it := range items {
		health := "healthy"
		if !it.Healthy {
			health = "unhealthy"
		}
		fmt.Printf("%-20s :%-5d  %s  %s\n", it.Name, it.Port, health, it.URL)
	}
}

// closeCmd tears down a detached tunnel. Idempotent: if the agent isn't
// running or no tunnel by that name exists, it's a no-op that still
// exits 0. Never spawns an agent.
func closeCmd(args []string) {
	fs := flag.NewFlagSet("close", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON object {name,removed} and nothing else")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd close <name> [-p profile] [--json]")
		os.Exit(2)
	}
	name := fs.Arg(0)
	rc := resolveContext(cf)

	removed := false
	if rc.AgentSocket != "" && daemon.IsRunning(rc.AgentSocket) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := daemon.NewLocalClient(rc.AgentSocket).Close(ctx, name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "close failed:", err)
			os.Exit(1)
		}
		removed = r
	}

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Name    string `json:"name"`
			Removed bool   `json:"removed"`
		}{name, removed})
		return
	}
	if removed {
		fmt.Printf("removed '%s'\n", name)
	} else {
		fmt.Printf("no detached tunnel named '%s'\n", name)
	}
}

// statusCmd reports the background agent's state and connection health,
// for humans and (with --json) for callers reconciling state. Never
// spawns an agent.
func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON status object and nothing else")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)
	server := rc.Server

	running := rc.AgentSocket != "" && daemon.IsRunning(rc.AgentSocket)
	slug := ""
	healthy := false
	var health *daemon.HealthzResponse
	if running {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if h, err := daemon.NewLocalClient(rc.AgentSocket).Ping(ctx); err == nil {
			health = h
			slug = h.Slug
			healthy = h.Healthy
		}
		cancel()
	}

	var services map[string]int
	if rc.Project != nil {
		services = rc.Project.Services
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			AgentRunning        bool           `json:"agentRunning"`
			Server              string         `json:"server"`
			Slug                string         `json:"slug"`
			Scope               string         `json:"scope"`
			Healthy             bool           `json:"healthy"`
			Transport           string         `json:"transport,omitempty"`
			ConfiguredTransport string         `json:"configuredTransport,omitempty"`
			FallbackCount       uint64         `json:"fallbackCount,omitempty"`
			LastFallbackReason  string         `json:"lastFallbackReason,omitempty"`
			ReconnectCount      uint64         `json:"reconnectCount,omitempty"`
			LastCloseReason     string         `json:"lastCloseReason,omitempty"`
			ProjectFile         string         `json:"projectFile,omitempty"`
			Services            map[string]int `json:"services,omitempty"`
		}{
			AgentRunning:        running,
			Server:              server,
			Slug:                slug,
			Scope:               rc.Scope,
			Healthy:             healthy,
			Transport:           healthString(health, func(h *daemon.HealthzResponse) string { return h.Transport }),
			ConfiguredTransport: healthString(health, func(h *daemon.HealthzResponse) string { return h.ConfiguredTransport }),
			FallbackCount:       healthUint(health, func(h *daemon.HealthzResponse) uint64 { return h.FallbackCount }),
			LastFallbackReason:  healthString(health, func(h *daemon.HealthzResponse) string { return h.LastFallbackReason }),
			ReconnectCount:      healthUint(health, func(h *daemon.HealthzResponse) uint64 { return h.ReconnectCount }),
			LastCloseReason:     healthString(health, func(h *daemon.HealthzResponse) string { return h.LastCloseReason }),
			ProjectFile:         projectFileLocation(rc),
			Services:            services,
		})
		return
	}

	if server != "" {
		fmt.Printf("account: %s\n", server)
	}
	if rc.Scope != "" {
		fmt.Printf("scope:   %s\n", rc.Scope)
	}
	if line := projectStatusLine(rc); line != "" {
		fmt.Printf("project: %s\n", line)
	}
	if names := serviceNames(rc.Project); len(names) > 0 {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%s→%d", n, rc.Project.Services[n])
		}
		fmt.Printf("services: %s\n", strings.Join(parts, ", "))
	}
	fmt.Printf("agent:   %s\n", boolWord(running, "running", "not running"))
	if slug != "" {
		fmt.Printf("slug:    %s\n", slug)
	}
	if running {
		fmt.Printf("tunnel:  %s\n", boolWord(healthy, "connected", "disconnected"))
		if health != nil && health.Transport != "" {
			fmt.Printf("transport: %s\n", health.Transport)
		}
	}
}

func healthString(h *daemon.HealthzResponse, get func(*daemon.HealthzResponse) string) string {
	if h == nil {
		return ""
	}
	return get(h)
}

func healthUint(h *daemon.HealthzResponse, get func(*daemon.HealthzResponse) uint64) uint64 {
	if h == nil {
		return 0
	}
	return get(h)
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// projectFileLocation is the path to the discovered project file — just
// "beamd.yaml" when it's in the cwd, otherwise the full path to the dir it was
// found in. "" when no project file is in effect.
func projectFileLocation(rc *tunnelContext) string {
	if rc.Project == nil {
		return ""
	}
	if rc.ProjectDir == "" || rc.ProjectDir == rc.Cwd {
		return config.ProjectFile
	}
	return filepath.Join(rc.ProjectDir, config.ProjectFile)
}

// projectStatusLine renders the active project file plus the fields it pins,
// e.g. `beamd.yaml (scope=trey, server=staging.beamd.run)`. Makes the otherwise
// invisible file visible during normal use.
func projectStatusLine(rc *tunnelContext) string {
	loc := projectFileLocation(rc)
	if loc == "" {
		return ""
	}
	var pins []string
	if rc.Project.Server != "" {
		pins = append(pins, "server="+rc.Project.Server)
	}
	if rc.Project.Scope != "" {
		pins = append(pins, "scope="+rc.Project.Scope)
	}
	if rc.Project.Name != "" {
		pins = append(pins, "name="+rc.Project.Name)
	}
	if rc.Project.From != "" {
		pins = append(pins, "from="+rc.Project.From)
	}
	if len(pins) == 0 {
		return loc
	}
	return loc + " (" + strings.Join(pins, ", ") + ")"
}

// ensureAgent makes sure the selected profile's background agent (the
// long-lived worker that holds the tunnel session) is running, spawning it
// on demand against its own socket, and returns a client to that socket.
// configPath is the profile file (or explicit --config) the agent loads its
// server/token from.
func ensureAgent(configPath, socket, scope string, insecure bool) *daemon.LocalClient {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate executable:", err)
		os.Exit(1)
	}
	env := []string{"BEAMD_CONFIG=" + configPath}
	if scope != "" {
		// Propagate the resolved scope so the detached agent requests the same
		// org the foreground command would have (the account file holds a
		// default, but a one-off --scope / beamd.yaml scope: must reach the agent).
		env = append(env, "BEAMD_SCOPE="+scope)
	}
	if insecure {
		// Propagate a one-off --insecure to the spawned agent (the config
		// field would otherwise be the only way to set it for detached use).
		env = append(env, "BEAMD_INSECURE=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := daemon.EnsureRunning(ctx, exe, socket, env); err != nil {
		fmt.Fprintln(os.Stderr, "agent not available:", err)
		// The spawned agent logs its actual failure (bad token, unreachable
		// edge, …) to agent.log and exits; without this the user only ever
		// sees "did not start within 5s" and debugs the wrong layer.
		if tail := agentLogTail(3); tail != "" {
			fmt.Fprintln(os.Stderr, "recent agent log:")
			fmt.Fprintln(os.Stderr, tail)
		}
		os.Exit(1)
	}
	return daemon.NewLocalClient(socket)
}

// agentLogTail returns the last n lines of the agent log, indented for
// display, or "" if the log can't be read.
func agentLogTail(n int) string {
	path, err := defaultAgentLogPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// agentCmd runs the background worker. It is spawned internally by the
// client subcommands (via ensureAgent); it is not meant to be run by
// hand.
func agentCmd(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	socket := fs.String("socket", "", "unix socket path")
	configPath := fs.String("config", os.Getenv("BEAMD_CONFIG"), "client config path")
	_ = fs.Parse(args)

	if *configPath == "" {
		*configPath = defaultConfigPath()
	}

	logPath, _ := defaultAgentLogPath()
	logOut := os.Stderr
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		logOut = f
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logOut, nil)))

	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		slog.Error("load config", "err", err.Error())
		os.Exit(1)
	}
	cfg.Server = normalizeServerAddr(cfg.Server) // tolerate a port-less server:
	if cfg.Server == "" || cfg.Token == "" {
		slog.Error("missing server or token; run `beamd login` first")
		os.Exit(1)
	}
	if *socket == "" {
		*socket = cfg.AgentSocket
	}

	insecure := cfg.InsecureSkipVerify || os.Getenv("BEAMD_INSECURE") == "1"
	scope := os.Getenv("BEAMD_SCOPE")

	// Resolve the yamux stream window once at agent startup and reuse it across
	// reconnects (§8.1). A present-invalid value is fatal; log the effective one.
	yamuxWindow, err := config.ResolveYamuxWindow()
	if err != nil {
		slog.Error("yamux stream window", "err", err.Error())
		os.Exit(1)
	}
	if err := config.ValidateAgentYamuxWindow(yamuxWindow); err != nil {
		slog.Error("yamux stream window exposure", "err", err.Error())
		os.Exit(1)
	}
	transportMode := mustTransport(cfg)
	slog.Info("agent starting",
		"socket", *socket,
		"transport", transportMode,
		"yamux_stream_window_bytes", yamuxWindow,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := client.Connect(ctx, cfg.Server, cfg.Token, client.Options{
		InsecureSkipVerify:     insecure,
		Scope:                  scope,
		YamuxStreamWindowBytes: yamuxWindow,
		Transport:              transportMode,
	})
	cancel()
	if err != nil {
		slog.Error("connect to edge failed", "err", err.Error())
		os.Exit(1)
	}
	defer c.Close()

	d := daemon.New(c, *socket)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		slog.Info("agent: shutdown signal received")
		_ = d.Shutdown(context.Background())
	}()

	if err := d.Serve(); err != nil {
		slog.Error("agent serve", "err", err.Error())
		os.Exit(1)
	}
}

func defaultAgentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".beamd", "agent.log"), nil
}
