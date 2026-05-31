package main

// Profile management commands: `beamd use`, `beamd profiles`, `beamd logout`.
// Login creates/updates profiles (see loginCmd); these select, list, and
// remove them.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/naming"
)

// useCmd sets the current (default) profile.
func useCmd(args []string) {
	fs := flag.NewFlagSet("use", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: beamd use <profile>")
		os.Exit(2)
	}
	name := fs.Arg(0)
	if err := naming.ValidateLabel(name); err != nil {
		fmt.Fprintf(os.Stderr, "invalid profile name %q\n", name)
		os.Exit(2)
	}
	if !config.ProfileExists(name) {
		fmt.Fprintf(os.Stderr, "no such profile %q — run `beamd profiles` to list, or `beamd login --profile %s`\n", name, name)
		os.Exit(2)
	}
	g, err := config.LoadGlobal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load global config:", err)
		os.Exit(1)
	}
	g.Current = name
	if err := config.SaveGlobal(g); err != nil {
		fmt.Fprintln(os.Stderr, "save global config:", err)
		os.Exit(1)
	}
	fmt.Printf("now using profile %q\n", name)
}

// profilesCmd lists profiles, marking the current one. (Slug is learned from
// the edge on connect and not persisted, so it's not shown here.)
func profilesCmd(args []string) {
	fs := flag.NewFlagSet("profiles", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print a JSON array and nothing else")
	_ = fs.Parse(args)

	names, err := config.ListProfiles()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list profiles:", err)
		os.Exit(1)
	}
	g, _ := config.LoadGlobal()

	type row struct {
		Name    string `json:"name"`
		Server  string `json:"server"`
		Current bool   `json:"current"`
	}
	rows := []row{}
	for _, n := range names {
		server := ""
		if c, err := config.LoadProfile(n); err == nil {
			server = c.Server
		}
		rows = append(rows, row{Name: n, Server: server, Current: n == g.Current})
	}

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(rows)
		return
	}
	if len(rows) == 0 {
		fmt.Println("(no profiles — run `beamd login`)")
		return
	}
	for _, r := range rows {
		marker := " "
		if r.Current {
			marker = "*"
		}
		fmt.Printf("%s %-16s %s\n", marker, r.Name, r.Server)
	}
}

// logoutCmd removes a profile (default: current). If the removed profile was
// current, it repoints current at another profile (or clears it).
func logoutCmd(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	profileFlag := fs.String("profile", "", "profile to remove (default: current)")
	fs.StringVar(profileFlag, "p", "", "shorthand for --profile")
	_ = fs.Parse(hoistFlags(args, map[string]bool{"profile": true, "p": true}))

	g, err := config.LoadGlobal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load global config:", err)
		os.Exit(1)
	}

	name := *profileFlag
	if name == "" {
		name = g.Current
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "logout: no profile specified and no current profile")
		os.Exit(2)
	}
	if err := naming.ValidateLabel(name); err != nil {
		fmt.Fprintf(os.Stderr, "invalid profile name %q\n", name)
		os.Exit(2)
	}
	if !config.ProfileExists(name) {
		fmt.Fprintf(os.Stderr, "no such profile %q\n", name)
		os.Exit(2)
	}
	if err := config.DeleteProfile(name); err != nil {
		fmt.Fprintln(os.Stderr, "remove profile:", err)
		os.Exit(1)
	}

	if g.Current == name {
		remaining, _ := config.ListProfiles()
		if len(remaining) > 0 {
			g.Current = remaining[0]
		} else {
			g.Current = ""
		}
		_ = config.SaveGlobal(g)
	}
	fmt.Printf("logged out of profile %q\n", name)
	if g.Current != "" && g.Current != name {
		fmt.Printf("current profile is now %q\n", g.Current)
	}
}
