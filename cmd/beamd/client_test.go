package main

import (
	"net"
	"reflect"
	"testing"
	"time"
)

func TestHoistFlags(t *testing.T) {
	vf := map[string]bool{"as": true, "config": true}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flags already first", []string{"--as", "api", "3000"}, []string{"--as", "api", "3000"}},
		{"flag after positional", []string{"3000", "--as", "api"}, []string{"--as", "api", "3000"}},
		{"bare bool flags hoisted, no value grabbed", []string{"3000", "-d", "--json"}, []string{"-d", "--json", "3000"}},
		{"value flag grabs its value", []string{"3000", "--as", "api", "-d"}, []string{"--as", "api", "-d", "3000"}},
		{"equals form is not value-consuming", []string{"3000", "--as=api"}, []string{"--as=api", "3000"}},
		{"lone dash is positional", []string{"-", "3000"}, []string{"-", "3000"}},
		{"non-value flag does not grab next", []string{"--json", "3000"}, []string{"--json", "3000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hoistFlags(tc.in, vf); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hoistFlags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		name             string
		in               []string
		wantRun, wantCmd []string
		wantOK           bool
	}{
		{"name then cmd", []string{"app", "--", "npm", "run", "dev"}, []string{"app"}, []string{"npm", "run", "dev"}, true},
		{"flags+name then cmd", []string{"--json", "app", "--", "serve"}, []string{"--json", "app"}, []string{"serve"}, true},
		{"no separator", []string{"app", "npm"}, nil, nil, false},
		{"separator at end", []string{"app", "--"}, nil, nil, false},
		{"separator first, cmd only", []string{"--", "ls"}, []string{}, []string{"ls"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRun, gotCmd, ok := splitRunArgs(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(gotRun, tc.wantRun) || !reflect.DeepEqual(gotCmd, tc.wantCmd) {
				t.Errorf("splitRunArgs(%v) = (%v, %v), want (%v, %v)", tc.in, gotRun, gotCmd, tc.wantRun, tc.wantCmd)
			}
		})
	}
}

func TestNormalizeServerAddr(t *testing.T) {
	cases := map[string]string{
		"demobeamd.dynami.sm":          "demobeamd.dynami.sm:443",
		"demobeamd.dynami.sm:443":      "demobeamd.dynami.sm:443",
		"demobeamd.dynami.sm:8443":     "demobeamd.dynami.sm:8443",
		"https://demobeamd.dynami.sm":  "demobeamd.dynami.sm:443",
		"https://demobeamd.dynami.sm/": "demobeamd.dynami.sm:443",
		"http://x.com:8443":            "x.com:8443",
		"  x.com  ":                    "x.com:443",
		"":                             "",
	}
	for in, want := range cases {
		if got := normalizeServerAddr(in); got != want {
			t.Errorf("normalizeServerAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWaitListening(t *testing.T) {
	never := make(chan struct{})

	// A live listener → true.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	livePort := ln.Addr().(*net.TCPAddr).Port
	if !waitListening(livePort, 2*time.Second, never) {
		t.Errorf("waitListening on a live port = false, want true")
	}

	// Nobody listening → false on timeout.
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()
	if waitListening(deadPort, 400*time.Millisecond, never) {
		t.Errorf("waitListening on a dead port = true, want false")
	}

	// Child already exited → false immediately, even if the port is live.
	exited := make(chan struct{})
	close(exited)
	if waitListening(livePort, 2*time.Second, exited) {
		t.Errorf("waitListening with an exited child = true, want false")
	}
}
