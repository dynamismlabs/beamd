// Package usage pushes per-slug usage deltas (bytes proxied, requests,
// active tunnels) to a configurable webhook on an interval. The hosted
// beamd wires this at the web app's billing endpoint; OSS leaves
// it unset and exposes the same data at `/metrics` for Prometheus.
//
// The reporter sends DELTAS rather than cumulative totals so a
// beamd restart doesn't double-count, and persists the
// "last reported" state to disk so deltas are correct across restarts.
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot is a point-in-time read of the edge's per-slug counters.
type Snapshot struct {
	// Cumulative bytes proxied since process start, per slug.
	BytesBySlug map[string]int64

	// Currently registered tunnels, per slug. Sample-in-time, not cumulative.
	TunnelsBySlug map[string]int64

	// Cumulative public-facing request count since process start.
	RequestsTotal int64
}

// Source is what the reporter reads counters from. Edge satisfies it.
type Source interface {
	UsageSnapshot() Snapshot
}

type Config struct {
	WebhookURL string
	// Secret is sent as `Authorization: Bearer <secret>` if non-empty.
	Secret    string
	Interval  time.Duration
	StateFile string
}

type Reporter struct {
	cfg    Config
	source Source
	http   *http.Client

	mu               sync.Mutex
	lastBytesBySlug  map[string]int64
	lastRequestTotal int64
	lastReportedAt   time.Time
}

// usageEvent is one line item on the wire. Bytes is a delta over the
// reporting period; ActiveTunnels is a snapshot at period end.
type usageEvent struct {
	Slug          string `json:"slug"`
	Bytes         int64  `json:"bytes"`
	ActiveTunnels int64  `json:"active_tunnels"`
	PeriodStart   string `json:"period_start"`
	PeriodEnd     string `json:"period_end"`
}

type usageReportBody struct {
	Events             []usageEvent `json:"events"`
	RequestsTotalDelta int64        `json:"requests_total_delta"`
}

func NewReporter(cfg Config, source Source) (*Reporter, error) {
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("WebhookURL is required")
	}
	if cfg.StateFile == "" {
		return nil, fmt.Errorf("StateFile is required (state-persistence is what makes deltas correct across restarts)")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}

	r := &Reporter{
		cfg:             cfg,
		source:          source,
		http:            &http.Client{Timeout: 10 * time.Second},
		lastBytesBySlug: make(map[string]int64),
		lastReportedAt:  time.Now(),
	}
	if err := r.loadState(); err != nil {
		// Stale or missing state is not fatal — we just start "from
		// zero" and the first report will include all current totals.
		slog.Warn("usage reporter: state load failed; starting fresh", "err", err.Error())
	}
	return r, nil
}

// Run reports on each tick of cfg.Interval. Cancellation triggers a
// final report so the bill doesn't lose the last partial period.
func (r *Reporter) Run(ctx context.Context) {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	slog.Info("usage reporter: started", "url", r.cfg.WebhookURL, "interval", r.cfg.Interval)
	for {
		select {
		case <-ctx.Done():
			if err := r.Report(); err != nil {
				slog.Warn("usage reporter: final report failed", "err", err.Error())
			}
			return
		case <-tick.C:
			if err := r.Report(); err != nil {
				slog.Warn("usage reporter: report failed (will retry next tick with wider window)", "err", err.Error())
			}
		}
	}
}

// Report computes deltas since the last successful report and POSTs
// them. Safe to call directly from tests.
func (r *Reporter) Report() error {
	snap := r.source.UsageSnapshot()
	now := time.Now()

	r.mu.Lock()
	start := r.lastReportedAt
	prevBytes := copyMap(r.lastBytesBySlug)
	prevRequests := r.lastRequestTotal
	r.mu.Unlock()

	var events []usageEvent
	for slug, current := range snap.BytesBySlug {
		delta := current - prevBytes[slug]
		// Negative delta means the counter reset (process restart). The
		// pre-reset totals were already reported; only count the new bytes.
		if delta < 0 {
			delta = current
		}
		if delta == 0 && snap.TunnelsBySlug[slug] == 0 {
			continue
		}
		events = append(events, usageEvent{
			Slug:          slug,
			Bytes:         delta,
			ActiveTunnels: snap.TunnelsBySlug[slug],
			PeriodStart:   start.UTC().Format(time.RFC3339),
			PeriodEnd:     now.UTC().Format(time.RFC3339),
		})
	}

	requestsDelta := snap.RequestsTotal - prevRequests
	if requestsDelta < 0 {
		requestsDelta = snap.RequestsTotal
	}

	if len(events) == 0 && requestsDelta == 0 {
		// Nothing changed. Still bump the last-reported time so the
		// next period starts cleanly.
		r.mu.Lock()
		r.lastReportedAt = now
		r.mu.Unlock()
		return nil
	}

	body := usageReportBody{
		Events:             events,
		RequestsTotalDelta: requestsDelta,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, r.cfg.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.Secret)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", r.cfg.WebhookURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}

	// Commit: advance the watermark so the next period reports against
	// these totals.
	r.mu.Lock()
	for slug, current := range snap.BytesBySlug {
		r.lastBytesBySlug[slug] = current
	}
	r.lastRequestTotal = snap.RequestsTotal
	r.lastReportedAt = now
	r.mu.Unlock()

	if err := r.saveState(); err != nil {
		slog.Warn("usage reporter: state save failed", "err", err.Error())
	}
	return nil
}

type persistedState struct {
	LastBytesBySlug  map[string]int64 `json:"last_bytes_by_slug"`
	LastRequestTotal int64            `json:"last_request_total"`
	LastReportedAt   time.Time        `json:"last_reported_at"`
}

func (r *Reporter) loadState() error {
	b, err := os.ReadFile(r.cfg.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s persistedState
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	r.mu.Lock()
	if s.LastBytesBySlug != nil {
		r.lastBytesBySlug = s.LastBytesBySlug
	}
	r.lastRequestTotal = s.LastRequestTotal
	if !s.LastReportedAt.IsZero() {
		r.lastReportedAt = s.LastReportedAt
	}
	r.mu.Unlock()
	return nil
}

func (r *Reporter) saveState() error {
	r.mu.Lock()
	state := persistedState{
		LastBytesBySlug:  copyMap(r.lastBytesBySlug),
		LastRequestTotal: r.lastRequestTotal,
		LastReportedAt:   r.lastReportedAt,
	}
	r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(r.cfg.StateFile), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// Atomic-ish write: tempfile + rename.
	tmp := r.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.cfg.StateFile)
}

func copyMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
