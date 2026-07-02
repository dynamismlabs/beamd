package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two agents racing for the same socket must resolve to exactly one owner —
// the loser errors out instead of unlinking the winner's live socket (which
// used to orphan an agent that still held edge registrations).
func TestAcquireLock_SecondHolderRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock.lock")

	l1, err := acquireLock(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Close()

	if _, err := acquireLock(path, 300*time.Millisecond); err == nil {
		t.Fatal("second acquire should fail while the first holder is alive")
	}
}

// The lock must be waitable across a handoff: when the holder releases (as
// the old agent does during `beamd reload`), a waiting acquirer gets it.
func TestAcquireLock_HandoffAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock.lock")

	l1, err := acquireLock(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		l2, err := acquireLock(path, 3*time.Second)
		if err == nil {
			_ = l2.Close()
		}
		done <- err
	}()

	time.Sleep(200 * time.Millisecond)
	_ = l1.Close() // releasing the fd releases the flock

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting acquirer should win after release: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("waiting acquirer never acquired after release")
	}
	_ = os.Remove(path)
}
