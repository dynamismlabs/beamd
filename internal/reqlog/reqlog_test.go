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
		fs.Record(RequestEvent{RequestID: NewID(), Host: "api-acme.beamd.sh", Method: "GET", Status: 200, Outcome: OutcomeOK})
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
		if ev.Host != "api-acme.beamd.sh" {
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
