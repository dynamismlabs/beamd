package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/dns"
	"github.com/dynamismlabs/beamd/internal/edge"
	"github.com/dynamismlabs/beamd/internal/naming"
	"github.com/dynamismlabs/beamd/internal/reqlog"
)

// Version is set at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	// edge / server role
	case "serve":
		serveCmd(os.Args[2:])
	case "init":
		initCmd(os.Args[2:])
	case "add-developer":
		addDeveloperCmd(os.Args[2:])
	case "provision-dev":
		provisionDevCmd(os.Args[2:])
	case "issue-token":
		notImpl("issue-token", "device-code milestone (post-v1)")
	// client role
	case "login":
		loginCmd(os.Args[2:])
	case "logout":
		logoutCmd(os.Args[2:])
	case "link":
		linkCmd(os.Args[2:])
	case "default":
		defaultCmd(os.Args[2:])
	case "accounts":
		accountsCmd(os.Args[2:])
	case "orgs", "scopes":
		orgsCmd(os.Args[2:])
	case "whoami":
		whoamiCmd(os.Args[2:])
	case "check":
		checkCmd(os.Args[2:])
	case "reload":
		reloadCmd(os.Args[2:])
	case "open":
		openCmd(os.Args[2:])
	case "close":
		closeCmd(os.Args[2:])
	case "run":
		runCmd(os.Args[2:])
	case "list":
		listCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	case "mcp":
		mcpCmd(os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(Version)
	case "help", "--help", "-h":
		usage()
	default:
		// Bare `beamd <port>` is shorthand for `beamd open <port>`.
		if p, err := strconv.Atoi(os.Args[1]); err == nil {
			if p < 1 || p > 65535 {
				// Numeric but not a port: say so, instead of the misleading
				// "unknown command: 70000" + a usage dump.
				fmt.Fprintf(os.Stderr, "invalid port: %s (must be 1-65535)\n", os.Args[1])
				os.Exit(2)
			}
			openCmd(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: beamd <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "edge (server):")
	fmt.Fprintln(os.Stderr, "  serve           run the edge server")
	fmt.Fprintln(os.Stderr, "  init            interactively write beamd.yaml + an empty tokens.json")
	fmt.Fprintln(os.Stderr, "  add-developer   issue a token for a slug, provision DNS + cert, print the token")
	fmt.Fprintln(os.Stderr, "  provision-dev   write DNS + pre-warm cert for a slug (low-level — add-developer wraps this)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "client:")
	fmt.Fprintln(os.Stderr, "  login           authenticate against a beamd edge (login --server <edge> [--token <t>])")
	fmt.Fprintln(os.Stderr, "  logout          remove an account (logout [--server <edge>])")
	fmt.Fprintln(os.Stderr, "  accounts        list the edges you're logged into, marking the current one")
	fmt.Fprintln(os.Stderr, "  orgs            list the orgs/scopes your current account can act in")
	fmt.Fprintln(os.Stderr, "  default         show or set your account's default scope (default [<scope>])")
	fmt.Fprintln(os.Stderr, "  link            pin this repo to an org/edge — writes beamd.yaml (link [--scope <org>])")
	fmt.Fprintln(os.Stderr, "  whoami          show the resolved account + scope")
	fmt.Fprintln(os.Stderr, "  check           verify credentials against the edge (no tunnel, no agent)")
	fmt.Fprintln(os.Stderr, "  open            expose a local port as a public URL (foreground; -d to detach)")
	fmt.Fprintln(os.Stderr, "  close           remove a detached tunnel")
	fmt.Fprintln(os.Stderr, "  run             run a command and expose its port: run [name] -- <cmd...>")
	fmt.Fprintln(os.Stderr, "  list            list detached tunnels")
	fmt.Fprintln(os.Stderr, "  status          show account + scope + agent + connection status")
	fmt.Fprintln(os.Stderr, "  reload          restart the background agent with fresh credentials")
	fmt.Fprintln(os.Stderr, "  mcp             run the MCP stdio server")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  most client commands accept --server <edge> and --scope <org> to pick where a tunnel lands")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  version         print version and exit")
}

func notImpl(name, milestone string) {
	fmt.Fprintf(os.Stderr, "%s: not implemented (%s)\n", name, milestone)
	os.Exit(2)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "/etc/beamd/beamd.yaml", "path to config file")
	_ = fs.Parse(args)

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err.Error())
		os.Exit(1)
	}

	// Resolve the yamux stream window once from the environment and inject it for
	// edge.New (transport-performance-spec §8.1). A present-invalid value is fatal.
	yamuxWindow, err := config.ResolveYamuxWindow()
	if err != nil {
		slog.Error("yamux stream window", "err", err.Error())
		os.Exit(1)
	}
	cfg.YamuxStreamWindowBytes = yamuxWindow

	tokens, err := auth.Open(cfg.TokenStore)
	if err != nil {
		slog.Error("token store load failed", "spec", cfg.TokenStore, "err", err.Error())
		os.Exit(1)
	}

	certMgr, err := buildCertManager(cfg)
	if err != nil {
		slog.Error("cert manager init failed", "err", err.Error())
		os.Exit(1)
	}

	slog.Info("ready",
		"version", Version,
		"base_domain", cfg.BaseDomain,
		"listen_https", cfg.ListenHTTPS,
		"dns_provider", cfg.DNSProvider,
		"token_store", cfg.TokenStore,
		"yamux_stream_window_bytes", yamuxWindow,
	)

	e := edge.New(cfg, Version, tokens, certMgr)

	// Per-request event pipeline: an always-on local file sink (OSS gets request
	// logs for free), plus a hosted shipper that tails it to the control plane.
	stopReqlog := startRequestPipeline(cfg, e)
	defer stopReqlog() // backstop; the paths below stop it in the right order.

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	drained := make(chan struct{})
	go func() {
		sig := <-sigs
		slog.Info("shutdown signal received", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Drain in-flight requests FIRST so their final events are recorded, THEN
		// flush + stop the request log. Closing the sink before the drain would
		// drop the events of requests that complete during the drain window.
		_ = e.Shutdown(ctx)
		stopReqlog()
		close(drained)
	}()

	if err := e.Serve(); err != nil {
		slog.Error("serve failed", "err", err.Error())
		stopReqlog() // non-signal exit (e.g. listener error): still flush the sink
		os.Exit(1)
	}
	// Graceful path: Serve returned because Shutdown closed the listener. Wait for
	// the drain + sink flush to finish before exiting (Serve returns as soon as the
	// listener closes, while Shutdown is still draining).
	<-drained
}

// startRequestPipeline wires the always-on per-request file sink into the edge
// and, when a request_reporter webhook is configured, starts the hosted shipper
// that tails the log and ships batches to the control plane. Returns an
// idempotent stop func: it flushes the file sink (durable) then stops the shipper
// — anything unsent ships on next start (the file + cursor persist), so it's
// lossless across restarts (request-events-spec §4.7).
func startRequestPipeline(cfg *config.Server, e *edge.Edge) func() {
	if !cfg.RequestLogEnabled() {
		return func() {}
	}
	logPath := cfg.RequestLog.Path
	if logPath == "" {
		logPath = filepath.Join(cfg.DataDir, "requests.log")
	}
	fs, err := reqlog.NewFileSink(reqlog.FileSinkConfig{
		Path:     logPath,
		MaxBytes: int64(cfg.RequestLog.MaxSizeMB) << 20,
		Fsync:    time.Duration(cfg.RequestLog.FsyncMs) * time.Millisecond,
		// A shipper tails the rotated .1 only when a reporter webhook is set; tell
		// the sink so rotation won't clobber an unshipped .1 (lossless buffer).
		Tailed: cfg.RequestReporter.WebhookURL != "",
	})
	if err != nil {
		slog.Error("reqlog: file sink init failed", "err", err.Error())
		os.Exit(1)
	}
	e.SetReqSink(fs)

	shipperCancel := func() {}
	if cfg.RequestReporter.WebhookURL != "" {
		cursorPath := cfg.RequestReporter.CursorFile
		if cursorPath == "" {
			cursorPath = filepath.Join(cfg.DataDir, "requests.cursor")
		}
		secret := ""
		if cfg.RequestReporter.SecretEnv != "" {
			secret = os.Getenv(cfg.RequestReporter.SecretEnv)
		}
		if secret == "" {
			slog.Error("reqlog: request_reporter set but secret env is empty")
			os.Exit(1)
		}
		sh := reqlog.NewShipper(reqlog.ShipperConfig{
			LogPath:    logPath,
			CursorPath: cursorPath,
			WebhookURL: cfg.RequestReporter.WebhookURL,
			Secret:     secret,
			BatchSize:  cfg.RequestReporter.BatchSize,
			Flush:      time.Duration(cfg.RequestReporter.FlushMs) * time.Millisecond,
		})
		ctx, cancel := context.WithCancel(context.Background())
		shipperCancel = cancel
		go sh.Run(ctx)
		slog.Info("reqlog: shipper started", "webhook", cfg.RequestReporter.WebhookURL)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			fs.Close()      // flush buffered events to the durable file
			shipperCancel() // best-effort final ship; unsent ships on next start
		})
	}
}

// resolveZone returns the registered DNS zone to manage records in.
// Honors an explicit dns_zone config override; otherwise auto-detects
// from base_domain via the provider (so subdomain base_domains work).
func resolveZone(ctx context.Context, cfg *config.Server) (string, error) {
	if cfg.DNSZone != "" {
		return cfg.DNSZone, nil
	}
	return dns.ResolveZone(ctx, cfg.DNSProvider, cfg.DNSProviderCreds, cfg.BaseDomain)
}

func provisionDevCmd(args []string) {
	fs := flag.NewFlagSet("provision-dev", flag.ExitOnError)
	configPath := fs.String("config", "/etc/beamd/beamd.yaml", "path to config file")
	slug := fs.String("slug", "", "namespace tunnels under <slug> (omit for flat routing: *.<base>)")
	_ = fs.Parse(args)

	if *slug != "" {
		if err := naming.ValidateLabel(*slug); err != nil {
			fmt.Fprintln(os.Stderr, "invalid slug:", err)
			os.Exit(2)
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err.Error())
		os.Exit(1)
	}

	p, err := dns.Open(cfg.DNSProvider, cfg.DNSProviderCreds)
	if err != nil {
		slog.Error("dns provider open failed", "provider", cfg.DNSProvider, "err", err.Error())
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	zone, err := resolveZone(ctx, cfg)
	if err != nil {
		slog.Error("resolve dns zone failed", "base_domain", cfg.BaseDomain, "err", err.Error())
		os.Exit(1)
	}

	if err := dns.ProvisionSlug(ctx, p, zone, cfg.BaseDomain, *slug, cfg.EdgeIPv4, cfg.EdgeIPv6); err != nil {
		slog.Error("dns provision failed", "slug", *slug, "err", err.Error())
		os.Exit(1)
	}
	slog.Info("dns provisioned",
		"slug", *slug,
		"base_domain", cfg.BaseDomain,
		"edge_ipv4", cfg.EdgeIPv4,
		"edge_ipv6", cfg.EdgeIPv6,
	)

	// Pre-warm cert. For MagicManager this issues + caches to disk
	// eagerly so the developer's first public request doesn't pay
	// ACME issuance latency.
	certMgr, err := buildCertManager(cfg)
	if err != nil {
		slog.Error("cert manager init failed", "err", err.Error())
		os.Exit(1)
	}
	if err := certMgr.PreWarm(*slug); err != nil {
		slog.Error("cert pre-warm failed", "slug", *slug, "err", err.Error())
		os.Exit(1)
	}
	slog.Info("cert pre-warmed", "slug", *slug, "issuance_count", certMgr.IssuanceCount())
}

func initCmd(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", "/etc/beamd/beamd.yaml", "where to write the server config")
	nonInteractive := fs.Bool("non-interactive", false, "skip prompts; use the flag values only")
	baseDomain := fs.String("base-domain", "", "e.g. tunnel.example.com")
	edgeIPv4 := fs.String("edge-ipv4", "", "this server's public IPv4")
	edgeIPv6 := fs.String("edge-ipv6", "", "this server's public IPv6 (optional)")
	acmeEmail := fs.String("acme-email", "", "Let's Encrypt contact email")
	dnsProvider := fs.String("dns-provider", "cloudflare", "libdns provider name")
	tokenStorePath := fs.String("token-store-path", "/etc/beamd/tokens.json", "where tokens.json lives")
	dataDir := fs.String("data-dir", "/var/lib/beamd", "where beamd persists state (cert cache, etc.)")
	force := fs.Bool("force", false, "overwrite an existing config file")
	_ = fs.Parse(args)

	if !*nonInteractive {
		fmt.Println("beamd init — interactive setup")
		fmt.Println("(press Enter to accept the [default] in brackets)")
		fmt.Println()
		r := bufio.NewReader(os.Stdin)
		*baseDomain = prompt(r, "base_domain (e.g. tunnel.example.com)", *baseDomain)
		*edgeIPv4 = prompt(r, "edge_ipv4 (this server's public IP)", *edgeIPv4)
		*edgeIPv6 = prompt(r, "edge_ipv6 (optional)", *edgeIPv6)
		*acmeEmail = prompt(r, "acme_email (Let's Encrypt contact)", *acmeEmail)
		*dnsProvider = prompt(r, "dns_provider", *dnsProvider)
		*tokenStorePath = prompt(r, "tokens.json path", *tokenStorePath)
		*dataDir = prompt(r, "data_dir", *dataDir)
	}

	missing := []string{}
	if *baseDomain == "" {
		missing = append(missing, "--base-domain")
	}
	if *edgeIPv4 == "" {
		missing = append(missing, "--edge-ipv4")
	}
	if *acmeEmail == "" {
		missing = append(missing, "--acme-email")
	}
	if len(missing) > 0 {
		fmt.Fprintln(os.Stderr, "missing required values:", strings.Join(missing, ", "))
		os.Exit(2)
	}

	if !*force {
		if _, err := os.Stat(*configPath); err == nil {
			fmt.Fprintln(os.Stderr, "refusing to overwrite existing config at", *configPath)
			fmt.Fprintln(os.Stderr, "pass --force to override")
			os.Exit(2)
		}
	}

	cfgBody := fmt.Sprintf(`# generated by `+"`beamd init`"+`
base_domain: %s
edge_ipv4: %s
edge_ipv6: %q

listen_https: ":443"

acme_email: %s
acme_ca: ""                    # blank = Let's Encrypt production

dns_provider: %s
dns_provider_creds: ""         # set via BEAMD_DNS_PROVIDER_CREDS env

token_store: %q
data_dir: %s

max_tunnels_per_token: 25
max_request_body_bytes: 33554432
`,
		*baseDomain, *edgeIPv4, *edgeIPv6, *acmeEmail, *dnsProvider,
		"file:"+*tokenStorePath, *dataDir,
	)

	if err := os.MkdirAll(filepath.Dir(*configPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir config dir:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*configPath, []byte(cfgBody), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write config:", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(*tokenStorePath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir token dir:", err)
		os.Exit(1)
	}
	if _, err := os.Stat(*tokenStorePath); os.IsNotExist(err) {
		if err := os.WriteFile(*tokenStorePath, []byte("{}\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write empty tokens.json:", err)
			os.Exit(1)
		}
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir data_dir:", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("wrote:\n  %s\n  %s\n", *configPath, *tokenStorePath)
	fmt.Printf("data dir: %s\n", *dataDir)
	fmt.Println()
	fmt.Println("Next:")
	fmt.Printf("  1. export BEAMD_DNS_PROVIDER_CREDS=<your-DNS-provider-token>\n")
	fmt.Printf("  2. beamd add-developer --slug <yourname> --config %s\n", *configPath)
	fmt.Printf("  3. beamd serve --config %s\n", *configPath)
}

// prompt reads one line from r and returns it, or defaultVal if the
// user just pressed Enter.
func prompt(r *bufio.Reader, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("  %s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("  %s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return defaultVal
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal
	}
	return line
}

func addDeveloperCmd(args []string) {
	fs := flag.NewFlagSet("add-developer", flag.ExitOnError)
	configPath := fs.String("config", "/etc/beamd/beamd.yaml", "path to beamd.yaml")
	slug := fs.String("slug", "", "namespace this developer's tunnels under <slug> (omit for flat: <name>.<base>)")
	skipProvision := fs.Bool("skip-provision", false, "skip the DNS + cert provision step (token-only)")
	_ = fs.Parse(args)

	if *slug != "" {
		if err := naming.ValidateLabel(*slug); err != nil {
			fmt.Fprintln(os.Stderr, "invalid slug:", err)
			os.Exit(2)
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err.Error())
		os.Exit(1)
	}

	if !strings.HasPrefix(cfg.TokenStore, "file:") {
		fmt.Fprintln(os.Stderr, "add-developer only supports file: token stores (got:", cfg.TokenStore+")")
		os.Exit(2)
	}
	tokensPath := strings.TrimPrefix(cfg.TokenStore, "file:")

	// Load existing tokens (or treat missing file as empty).
	tokens := map[string]string{}
	if b, err := os.ReadFile(tokensPath); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &tokens); err != nil {
			fmt.Fprintln(os.Stderr, "parse existing tokens.json:", err)
			os.Exit(1)
		}
	}

	// A non-empty slug is one developer → one token; refuse a duplicate.
	// Flat ("") tokens are the shared root namespace, so multiple are fine
	// (e.g. several of your own machines).
	if *slug != "" {
		for _, existing := range tokens {
			if existing == *slug {
				fmt.Fprintf(os.Stderr, "slug %q already has a token in %s — refusing to issue another\n", *slug, tokensPath)
				fmt.Fprintln(os.Stderr, "if you need to rotate, delete the existing entry first")
				os.Exit(2)
			}
		}
	}

	// Generate a fresh 32-byte token (64 hex chars).
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		fmt.Fprintln(os.Stderr, "rand:", err)
		os.Exit(1)
	}
	token := hex.EncodeToString(buf)
	tokens[token] = *slug

	body, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal tokens:", err)
		os.Exit(1)
	}
	body = append(body, '\n')
	if err := atomicWrite(tokensPath, body, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write tokens.json:", err)
		os.Exit(1)
	}

	// Provision DNS + pre-warm cert (the existing provision-dev flow).
	if !*skipProvision {
		p, err := dns.Open(cfg.DNSProvider, cfg.DNSProviderCreds)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open dns provider:", err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		zone, err := resolveZone(ctx, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve dns zone:", err)
			os.Exit(1)
		}
		if err := dns.ProvisionSlug(ctx, p, zone, cfg.BaseDomain, *slug, cfg.EdgeIPv4, cfg.EdgeIPv6); err != nil {
			fmt.Fprintln(os.Stderr, "provision dns:", err)
			os.Exit(1)
		}
		certMgr, err := buildCertManager(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "build cert manager:", err)
			os.Exit(1)
		}
		if err := certMgr.PreWarm(*slug); err != nil {
			fmt.Fprintln(os.Stderr, "pre-warm cert:", err)
			os.Exit(1)
		}
	}

	exampleHost := "api." + cfg.BaseDomain
	if *slug != "" {
		exampleHost = "api." + *slug + "." + cfg.BaseDomain
	}

	fmt.Println()
	fmt.Println("developer added:")
	if *slug == "" {
		fmt.Printf("  routing: flat — tunnels at <name>.%s\n", cfg.BaseDomain)
	} else {
		fmt.Printf("  slug:    %s — tunnels at <name>.%s.%s\n", *slug, *slug, cfg.BaseDomain)
	}
	fmt.Printf("  token:   %s\n", token)
	fmt.Println()
	fmt.Println("Restart beamd to pick up the new token (the file is read at startup):")
	fmt.Println("  docker restart beamd        # if running under Docker")
	fmt.Println("  systemctl restart beamd     # if running as a systemd unit")
	fmt.Println()
	fmt.Println("Developer setup (their laptop):")
	fmt.Printf("  beamd login --server %s --token <token above>\n", cfg.BaseDomain)
	fmt.Printf("  beamd open 3001 --as api      # → https://%s\n", exampleHost)
}

// atomicWrite writes data to path via a sibling tmpfile + rename, so a
// crash mid-write doesn't truncate the existing tokens file.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// buildCertManager picks the right cert.Manager for the config:
// `acme_ca: off` or `self_signed` → SelfSignedManager (dev/test);
// anything else → MagicManager with ACME DNS-01 via the configured
// libdns provider.
func buildCertManager(cfg *config.Server) (certs.Manager, error) {
	if cfg.ACMECA == "off" || cfg.ACMECA == "self_signed" {
		slog.Info("certs: using self-signed manager (acme_ca: off)")
		return certs.NewSelfSignedManager(cfg.BaseDomain)
	}

	dp, err := dns.Open(cfg.DNSProvider, cfg.DNSProviderCreds)
	if err != nil {
		return nil, fmt.Errorf("dns provider %q: %w", cfg.DNSProvider, err)
	}

	storageDir := filepath.Join(cfg.DataDir, "certs")
	slog.Info("certs: using ACME manager",
		"acme_ca", cfg.ACMECA,
		"dns_provider", cfg.DNSProvider,
		"storage_dir", storageDir,
	)
	// Manage the apex eagerly so /.well-known/beam-auth (and /healthz, /metrics)
	// serve a real cert. For the hyphen/flat shapes the single `*.<base>` wildcard
	// covers every tunnel, so issue it eagerly too (instant tunnels); the subdomain
	// shape's per-slug `*.<slug>.<base>` certs stay lazy (issued on first handshake).
	eager := []string{cfg.BaseDomain}
	if cfg.Shape() != naming.ShapeSubdomain {
		eager = append(eager, "*."+cfg.BaseDomain)
	}
	return certs.NewMagicManager(certs.MagicConfig{
		BaseDomain:  cfg.BaseDomain,
		ACMEEmail:   cfg.ACMEEmail,
		ACMECA:      cfg.ACMECA,
		DNSProvider: dp,
		StorageDir:  storageDir,
		EagerNames:  eager,
		// Custom domains (url-model §8.2, path B): On-Demand TLS gated by the
		// control plane's verified-domain allowlist (resolve-host).
		OnDemandDecision: onDemandDecision(cfg),
	})
}

// onDemandDecision returns the custom-domain cert authorizer when the edge is in
// hosted mode (an HTTP token store), else nil (a self-host edge serves no custom
// domains). It gates On-Demand TLS issuance on the control plane's
// verified-domain allowlist via GET /api/internal/resolve-host (derived from the
// verify-token endpoint, same shared secret).
func onDemandDecision(cfg *config.Server) func(context.Context, string) error {
	ts := cfg.TokenStore
	if !strings.HasPrefix(ts, "http://") && !strings.HasPrefix(ts, "https://") {
		return nil
	}
	endpoint := strings.Replace(ts, "/verify-token", "/resolve-host", 1)
	if endpoint == ts {
		if i := strings.LastIndex(ts, "/"); i >= 0 {
			endpoint = ts[:i] + "/resolve-host"
		}
	}
	return certs.NewResolveHostAuthorizer(endpoint, os.Getenv("BEAMD_AUTH_VERIFY_SECRET"))
}
