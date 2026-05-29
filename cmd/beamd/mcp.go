package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dynamismlabs/beamd/internal/mcp"
)

func mcpCmd(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "client config path")
	_ = fs.Parse(args)

	cfg := mustLoadConfig(*configPath)
	lc := ensureAgent(cfg, *configPath)

	srv := mcp.New(lc, os.Stdin, os.Stdout, "beamd", Version)
	if err := srv.Run(context.Background()); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}
