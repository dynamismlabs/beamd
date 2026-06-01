package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestCLI_Check exercises `beamd check`: it authenticates and reports
// identity without registering a tunnel or spawning the long-lived agent.
func TestCLI_Check(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary + spawns the edge")
	}
	beamd, _ := buildBinaries(t)
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	env := envWithHome(t.TempDir())

	// Valid creds → ok:true with identity, and NO agent spawned.
	cfg, socket := writeCLIConfig(t, edgeAddr, "T1")
	out, errOut, code := runBeamd(t, env, beamd, "check", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("check exit=%d stderr=%s", code, errOut)
	}
	var res struct {
		Ok         bool   `json:"ok"`
		Server     string `json:"server"`
		Slug       string `json:"slug"`
		BaseDomain string `json:"baseDomain"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("check --json not one object: %q (%v)", out, err)
	}
	if !res.Ok || res.Slug != "turing" || res.BaseDomain != testBaseDomain {
		t.Fatalf("check --json = %+v, want ok slug=turing base=%s", res, testBaseDomain)
	}
	if _, err := os.Stat(socket); err == nil {
		t.Errorf("check must not spawn an agent, but socket %s exists", socket)
	}

	// Bad token → ok:false, non-zero exit, still emits the JSON object.
	badCfg, _ := writeCLIConfig(t, edgeAddr, "WRONG")
	out, _, code = runBeamd(t, env, beamd, "check", "--json", "--config", badCfg)
	if code == 0 {
		t.Error("check with a bad token should exit non-zero")
	}
	if !strings.Contains(out, `"ok":false`) {
		t.Errorf("check bad-token out = %q, want ok:false", out)
	}
}
