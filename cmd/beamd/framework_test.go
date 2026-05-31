package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestFindFrameworkBasename(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"vite"}, "vite"},
		{[]string{"vite", "dev"}, "vite"},
		{[]string{"/abs/path/vite", "dev"}, "vite"},
		{[]string{"npx", "vite"}, "vite"},
		{[]string{"bunx", "--bun", "vite", "dev"}, "vite"},
		{[]string{"yarn", "dlx", "vite"}, "vite"},
		{[]string{"yarn", "vite"}, "vite"}, // implicit bin
		{[]string{"pnpm", "exec", "astro", "dev"}, "astro"},
		{[]string{"npm", "run", "dev"}, ""}, // npm isn't a framework or runner
		{[]string{"next", "dev"}, ""},       // honors PORT, not in the table
		{[]string{"node", "server.js"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := findFrameworkBasename(tc.in); got != tc.want {
			t.Errorf("findFrameworkBasename(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInjectFrameworkFlags(t *testing.T) {
	// vite → --port + --strictPort + --host
	got, changed := injectFrameworkFlags([]string{"vite", "dev"}, 4321)
	want := []string{"vite", "dev", "--port", "4321", "--strictPort", "--host", "127.0.0.1"}
	if !changed || !reflect.DeepEqual(got, want) {
		t.Errorf("vite: got %v (changed=%v), want %v", got, changed, want)
	}

	// astro → --port + --host, no --strictPort
	got, _ = injectFrameworkFlags([]string{"astro", "dev"}, 4321)
	want = []string{"astro", "dev", "--port", "4321", "--host", "127.0.0.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("astro: got %v, want %v", got, want)
	}

	// Idempotent: user already passed --port → don't add another (still adds --host).
	got, _ = injectFrameworkFlags([]string{"vite", "--port", "9999"}, 4321)
	if strings.Count(strings.Join(got, " "), "--port") != 1 {
		t.Errorf("idempotent --port (space form) failed: %v", got)
	}
	// Equals form is detected too.
	got, _ = injectFrameworkFlags([]string{"vite", "--port=9999"}, 4321)
	if strings.Count(strings.Join(got, " "), "--port") != 1 {
		t.Errorf("idempotent --port=9999 failed: %v", got)
	}
	// User-supplied --host (equals form) suppresses injection.
	got, _ = injectFrameworkFlags([]string{"vite", "--host=0.0.0.0"}, 4321)
	if strings.Count(strings.Join(got, " "), "--host") != 1 {
		t.Errorf("idempotent --host=0.0.0.0 failed: %v", got)
	}

	// Non-framework → untouched.
	got, changed = injectFrameworkFlags([]string{"npm", "run", "dev"}, 4321)
	if changed || !reflect.DeepEqual(got, []string{"npm", "run", "dev"}) {
		t.Errorf("non-framework should be untouched: %v (changed=%v)", got, changed)
	}
}

func TestNextHint(t *testing.T) {
	if h := nextAllowedOriginsHint([]string{"next", "dev"}, "web.turing.example.com"); !strings.Contains(h, "allowedDevOrigins") || !strings.Contains(h, "web.turing.example.com") {
		t.Errorf("next hint missing detail: %q", h)
	}
	if h := nextAllowedOriginsHint([]string{"vite"}, "x"); h != "" {
		t.Errorf("non-next should produce no hint, got %q", h)
	}
}
