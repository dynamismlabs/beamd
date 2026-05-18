package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockSource lets tests control what UsageSnapshot returns.
type mockSource struct {
	mu   sync.Mutex
	snap Snapshot
}

func (m *mockSource) set(s Snapshot) {
	m.mu.Lock()
	m.snap = s
	m.mu.Unlock()
}

func (m *mockSource) UsageSnapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy so the caller can't accidentally mutate.
	return Snapshot{
		BytesBySlug:   copyMap(m.snap.BytesBySlug),
		TunnelsBySlug: copyMap(m.snap.TunnelsBySlug),
		RequestsTotal: m.snap.RequestsTotal,
	}
}

func startReceiver(t *testing.T) (*httptest.Server, *[]usageReportBody, *[]string) {
	t.Helper()
	var (
		mu       sync.Mutex
		received []usageReportBody
		auths    []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body usageReportBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, body)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &received, &auths
}

func TestReporter_FirstReportSendsCurrentTotals(t *testing.T) {
	srv, received, _ := startReceiver(t)
	source := &mockSource{}
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 100, "alex": 50},
		TunnelsBySlug: map[string]int64{"trey": 2, "alex": 1},
		RequestsTotal: 5,
	})

	r, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Interval:   time.Hour,
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
	}, source)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Report(); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(*received) != 1 {
		t.Fatalf("got %d reports, want 1", len(*received))
	}

	body := (*received)[0]
	if body.RequestsTotalDelta != 5 {
		t.Errorf("requests delta = %d, want 5", body.RequestsTotalDelta)
	}
	got := bytesBySlug(body.Events)
	if got["trey"] != 100 || got["alex"] != 50 {
		t.Errorf("first-report bytes wrong: %v", got)
	}
}

func TestReporter_SecondReportSendsDeltas(t *testing.T) {
	srv, received, _ := startReceiver(t)
	source := &mockSource{}
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 100, "alex": 50},
		TunnelsBySlug: map[string]int64{"trey": 1, "alex": 1},
		RequestsTotal: 5,
	})

	r, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Interval:   time.Hour,
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Report(); err != nil {
		t.Fatal(err)
	}

	// trey added 75 bytes, alex unchanged, requests +3.
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 175, "alex": 50},
		TunnelsBySlug: map[string]int64{"trey": 1, "alex": 1},
		RequestsTotal: 8,
	})

	if err := r.Report(); err != nil {
		t.Fatal(err)
	}
	if len(*received) != 2 {
		t.Fatalf("got %d reports, want 2", len(*received))
	}
	body := (*received)[1]
	if body.RequestsTotalDelta != 3 {
		t.Errorf("requests delta = %d, want 3", body.RequestsTotalDelta)
	}
	got := bytesBySlug(body.Events)
	if got["trey"] != 75 {
		t.Errorf("trey delta = %d, want 75", got["trey"])
	}
	// alex had 0 delta AND nonzero active_tunnels — still reported.
	if _, ok := got["alex"]; !ok {
		t.Errorf("alex should still be reported when active_tunnels > 0; events: %+v", body.Events)
	}
}

func TestReporter_RestartTreatsResetAsNonNegative(t *testing.T) {
	srv, received, _ := startReceiver(t)
	source := &mockSource{}
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 1000},
		TunnelsBySlug: map[string]int64{"trey": 1},
		RequestsTotal: 50,
	})

	stateFile := filepath.Join(t.TempDir(), "state.json")
	r, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Interval:   time.Hour,
		StateFile:  stateFile,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Report(); err != nil {
		t.Fatal(err)
	}

	// Simulate conduitd restart: source returns 0 (fresh counters), but
	// state file still says we last reported 1000.
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 200},
		TunnelsBySlug: map[string]int64{"trey": 1},
		RequestsTotal: 10,
	})
	r2, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Interval:   time.Hour,
		StateFile:  stateFile,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Report(); err != nil {
		t.Fatal(err)
	}

	if len(*received) != 2 {
		t.Fatalf("got %d reports, want 2", len(*received))
	}
	// Second report should NOT be -800 — it should be the post-reset
	// 200, treated as "all new" because the counter clearly reset.
	got := bytesBySlug((*received)[1].Events)
	if got["trey"] != 200 {
		t.Errorf("post-reset delta = %d, want 200", got["trey"])
	}
}

func TestReporter_NoUsageNoEvent(t *testing.T) {
	srv, received, _ := startReceiver(t)
	source := &mockSource{}
	source.set(Snapshot{}) // empty

	r, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Interval:   time.Hour,
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Report(); err != nil {
		t.Fatal(err)
	}
	if len(*received) != 0 {
		t.Errorf("empty snapshot should produce no webhook call; got %d", len(*received))
	}
}

func TestReporter_SecretSentAsBearer(t *testing.T) {
	srv, _, auths := startReceiver(t)
	source := &mockSource{}
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 100},
		TunnelsBySlug: map[string]int64{"trey": 1},
		RequestsTotal: 1,
	})

	r, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Secret:     "s3cret",
		Interval:   time.Hour,
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Report(); err != nil {
		t.Fatal(err)
	}
	if len(*auths) == 0 || (*auths)[0] != "Bearer s3cret" {
		t.Errorf("Authorization = %v, want 'Bearer s3cret'", *auths)
	}
}

func TestReporter_FailedWebhookKeepsWatermark(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	source := &mockSource{}
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 100},
		TunnelsBySlug: map[string]int64{"trey": 1},
		RequestsTotal: 5,
	})

	r, err := NewReporter(Config{
		WebhookURL: srv.URL,
		Interval:   time.Hour,
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Report(); err == nil {
		t.Fatal("expected error on 500")
	}

	// Next report: source advanced, but because last attempt failed,
	// we should still see the FULL cumulative bytes (100→150 → delta 150,
	// not 50) — i.e. the watermark didn't advance.
	source.set(Snapshot{
		BytesBySlug:   map[string]int64{"trey": 150},
		TunnelsBySlug: map[string]int64{"trey": 1},
		RequestsTotal: 8,
	})

	// Verify watermark unchanged.
	r.mu.Lock()
	last := r.lastBytesBySlug["trey"]
	r.mu.Unlock()
	if last != 0 {
		t.Errorf("watermark advanced after failed report: %d", last)
	}
}

func bytesBySlug(events []usageEvent) map[string]int64 {
	out := make(map[string]int64, len(events))
	for _, e := range events {
		out[e.Slug] = e.Bytes
	}
	return out
}
