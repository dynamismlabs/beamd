package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/daemon"
)

// checkCmd authenticates against the edge and reports identity WITHOUT
// registering a tunnel or spawning the long-lived agent — a cheap
// "test connection" (vs opening a throwaway probe tunnel).
func checkCmd(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print one JSON object and nothing else")
	insecure := fs.Bool("insecure", false, "skip edge TLS verification (self-signed dev edges only)")
	transportFlag := fs.String("transport", "", "force transport for this check: tcp, quic, or auto")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames("transport")))

	rc := resolveContext(cf)
	cfg := rc.mustAuth()
	ins := *insecure || cfg.InsecureSkipVerify
	transportMode := *transportFlag
	if transportMode == "" {
		transportMode = mustTransport(cfg.Transport)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	started := time.Now()
	c, err := client.Connect(ctx, cfg.Server, cfg.Token, client.Options{
		InsecureSkipVerify:     ins,
		Scope:                  rc.Scope,
		YamuxStreamWindowBytes: mustYamuxWindow(),
		Transport:              transportMode,
	})
	handshakeMs := time.Since(started).Milliseconds()
	cancel()
	if err != nil {
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(struct {
				Ok          bool   `json:"ok"`
				Server      string `json:"server"`
				Error       string `json:"error"`
				Transport   string `json:"transport,omitempty"`
				HandshakeMs int64  `json:"handshakeMs"`
			}{false, cfg.Server, err.Error(), transportMode, handshakeMs})
		} else {
			fmt.Fprintln(os.Stderr, "check failed:", err)
		}
		os.Exit(1)
	}
	slug, base := c.Slug(), c.BaseDomain()
	selectedTransport := string(c.Transport())
	_ = c.Close() // no tunnel was registered; just drop the control session

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Ok          bool   `json:"ok"`
			Server      string `json:"server"`
			Slug        string `json:"slug"`
			BaseDomain  string `json:"baseDomain"`
			Transport   string `json:"transport"`
			HandshakeMs int64  `json:"handshakeMs"`
		}{true, cfg.Server, slug, base, selectedTransport, handshakeMs})
		return
	}
	fmt.Printf("ok\nserver:    %s\nslug:      %s\nbase:      %s\ntransport: %s\nhandshake: %dms\n",
		cfg.Server, orDash(slug), base, selectedTransport, handshakeMs)
}

// reloadCmd restarts the selected profile's background agent so a changed
// server/token takes effect immediately (a long-lived agent caches its creds
// for its lifetime). Stops any agent on the socket, then spawns a fresh one.
func reloadCmd(args []string) {
	fs := flag.NewFlagSet("reload", flag.ExitOnError)
	insecure := fs.Bool("insecure", false, "skip edge TLS verification (self-signed dev edges only)")
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)
	cfg := rc.mustAuth()

	if rc.AgentSocket != "" && daemon.IsRunning(rc.AgentSocket) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = daemon.NewLocalClient(rc.AgentSocket).Shutdown(ctx)
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for daemon.IsRunning(rc.AgentSocket) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if daemon.IsRunning(rc.AgentSocket) {
			fmt.Fprintln(os.Stderr, "reload: previous agent did not stop in time")
			os.Exit(1)
		}
	}

	ins := *insecure || cfg.InsecureSkipVerify
	lc := ensureAgent(rc.ConfigPath, rc.AgentSocket, rc.Scope, ins)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	slug := ""
	if h, err := lc.Ping(ctx); err == nil {
		slug = h.Slug
	}
	if rc.Server != "" {
		fmt.Printf("agent reloaded (%s, slug %s)\n", rc.Server, orDash(slug))
	} else {
		fmt.Printf("agent reloaded (slug %s)\n", orDash(slug))
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
