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
	"strconv"
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

// isInteractive reports whether stdin is a terminal, so we can prompt the
// user — vs a pipe/redirect/CI, where we must not block on a prompt.
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "beamd edge address, e.g. beam.example.com:443")
	token := fs.String("token", "", "bearer token (copy-paste flow); omit for device-code login")
	insecure := fs.Bool("insecure", false, "skip TLS verification for the discovery + device-code calls (dev/self-signed setups)")
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	// In an interactive terminal, prompt for anything missing (with hints)
	// instead of erroring — friendlier than re-typing the whole command.
	// Piped/scripted use keeps the strict behavior so it fails fast rather
	// than blocking on a prompt no one will answer.
	if (*server == "" || *token == "") && isInteractive() {
		r := bufio.NewReader(os.Stdin)
		if *server == "" {
			fmt.Println("Connect this machine to a beamd edge.")
			*server = prompt(r, "edge address (host:port, e.g. tunnel.example.com:443)", "")
		}
		if *token == "" {
			fmt.Println("Your developer token — ask whoever runs the edge, or find it in the")
			fmt.Println("edge's tokens.json (the key that maps to your slug).")
			*token = prompt(r, "token (or Enter to try browser login, if the edge supports it)", "")
		}
	}

	if *server == "" {
		fmt.Fprintln(os.Stderr, "login: --server is required")
		os.Exit(2)
	}

	if *token == "" {
		got, err := deviceCodeLogin(*server, *insecure)
		if err != nil {
			fmt.Fprintln(os.Stderr, "login:", err)
			os.Exit(1)
		}
		*token = got
	}

	sock, err := config.DefaultAgentSocket()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve agent socket:", err)
		os.Exit(1)
	}

	cfg := &config.Client{Server: *server, Token: *token, AgentSocket: sock}
	if err := config.SaveClient(cfg, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "save config:", err)
		os.Exit(1)
	}
	fmt.Println("logged in")
}

// deviceCodeLogin runs the no-token login flow: ask beamd for its
// discovery payload, then do the device-code dance against whatever
// web app the operator pointed it at. Returns the issued token.
func deviceCodeLogin(server string, insecure bool) (string, error) {
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
		return "", fmt.Errorf(
			"discovery failed: %w\n  → pass --token <T> instead, or --insecure if the server's apex cert is self-signed",
			err,
		)
	}
	if disc == nil {
		return "", fmt.Errorf(
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
	name := fs.String("as", "", "subdomain label (defaults to port number)")
	detach := fs.Bool("detach", false, "run in the background agent and return immediately (the path automation uses)")
	fs.BoolVar(detach, "d", false, "shorthand for --detach")
	jsonOut := fs.Bool("json", false, "print exactly one JSON object describing the tunnel, and nothing else")
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"as": true, "config": true}))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd open <port> [--as name] [-d] [--json]")
		os.Exit(2)
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "invalid port:", fs.Arg(0))
		os.Exit(2)
	}

	cfg := mustLoadConfig(*configPath)
	if *detach {
		openDetached(cfg, *configPath, port, *name, *jsonOut)
		return
	}
	openForeground(cfg, port, *name, *jsonOut)
}

// openForeground holds the tunnel in *this* process (the default, like
// ngrok): it opens its own connection to the edge, registers, prints the
// URL, and blocks until interrupted — then tears the tunnel down. No
// agent is involved.
func openForeground(cfg *config.Client, port int, name string, jsonOut bool) {
	quietClientLogs()

	c, url, err := dialAndRegister(cfg, port, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open failed:", err)
		os.Exit(1)
	}
	defer c.Close()

	resolved := name
	if resolved == "" {
		resolved = naming.LabelFromPort(port)
	}

	if jsonOut {
		printOpenResult(openResult{URL: url, Name: resolved, Port: port, Slug: c.Slug(), BaseDomain: c.BaseDomain()})
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

// openDetached hands the tunnel to the background agent and returns
// immediately. This is the path automation (e.g. Flow) uses.
func openDetached(cfg *config.Client, configPath string, port int, name string, jsonOut bool) {
	lc := ensureAgent(cfg, configPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := lc.Open(ctx, port, name)
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

// dialAndRegister opens a foreground connection to the edge and registers
// name→port, returning the live client and its public URL. The caller
// owns closing the client.
func dialAndRegister(cfg *config.Client, port int, name string) (*client.Client, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	c, err := client.Connect(ctx, cfg.Server, cfg.Token)
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

// runCmd wraps a command as a tunnel: it picks a free local port, runs the
// command with $PORT set, waits for it to start listening, opens a
// foreground tunnel to it, then cleans up (tunnel + the command's whole
// process group) on Ctrl-C or when the command exits. Mirrors the loved
// `portless <name> <cmd>` ergonomic.
//
// Usage: beamd run <name> [--port N] [--json] -- <command> [args...]
// splitRunArgs splits `run` args on the first "--": the part before is
// run's own flags + name, the part after is the command to execute. ok is
// false when there's no "--" or nothing follows it.
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

func runCmd(args []string) {
	runArgs, cmdArgs, ok := splitRunArgs(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: beamd run <name> [--port N] [--json] -- <command> [args...]")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	portFlag := fs.Int("port", 0, "local port to expose (0 = pick a free one and set $PORT)")
	jsonOut := fs.Bool("json", false, "print one JSON object describing the tunnel, and nothing else")
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(hoistFlags(runArgs, map[string]bool{"port": true, "config": true}))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd run <name> [--port N] [--json] -- <command> [args...]")
		os.Exit(2)
	}
	name := fs.Arg(0)
	if err := naming.ValidateLabel(name); err != nil {
		fmt.Fprintln(os.Stderr, "invalid name:", err)
		os.Exit(2)
	}

	cfg := mustLoadConfig(*configPath)
	quietClientLogs()

	port := *portFlag
	if port == 0 {
		port = freePort()
	}

	// Spawn the command with $PORT set, in its own process group so we can
	// signal the whole tree (the command may fork workers) on teardown.
	child := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	child.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
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

	// Wait for the command to bind $PORT (or bail if it exits first).
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

	c, url, err := dialAndRegister(cfg, port, name)
	if err != nil {
		killChild(child, childExited)
		fmt.Fprintln(os.Stderr, "run: open tunnel:", err)
		os.Exit(1)
	}
	defer c.Close()

	if *jsonOut {
		printOpenResult(openResult{URL: url, Name: name, Port: port, Slug: c.Slug(), BaseDomain: c.BaseDomain()})
	} else {
		fmt.Printf("%s  →  http://127.0.0.1:%d  (running: %s)\n\ntunnel live — Ctrl-C to stop\n", url, port, strings.Join(cmdArgs, " "))
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigs:
		if !*jsonOut {
			fmt.Fprintln(os.Stderr, "\nshutting down…")
		}
		_ = c.Close()
		killChild(child, childExited)
	case <-childExited:
		if !*jsonOut {
			fmt.Fprintf(os.Stderr, "\ncommand exited: %v\n", childErr)
		}
		_ = c.Close()
		if ee, ok := childErr.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
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
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"config": true}))

	cfg := loadClientConfig(*configPath)

	items := []daemon.ListItem{}
	if daemon.IsRunning(cfg.AgentSocket) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got, err := daemon.NewLocalClient(cfg.AgentSocket).List(ctx)
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
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"config": true}))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd close <name> [--json]")
		os.Exit(2)
	}
	name := fs.Arg(0)
	cfg := loadClientConfig(*configPath)

	removed := false
	if daemon.IsRunning(cfg.AgentSocket) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := daemon.NewLocalClient(cfg.AgentSocket).Close(ctx, name)
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
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"config": true}))

	cfg := loadClientConfig(*configPath)

	running := daemon.IsRunning(cfg.AgentSocket)
	slug := ""
	healthy := false
	if running {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if h, err := daemon.NewLocalClient(cfg.AgentSocket).Ping(ctx); err == nil {
			slug = h.Slug
			healthy = h.Healthy
		}
		cancel()
	}

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			AgentRunning bool   `json:"agentRunning"`
			Server       string `json:"server"`
			Slug         string `json:"slug"`
			Healthy      bool   `json:"healthy"`
		}{running, cfg.Server, slug, healthy})
		return
	}

	fmt.Printf("agent:   %s\n", boolWord(running, "running", "not running"))
	if cfg.Server != "" {
		fmt.Printf("server:  %s\n", cfg.Server)
	}
	if slug != "" {
		fmt.Printf("slug:    %s\n", slug)
	}
	if running {
		fmt.Printf("tunnel:  %s\n", boolWord(healthy, "connected", "disconnected"))
	}
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func mustLoadConfig(path string) *config.Client {
	cfg, err := config.LoadClient(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	if cfg.Server == "" || cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "missing server or token — run `beamd login` first")
		os.Exit(2)
	}
	return cfg
}

// loadClientConfig loads config without requiring login — it only needs
// the agent socket path. Used by list/down/status, which operate on a
// possibly-absent agent and must not hard-fail when the user hasn't
// logged in yet.
func loadClientConfig(path string) *config.Client {
	cfg, err := config.LoadClient(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	return cfg
}

// ensureAgent makes sure the background agent (the long-lived worker that
// holds the tunnel session) is running, spawning it on demand, and
// returns a client to its local socket API.
func ensureAgent(cfg *config.Client, configPath string) *daemon.LocalClient {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate executable:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := daemon.EnsureRunning(ctx, exe, cfg.AgentSocket, []string{
		"BEAMD_CONFIG=" + configPath,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "agent not available:", err)
		os.Exit(1)
	}
	return daemon.NewLocalClient(cfg.AgentSocket)
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
	if cfg.Server == "" || cfg.Token == "" {
		slog.Error("missing server or token; run `beamd login` first")
		os.Exit(1)
	}
	if *socket == "" {
		*socket = cfg.AgentSocket
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := client.Connect(ctx, cfg.Server, cfg.Token)
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
