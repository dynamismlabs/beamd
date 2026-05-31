package e2e

// Profile-machinery CLI test: drives the real binary through login (two
// profiles), profiles, use, and status — no edge needed, since the
// token-login path saves a profile without connecting. Exercises §1's
// acceptance: be logged into two edges at once and switch with a flag/env/
// default, no logout churn.

import (
	"encoding/json"
	"strings"
	"testing"
)

type profileRow struct {
	Name    string `json:"name"`
	Server  string `json:"server"`
	Current bool   `json:"current"`
}

type statusOut struct {
	Profile      string `json:"profile"`
	AgentRunning bool   `json:"agentRunning"`
	Server       string `json:"server"`
}

func TestCLI_ProfileFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	beamd, _ := buildBinaries(t)
	env := envWithHome(t.TempDir())

	// Log into two edges as profiles a and b (token path = no network).
	if _, se, code := runBeamd(t, env, beamd, "login", "--server", "a.example.com", "--token", "TA", "--profile", "a"); code != 0 {
		t.Fatalf("login a: exit %d: %s", code, se)
	}
	if _, se, code := runBeamd(t, env, beamd, "login", "--server", "b.example.com", "--token", "TB", "--profile", "b"); code != 0 {
		t.Fatalf("login b: exit %d: %s", code, se)
	}

	// profiles --json: both present; a is current (first created).
	rows := readProfiles(t, env, beamd)
	if len(rows) != 2 {
		t.Fatalf("profiles = %+v, want 2", rows)
	}
	if !current(rows, "a") || current(rows, "b") {
		t.Errorf("after two logins, expected a current, b not: %+v", rows)
	}
	if serverOf(rows, "a") != "a.example.com:443" {
		t.Errorf("profile a server = %q, want a.example.com:443", serverOf(rows, "a"))
	}

	// use b → b becomes current.
	if _, se, code := runBeamd(t, env, beamd, "use", "b"); code != 0 {
		t.Fatalf("use b: exit %d: %s", code, se)
	}
	rows = readProfiles(t, env, beamd)
	if !current(rows, "b") || current(rows, "a") {
		t.Errorf("after `use b`, expected b current: %+v", rows)
	}

	// status -p a reports profile a regardless of current.
	st := readStatus(t, env, beamd, "-p", "a")
	if st.Profile != "a" || st.Server != "a.example.com:443" || st.AgentRunning {
		t.Errorf("status -p a = %+v, want profile a / a.example.com:443 / not running", st)
	}
	// bare status follows current (b).
	if st := readStatus(t, env, beamd); st.Profile != "b" {
		t.Errorf("bare status profile = %q, want b (current)", st.Profile)
	}
	// BEAMD_PROFILE overrides current.
	if st := readStatus(t, append(env, "BEAMD_PROFILE=a"), beamd); st.Profile != "a" {
		t.Errorf("BEAMD_PROFILE=a status profile = %q, want a", st.Profile)
	}

	// logout a; b survives and stays current.
	if _, se, code := runBeamd(t, env, beamd, "logout", "--profile", "a"); code != 0 {
		t.Fatalf("logout a: exit %d: %s", code, se)
	}
	rows = readProfiles(t, env, beamd)
	if len(rows) != 1 || rows[0].Name != "b" || !rows[0].Current {
		t.Errorf("after logout a: %+v, want only b (current)", rows)
	}
}

func readProfiles(t *testing.T, env []string, beamd string) []profileRow {
	t.Helper()
	out, se, code := runBeamd(t, env, beamd, "profiles", "--json")
	if code != 0 {
		t.Fatalf("profiles --json: exit %d: %s", code, se)
	}
	var rows []profileRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("profiles --json not valid: %q (%v)", out, err)
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

func current(rows []profileRow, name string) bool {
	for _, r := range rows {
		if r.Name == name {
			return r.Current
		}
	}
	return false
}

func serverOf(rows []profileRow, name string) string {
	for _, r := range rows {
		if r.Name == name {
			return r.Server
		}
	}
	return ""
}
