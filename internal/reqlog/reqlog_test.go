package reqlog

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIDIsUUIDv7Shaped(t *testing.T) {
	id := NewID()
	// 8-4-4-4-12 with version nibble '7'
	parts := strings.Split(id, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || parts[2][0] != '7' {
		t.Fatalf("NewID() = %q, not a uuidv7 shape", id)
	}
	if NewID() == id {
		t.Errorf("NewID() returned duplicates")
	}
}

func TestTruncateIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.45":      "203.0.113.0",
		"2001:db8::1":       "2001:db8::",
		"2001:db8:1:2:3::4": "2001:db8:1::", // exercises zeroing bytes 6..15 (/48)
		"not-an-ip":         "",
	}
	for in, want := range cases {
		if got := TruncateIP(in); got != want {
			t.Errorf("TruncateIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileSinkWritesLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	fs, err := NewFileSink(FileSinkConfig{Path: path, Fsync: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		fs.Record(RequestEvent{RequestID: NewID(), Host: "api-acme.beamd.run", Method: "GET", Status: 200, Outcome: OutcomeOK})
	}
	fs.Close() // drains

	f, _ := os.Open(path)
	defer f.Close()
	var n int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev RequestEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad line: %v", err)
		}
		if ev.Host != "api-acme.beamd.run" {
			t.Errorf("host = %q", ev.Host)
		}
		n++
	}
	if n != 3 {
		t.Errorf("wrote %d lines, want 3", n)
	}
}

// drainFile must NOT advance the cursor when a ship fails — this is what stops
// rotation-follow from losing the rotated file's tail on a control-plane outage.
func TestDrainFileDoesNotAdvanceOnShipFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.log")
	f, _ := os.Create(path)
	for i := 0; i < 3; i++ {
		b, _ := json.Marshal(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
		f.Write(append(b, '\n'))
	}
	f.Close()

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer fail.Close()
	s := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c"), WebhookURL: fail.URL, Secret: "x", BatchSize: 10})
	cur := cursor{}
	if s.drainFile(path, &cur) {
		t.Errorf("drainFile must return false when a ship fails (do not advance)")
	}
	if cur.Offset != 0 {
		t.Errorf("cursor must stay at 0 on ship failure, got %d", cur.Offset)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ok.Close()
	s2 := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c2"), WebhookURL: ok.URL, Secret: "x", BatchSize: 10})
	cur2 := cursor{}
	if !s2.drainFile(path, &cur2) {
		t.Errorf("drainFile must return true when fully drained")
	}
	if cur2.Offset == 0 {
		t.Errorf("cursor must advance after a full drain")
	}
}

func TestShipperTailsAndAdvances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	cursorPath := filepath.Join(dir, "requests.cursor")

	// Write 5 complete lines + one partial (no newline) — the partial must NOT ship.
	f, _ := os.Create(path)
	for i := 0; i < 5; i++ {
		b, _ := json.Marshal(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
		f.Write(append(b, '\n'))
	}
	f.WriteString(`{"partial":`) // no trailing newline
	f.Close()

	var mu sync.Mutex
	var received int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(401)
			return
		}
		var body struct {
			Events []RequestEvent `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received += len(body.Events)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	sh := NewShipper(ShipperConfig{
		LogPath: path, CursorPath: cursorPath, WebhookURL: srv.URL,
		Secret: "s3cret", BatchSize: 2, Flush: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)
	// wait for the 5 complete lines to ship (partial held back)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := received
		mu.Unlock()
		if got >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if received != 5 {
		t.Errorf("shipped %d events, want 5 (the partial line must be held)", received)
	}
	// cursor advanced to just past the 5 complete lines
	b, _ := os.ReadFile(cursorPath)
	var c cursor
	_ = json.Unmarshal(b, &c)
	if c.Offset == 0 {
		t.Errorf("cursor did not advance")
	}
}

// A tailed sink must NOT clobber an unshipped <path>.1 on a second rotation: it
// defers (keeps appending to the live file, growing it past maxBytes) so no
// events are lost. Regression for the double-rotation billing-loss bug.
func TestRotateDefersWhenTailedAndDotOneUnshipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	fs, err := NewFileSink(FileSinkConfig{Path: path, MaxBytes: 300, Fsync: 5 * time.Millisecond, Tailed: true})
	if err != nil {
		t.Fatal(err)
	}
	const total = 30 // crosses maxBytes many times → would rotate repeatedly
	for i := 0; i < total; i++ {
		fs.Record(RequestEvent{RequestID: NewID(), Host: "api-acme.beamd.run", Method: "GET", Status: 200, Outcome: OutcomeOK})
	}
	fs.Close() // drains all events to disk

	dotOne := path + ".1"
	st1, err := os.Stat(dotOne)
	if err != nil {
		t.Fatalf("expected a rotated %s after crossing max: %v", dotOne, err)
	}
	if st1.Size() == 0 {
		t.Fatalf("%s is empty — its rotated tail was lost", dotOne)
	}
	stLive, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stLive.Size() <= 300 {
		t.Fatalf("live file did not grow past maxBytes; rotation was not deferred (size=%d)", stLive.Size())
	}
	// No loss: every event we wrote survives in (.1 + live) combined. A clobbering
	// second rotation would have dropped .1's tail → fewer than `total`.
	if got := countJSONLines(t, dotOne) + countJSONLines(t, path); got != total {
		t.Fatalf("events across .1+live = %d, want %d (double rotation lost data)", got, total)
	}
	if fs.Dropped() != 0 {
		t.Fatalf("dropped %d events, want 0", fs.Dropped())
	}
}

// An untailed (local-only) sink keeps the old rolling behavior: a second
// rotation may clobber .1, and the live file is bounded near maxBytes.
func TestRotateClobbersWhenUntailed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	fs, err := NewFileSink(FileSinkConfig{Path: path, MaxBytes: 300, Fsync: 5 * time.Millisecond}) // Tailed: false
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		fs.Record(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
	}
	fs.Close()
	// Live file should have been rotated (reset), so it stays near/under maxBytes
	// rather than growing without bound.
	stLive, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stLive.Size() > 300 {
		t.Fatalf("untailed live file should roll near maxBytes, got %d", stLive.Size())
	}
}

// After fully shipping the rotated <path>.1, the shipper must remove it so the
// sink can rotate again (the sink defers rotation while .1 still exists).
func TestShipperRemovesDotOneAfterShipping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	cursorPath := filepath.Join(dir, "requests.cursor")
	dotOne := path + ".1"

	// A rotated file (.1) with 3 lines + a fresh live file with 2 lines — distinct
	// inodes, so the shipper treats .1 as the pre-rotation file to finish first.
	writeJSONLines(t, dotOne, 3)
	writeJSONLines(t, path, 2)

	ino1, ok := inodeOf(dotOne)
	if !ok {
		t.Fatal("inodeOf(.1) failed")
	}
	b, _ := json.Marshal(cursor{Inode: ino1, Offset: 0}) // cursor points at .1's inode
	if err := os.WriteFile(cursorPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var received int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []RequestEvent `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received += len(body.Events)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	sh := NewShipper(ShipperConfig{LogPath: path, CursorPath: cursorPath, WebhookURL: srv.URL, Secret: "x", BatchSize: 10})
	sh.drain(sh.loadCursor())

	if _, err := os.Stat(dotOne); !os.IsNotExist(err) {
		t.Fatalf("%s should be removed after full shipping, stat err=%v", dotOne, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if received != 5 {
		t.Fatalf("shipped %d events, want 5 (3 from .1 + 2 from live)", received)
	}
}

func countJSONLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var ev RequestEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad line in %s: %v", path, err)
		}
		n++
	}
	return n
}

func writeJSONLines(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < n; i++ {
		b, _ := json.Marshal(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}
