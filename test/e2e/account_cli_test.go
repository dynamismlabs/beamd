package e2e

// Account-machinery CLI test: drives the real binary through login (two
// edges), accounts, default, whoami, status, and logout — no edge needed,
// since the token-login path saves an account without connecting. Exercises
// the accounts + scope model: logged into many edges at once, scope as a
// selector (--scope / `beamd default`), current account is the default.

import (
	"encoding/json"
	"strings"
	"testing"
)

type accountRow struct {
	Server       string `json:"server"`
	Kind         string `json:"kind"`
	DefaultScope string `json:"defaultScope"`
	Current      bool   `json:"current"`
}

type statusOut struct {
	AgentRunning bool   `json:"agentRunning"`
	Server       string `json:"server"`
	Scope        string `json:"scope"`
}

type whoamiOut struct {
	Server string `json:"server"`
	Kind   string `json:"kind"`
	Scope  string `json:"scope"`
}

func TestCLI_AccountFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	beamd, _ := buildBinaries(t)
	env := envWithHome(t.TempDir())

	// Log into two edges (token path = no network).
	if _, se, code := runBeamd(t, env, beamd, "login", "--server", "a.example.com", "--token", "TA"); code != 0 {
		t.Fatalf("login a: exit %d: %s", code, se)
	}
	if _, se, code := runBeamd(t, env, beamd, "login", "--server", "b.example.com", "--token", "TB"); code != 0 {
		t.Fatalf("login b: exit %d: %s", code, se)
	}

	// accounts --json: both present; a is current (first created); kind token.
	rows := readAccounts(t, env, beamd)
	if len(rows) != 2 {
		t.Fatalf("accounts = %+v, want 2", rows)
	}
	if !currentAcct(rows, "a.example.com:443") || currentAcct(rows, "b.example.com:443") {
		t.Errorf("after two logins, expected a current, b not: %+v", rows)
	}
	if kindOf(rows, "a.example.com:443") != "token" {
		t.Errorf("account a kind = %q, want token", kindOf(rows, "a.example.com:443"))
	}

	// `default acme` on a → persisted as a's default scope.
	if _, se, code := runBeamd(t, env, beamd, "default", "acme", "--server", "a.example.com"); code != 0 {
		t.Fatalf("default: exit %d: %s", code, se)
	}
	rows = readAccounts(t, env, beamd)
	if scopeOf(rows, "a.example.com:443") != "acme" {
		t.Errorf("after `default acme`, a defaultScope = %q, want acme", scopeOf(rows, "a.example.com:443"))
	}

	// whoami resolves the default; --scope overrides it for one command.
	if wh := readWhoami(t, env, beamd, "--server", "a.example.com"); wh.Scope != "acme" {
		t.Errorf("whoami --server a scope = %q, want acme (default)", wh.Scope)
	}
	if wh := readWhoami(t, env, beamd, "--server", "a.example.com", "--scope", "beta"); wh.Scope != "beta" {
		t.Errorf("whoami --scope beta = %q, want beta (override)", wh.Scope)
	}

	// status --server a reports a + its default scope, not running.
	st := readStatus(t, env, beamd, "--server", "a.example.com")
	if st.Server != "a.example.com:443" || st.AgentRunning {
		t.Errorf("status --server a = %+v, want a.example.com:443 / not running", st)
	}
	if st.Scope != "acme" {
		t.Errorf("status --server a scope = %q, want acme (default)", st.Scope)
	}
	// BEAMD_SERVER overrides current.
	if st := readStatus(t, append(env, "BEAMD_SERVER=b.example.com"), beamd); st.Server != "b.example.com:443" {
		t.Errorf("BEAMD_SERVER=b status server = %q, want b.example.com:443", st.Server)
	}

	// logout a; b survives and becomes current.
	if _, se, code := runBeamd(t, env, beamd, "logout", "--server", "a.example.com"); code != 0 {
		t.Fatalf("logout a: exit %d: %s", code, se)
	}
	rows = readAccounts(t, env, beamd)
	if len(rows) != 1 || rows[0].Server != "b.example.com:443" || !rows[0].Current {
		t.Errorf("after logout a: %+v, want only b (current)", rows)
	}
}

func readAccounts(t *testing.T, env []string, beamd string) []accountRow {
	t.Helper()
	out, se, code := runBeamd(t, env, beamd, "accounts", "--json")
	if code != 0 {
		t.Fatalf("accounts --json: exit %d: %s", code, se)
	}
	var rows []accountRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("accounts --json not valid: %q (%v)", out, err)
	}
	return rows
}

func readStatus(t *testing.T, env []string, beamd string, args ...string) statusOut {
	t.Helper()
	out, se, code := runBeamd(t, env, beamd, append([]string{"status", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("status --json: exit %d: %s", code, se)
	}
	var st statusOut
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &st); err != nil {
		t.Fatalf("status --json not valid: %q (%v)", out, err)
	}
	return st
}

func readWhoami(t *testing.T, env []string, beamd string, args ...string) whoamiOut {
	t.Helper()
	out, se, code := runBeamd(t, env, beamd, append([]string{"whoami", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("whoami --json: exit %d: %s", code, se)
	}
	var wh whoamiOut
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &wh); err != nil {
		t.Fatalf("whoami --json not valid: %q (%v)", out, err)
	}
	return wh
}

func currentAcct(rows []accountRow, server string) bool {
	for _, r := range rows {
		if r.Server == server {
			return r.Current
		}
	}
	return false
}

func kindOf(rows []accountRow, server string) string {
	for _, r := range rows {
		if r.Server == server {
			return r.Kind
		}
	}
	return ""
}

func scopeOf(rows []accountRow, server string) string {
	for _, r := range rows {
		if r.Server == server {
			return r.DefaultScope
		}
	}
	return ""
}
