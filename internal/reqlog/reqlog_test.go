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
	"sync/atomic"
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

// A hard kill can leave the log ending mid-line; on reopen the sink must
// truncate the fragment so it never merges with the next append.
func TestSinkRepairsCrashTornTailOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	writeJSONLines(t, path, 2)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"request_id":"torn`) // no newline: simulated crash mid-flush
	f.Close()

	sink, err := NewFileSink(FileSinkConfig{Path: path, Fsync: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	sink.Record(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
	time.Sleep(50 * time.Millisecond)
	sink.Close()

	// countJSONLines fails the test on any non-JSON line, so this asserts the
	// fragment is gone AND the post-repair append produced a clean line.
	if n := countJSONLines(t, path); n != 3 {
		t.Errorf("lines = %d, want 3 (2 originals + 1 new; torn fragment truncated)", n)
	}
	if sink.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1 (the torn record must be counted)", sink.Dropped())
	}
}

// One corrupt line (e.g. torn by a crash before repair existed) must be
// skipped and counted — not shipped forever in a poison batch that wedges the
// pipeline.
func TestShipperSkipsCorruptLineAndAdvances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	f, _ := os.Create(path)
	b, _ := json.Marshal(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
	f.Write(append(b, '\n'))
	f.WriteString("{\"torn\":\"fragment{\"request_id\":\"merged\"}\n") // one corrupt merged line
	b2, _ := json.Marshal(RequestEvent{RequestID: NewID(), Host: "h", Method: "GET", Status: 200, Outcome: OutcomeOK})
	f.Write(append(b2, '\n'))
	f.Close()

	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("shipped batch is not valid JSON: %v", err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		for _, e := range body.Events {
			got = append(got, string(e))
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c"), WebhookURL: srv.URL, Secret: "x", BatchSize: 10})
	cur := cursor{}
	if !s.drainFile(path, &cur) {
		t.Fatal("drainFile must fully drain despite the corrupt line")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Errorf("shipped %d events, want 2 (corrupt line skipped)", len(got))
	}
	if cur.Offset == 0 {
		t.Error("cursor must advance past the corrupt line")
	}
}

// A transient read error (here: permissions) must NOT count as "drained" —
// the caller would delete a rotated file whose events never shipped.
func TestShipperTransientReadErrorDoesNotDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	writeJSONLines(t, path, 3)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)

	s := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c"), WebhookURL: "http://127.0.0.1:0", Secret: "x"})
	cur := cursor{}
	if s.drainFile(path, &cur) {
		t.Error("unreadable file must not report drained (would trigger deletion of unshipped data)")
	}

	// A genuinely missing file IS drained (nothing left to ship).
	cur2 := cursor{}
	if !s.drainFile(filepath.Join(dir, "gone.log"), &cur2) {
		t.Error("missing file should report drained")
	}
}

// A pre-existing rotated .1 with a fresh cursor (webhook enabled later, or
// cursor lost) must ship and be removed — otherwise rotation defers forever.
func TestShipperFreshCursorShipsPreexistingDotOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	writeJSONLines(t, path+".1", 4)
	writeJSONLines(t, path, 2)

	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []json.RawMessage `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		received.Add(int64(len(body.Events)))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c"), WebhookURL: srv.URL, Secret: "x"})
	cur := s.drain(cursor{}) // fresh cursor

	if got := received.Load(); got != 6 {
		t.Errorf("shipped %d events, want 6 (4 from .1 + 2 live)", got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Error(".1 must be removed after fully shipping")
	}
	if ino, _ := inodeOf(path); cur.Inode != ino {
		t.Errorf("cursor should track the live file after drain")
	}
}

// In-place truncation (same inode, e.g. `> requests.log`) leaves the cursor
// past EOF; the shipper must restart from 0 instead of stalling forever.
func TestShipperOffsetPastEOFRestartsFromZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	writeJSONLines(t, path, 2)

	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []json.RawMessage `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		received.Add(int64(len(body.Events)))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c"), WebhookURL: srv.URL, Secret: "x"})
	cur := cursor{Offset: 1 << 30} // way past EOF, as if the file was truncated under us
	if !s.drainFile(path, &cur) {
		t.Fatal("drainFile should recover from a past-EOF cursor")
	}
	if got := received.Load(); got != 2 {
		t.Errorf("shipped %d events, want 2 (restarted from offset 0)", got)
	}
}

// When the cursor's generation is gone entirely (multi-rotation skew), the
// current .1 must ship from offset 0 — applying the stale offset to the wrong
// file would silently skip its head.
func TestShipperGenerationSkewShipsDotOneFromStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.log")
	writeJSONLines(t, path+".1", 5)
	writeJSONLines(t, path, 1)

	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []json.RawMessage `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		received.Add(int64(len(body.Events)))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewShipper(ShipperConfig{LogPath: path, CursorPath: filepath.Join(dir, "c"), WebhookURL: srv.URL, Secret: "x"})
	// A cursor whose inode matches neither the live file nor .1, with a large
	// offset — the generation it tracked has been removed.
	stale := cursor{Inode: 999999999, Offset: 1 << 20}
	s.drain(stale)

	if got := received.Load(); got != 6 {
		t.Errorf("shipped %d events, want 6 (all of .1 + live from 0)", got)
	}
}

// FuzzReadBatch: the shipper reads a file that a crash can leave in any state
// (torn lines, garbage, no newline). readBatch must never panic; every line it
// returns is fed to json.Valid downstream, so exercise that too.
func FuzzReadBatch(f *testing.F) {
	f.Add([]byte("{\"a\":1}\n{\"b\":2}\n"))
	f.Add([]byte("no trailing newline"))
	f.Add([]byte("{torn\n{\"ok\":1}\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "f.log")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Skip()
		}
		lines, consumed, err := readBatch(p, 0, 0, 500)
		if err != nil {
			return
		}
		if consumed < 0 {
			t.Fatalf("negative consumed: %d", consumed)
		}
		for _, l := range lines {
			_ = json.Valid(l) // must not panic
		}
	})
}

// FuzzRepairTail: the sink runs repairTail on a file a crash left in any state;
// it must never panic and must leave the file openable.
func FuzzRepairTail(f *testing.F) {
	f.Add([]byte("complete\n"))
	f.Add([]byte("partial"))
	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "f.log")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Skip()
		}
		if _, err := repairTail(p); err != nil {
			return
		}
		// After repair the file must still be readable and end at a line boundary.
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("file unreadable after repair: %v", err)
		}
		if len(b) > 0 && b[len(b)-1] != '\n' {
			t.Fatalf("repairTail left a non-newline tail: %q", b)
		}
	})
}
