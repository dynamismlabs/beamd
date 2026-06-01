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
	cf := addClientFlags(fs)
	_ = fs.Parse(hoistFlags(args, clientFlagValueNames()))

	rc := resolveContext(cf)
	rc.mustAuth()
	lc := ensureAgent(rc.ConfigPath, rc.AgentSocket, rc.Client.InsecureSkipVerify)

	srv := mcp.New(lc, os.Stdin, os.Stdout, "beamd", Version)
	if err := srv.Run(context.Background()); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}
