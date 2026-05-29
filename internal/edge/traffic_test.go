package edge

import (
	"net"
	"path/filepath"
	"sync"
	"testing"
)

func TestTrafficStore_RecordsAndRollsUpBySlug(t *testing.T) {
	s := newTrafficStore("")
	s.RecordTraffic("turing", "api", 10, 100)
	s.RecordTraffic("turing", "api", 5, 50)
	s.RecordTraffic("turing", "web", 1, 20)
	s.RecordTraffic("hopper", "api", 2, 200)
	s.RecordTraffic("", "ignored", 9, 9) // empty slug → dropped

	out := s.bytesOutBySlug()
	if out["turing"] != 170 { // 100 + 50 + 20
		t.Errorf("turing egress = %d, want 170", out["turing"])
	}
	if out["hopper"] != 200 {
		t.Errorf("hopper egress = %d, want 200", out["hopper"])
	}
	if _, ok := out[""]; ok {
		t.Errorf("empty slug should be dropped, got entry")
	}
}

func TestTrafficStore_PersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bandwidth.json")

	s := newTrafficStore(path)
	s.RecordTraffic("turing", "api", 10, 100)
	s.RecordTraffic("turing", "web", 3, 30)
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// A new store at the same path resumes the persisted totals.
	s2 := newTrafficStore(path)
	if out := s2.bytesOutBySlug(); out["turing"] != 130 {
		t.Fatalf("after reload, turing egress = %d, want 130", out["turing"])
	}
	// And new traffic accumulates on top of the loaded totals.
	s2.RecordTraffic("turing", "api", 0, 70)
	if out := s2.bytesOutBySlug(); out["turing"] != 200 {
		t.Errorf("turing egress = %d, want 200 after continued recording", out["turing"])
	}
}

func TestTrafficStore_BlankPathIsInMemoryOnly(t *testing.T) {
	s := newTrafficStore("")
	s.RecordTraffic("turing", "api", 1, 1)
	if err := s.Flush(); err != nil {
		t.Errorf("Flush with blank path should be a no-op, got %v", err)
	}
}

func TestCountingConn_TalliesBothDirections(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()

	var gotIn, gotOut int64
	var wg sync.WaitGroup
	wg.Add(1)
	cc := &countingConn{Conn: c1, onClose: func(in, out int64) {
		gotIn, gotOut = in, out
		wg.Done()
	}}

	// Peer drains the request (counted as `in`) then sends a response
	// (counted as `out` when we read it).
	go func() {
		buf := make([]byte, 64)
		_, _ = c2.Read(buf)
		_, _ = c2.Write([]byte("response-bytes")) // 14 bytes
	}()

	if _, err := cc.Write([]byte("req")); err != nil { // 3 bytes
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	if n, _ := cc.Read(buf); n != 14 {
		t.Fatalf("read %d bytes, want 14", n)
	}
	_ = cc.Close()
	wg.Wait()

	if gotIn != 3 {
		t.Errorf("bytesIn = %d, want 3 (request)", gotIn)
	}
	if gotOut != 14 {
		t.Errorf("bytesOut = %d, want 14 (response)", gotOut)
	}
}
