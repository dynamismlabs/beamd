package main

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
	"syscall"
	"time"

	"github.com/treyhuffine/beamd/internal/client"
	"github.com/treyhuffine/beamd/internal/config"
	"github.com/treyhuffine/beamd/internal/daemon"
	"github.com/treyhuffine/beamd/internal/devicecode"
)

// Version is set at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "login":
		loginCmd(os.Args[2:])
	case "expose":
		exposeCmd(os.Args[2:])
	case "list":
		listCmd(os.Args[2:])
	case "unexpose":
		unexposeCmd(os.Args[2:])
	case "daemon":
		daemonCmd(os.Args[2:])
	case "mcp":
		mcpCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(Version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: beam <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  login           authenticate against a beamd server")
	fmt.Fprintln(os.Stderr, "  expose          expose a local port as a public URL")
	fmt.Fprintln(os.Stderr, "  list            list active tunnels")
	fmt.Fprintln(os.Stderr, "  unexpose        remove a tunnel")
	fmt.Fprintln(os.Stderr, "  daemon          run the daemon (used internally by other subcommands)")
	fmt.Fprintln(os.Stderr, "  mcp             run the MCP stdio server")
	fmt.Fprintln(os.Stderr, "  version         print version and exit")
}

func notImpl(name, milestone string) {
	fmt.Fprintf(os.Stderr, "%s: not implemented (%s)\n", name, milestone)
	os.Exit(2)
}

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

	sock, err := config.DefaultDaemonSocket()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve daemon socket:", err)
		os.Exit(1)
	}

	cfg := &config.Client{Server: *server, Token: *token, DaemonSocket: sock}
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

func exposeCmd(args []string) {
	fs := flag.NewFlagSet("expose", flag.ExitOnError)
	name := fs.String("as", "", "subdomain label (defaults to port number)")
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beam expose <port> [--as name]")
		os.Exit(2)
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "invalid port:", fs.Arg(0))
		os.Exit(2)
	}

	cfg := mustLoadConfig(*configPath)
	lc := ensureDaemon(cfg, *configPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url, err := lc.Expose(ctx, port, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "expose failed:", err)
		os.Exit(1)
	}
	fmt.Println(url)
}

func listCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	cfg := mustLoadConfig(*configPath)
	lc := ensureDaemon(cfg, *configPath)

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

func unexposeCmd(args []string) {
	fs := flag.NewFlagSet("unexpose", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beam unexpose <name>")
		os.Exit(2)
	}
	name := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)
	lc := ensureDaemon(cfg, *configPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := lc.Unexpose(ctx, name); err != nil {
		fmt.Fprintln(os.Stderr, "unexpose failed:", err)
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
		fmt.Fprintln(os.Stderr, "missing server or token — run `beam login` first")
		os.Exit(2)
	}
	return cfg
}

func ensureDaemon(cfg *config.Client, configPath string) *daemon.LocalClient {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate executable:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := daemon.EnsureRunning(ctx, exe, cfg.DaemonSocket, []string{
		"BEAMD_CONFIG=" + configPath,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "daemon not available:", err)
		os.Exit(1)
	}
	return daemon.NewLocalClient(cfg.DaemonSocket)
}

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", "", "unix socket path")
	configPath := fs.String("config", os.Getenv("BEAMD_CONFIG"), "client config path")
	_ = fs.Parse(args)

	if *configPath == "" {
		*configPath = defaultConfigPath()
	}

	logPath, _ := defaultDaemonLogPath()
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
		slog.Error("missing server or token; run `beam login` first")
		os.Exit(1)
	}
	if *socket == "" {
		*socket = cfg.DaemonSocket
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
		slog.Info("daemon: shutdown signal received")
		_ = d.Shutdown(context.Background())
	}()

	if err := d.Serve(); err != nil {
		slog.Error("daemon serve", "err", err.Error())
		os.Exit(1)
	}
}

func defaultDaemonLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".beam", "daemon.log"), nil
}
