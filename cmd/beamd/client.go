package main

// Client-side subcommands of the single `beamd` binary: login, up, down,
// list, and the internal `agent` background worker. The edge/server
// subcommands (serve, init, add-developer, provision-dev) live in
// main.go; both roles ship in one binary.

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
)

func defaultConfigPath() string {
	p, err := config.DefaultClientPath()
	if err != nil {
		return "" // caller will get a clearer error from LoadClient
	}
	return p
}

func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "beamd edge address, e.g. beam.example.com:443")
	token := fs.String("token", "", "bearer token (copy-paste flow); omit for device-code login")
	insecure := fs.Bool("insecure", false, "skip TLS verification for the discovery + device-code calls (dev/self-signed setups)")
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

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

func upCmd(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	name := fs.String("as", "", "subdomain label (defaults to port number)")
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"as": true, "config": true}))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd up <port> [--as name]")
		os.Exit(2)
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "invalid port:", fs.Arg(0))
		os.Exit(2)
	}

	cfg := mustLoadConfig(*configPath)
	lc := ensureAgent(cfg, *configPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url, err := lc.Expose(ctx, port, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "up failed:", err)
		os.Exit(1)
	}
	fmt.Println(url)
}

func listCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	cfg := mustLoadConfig(*configPath)
	lc := ensureAgent(cfg, *configPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := lc.List(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list failed:", err)
		os.Exit(1)
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

func downCmd(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd down <name>")
		os.Exit(2)
	}
	name := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)
	lc := ensureAgent(cfg, *configPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := lc.Unexpose(ctx, name); err != nil {
		fmt.Fprintln(os.Stderr, "down failed:", err)
		os.Exit(1)
	}
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
