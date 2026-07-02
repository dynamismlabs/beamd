package reqlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// FileSink is the always-on local sink: a single writer goroutine drains a
// buffered channel and appends one JSON line per event to <path>. Record is a
// non-blocking channel send — on a full channel it DROPS and bumps a counter, so
// the proxy hot path never blocks (request-events-spec §4.2). The file is the
// durable buffer the hosted shipper tails.
type FileSink struct {
	path     string
	maxBytes int64
	fsync    time.Duration
	tailed   bool

	ch      chan RequestEvent
	dropped atomic.Int64

	wg   sync.WaitGroup
	stop chan struct{}
}

// FileSinkConfig configures a FileSink. Zero values get sensible defaults.
type FileSinkConfig struct {
	Path     string        // append target; required
	MaxBytes int64         // rotate at this size (default 128 MiB)
	Fsync    time.Duration // batch-fsync interval (default 250ms)
	Buffer   int           // channel depth (default 4096)
	// Tailed means a Shipper is consuming the rotated <path>.1. When set,
	// rotation will NOT clobber an existing <path>.1 (the shipper hasn't finished
	// it yet) — it defers, letting the live file grow past MaxBytes until the
	// shipper drains and removes .1. Untailed (local-only) logs just roll: losing
	// the oldest rotated chunk is fine since nothing ships it.
	Tailed bool
}

// NewFileSink starts the writer goroutine. Call Close to drain + stop.
func NewFileSink(cfg FileSinkConfig) (*FileSink, error) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 128 << 20
	}
	if cfg.Fsync <= 0 {
		cfg.Fsync = 250 * time.Millisecond
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 4096
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, err
	}
	fs := &FileSink{
		path:     cfg.Path,
		maxBytes: cfg.MaxBytes,
		fsync:    cfg.Fsync,
		tailed:   cfg.Tailed,
		ch:       make(chan RequestEvent, cfg.Buffer),
		stop:     make(chan struct{}),
	}
	// Open synchronously so an unwritable path/permission error surfaces AT
	// STARTUP (the caller exits) instead of being swallowed by the writer
	// goroutine, which would otherwise silently drop every event.
	f, w, size, err := fs.openOrErr()
	if err != nil {
		return nil, err
	}
	fs.wg.Add(1)
	go fs.run(f, w, size)
	return fs, nil
}

// Record implements Sink — non-blocking; drops (observably) under backpressure.
func (fs *FileSink) Record(ev RequestEvent) {
	select {
	case fs.ch <- ev:
	default:
		fs.dropped.Add(1)
	}
}

// Dropped is how many events were dropped — under channel backpressure OR because
// the sink couldn't write them (a reopen/write failure). Exposed at /metrics as
// beam_requests_dropped_total so loss is always observable, never silent.
func (fs *FileSink) Dropped() int64 { return fs.dropped.Load() }

// Close stops the writer after draining what's already queued.
func (fs *FileSink) Close() {
	close(fs.stop)
	fs.wg.Wait()
}

func (fs *FileSink) run(f *os.File, w *bufio.Writer, size int64) {
	defer fs.wg.Done()

	defer func() {
		if w != nil {
			_ = w.Flush()
		}
		if f != nil {
			_ = f.Sync()
			_ = f.Close()
		}
	}()

	ticker := time.NewTicker(fs.fsync)
	defer ticker.Stop()
	dirty := false
	rotateBlocked := false // tailed + .1 still unshipped: deferring rotation
	pending := 0           // events written into w since its last successful flush
	writeFailed := false   // a Write errored: w is latched, replace it next tick

	// reopen abandons a writer whose error state has latched (bufio never
	// recovers after a failed Write/Flush) and starts over with fresh handles.
	// Without this, one disk-full moment drops every subsequent event until
	// process restart — even after space is freed, because `size` stops
	// advancing and rotation (the only other reopen path) never triggers. The
	// abandoned buffer's events are unrecoverable; count them so the loss is
	// observable per the sink's contract.
	reopen := func() {
		fs.dropped.Add(int64(pending))
		pending = 0
		dirty = false
		writeFailed = false
		if f != nil {
			_ = f.Close()
		}
		f, w, size = fs.open()
	}

	writeOne := func(ev RequestEvent) {
		if f == nil || w == nil {
			// No open file (a reopen failed) — the event is lost. Count it
			// so the loss shows up at /metrics rather than vanishing silently.
			fs.dropped.Add(1)
			return
		}
		line, err := json.Marshal(ev)
		if err != nil {
			fs.dropped.Add(1)
			return
		}
		line = append(line, '\n')
		n, err := w.Write(line)
		size += int64(n)
		dirty = true
		if err != nil {
			// Partial/failed write (e.g. disk full) — observable, not silent.
			// Recovery happens on the next tick, not per-event, so sustained
			// failure doesn't open/close the file thousands of times a second.
			fs.dropped.Add(1)
			writeFailed = true
			slog.Error("reqlog: write", "path", fs.path, "err", err.Error())
			return
		}
		pending++
		if size >= fs.maxBytes {
			// When a shipper is tailing, never clobber an unshipped <path>.1: if it
			// still exists the shipper hasn't finished it, so defer rotation and keep
			// appending (the live file grows past maxBytes until .1 is drained +
			// removed). This is what makes the buffer lossless across a slow/down
			// control plane that would otherwise let a second rotation overwrite the
			// first's still-unshipped tail.
			if fs.tailed {
				if _, err := os.Stat(fs.path + ".1"); err == nil {
					if !rotateBlocked {
						slog.Warn("reqlog: rotation deferred; shipper behind, log growing past max", "path", fs.path)
						rotateBlocked = true
					}
					return
				}
			}
			rotateBlocked = false
			f, w, size = fs.rotate(f, w)
			pending = 0
		}
	}

	flushTick := func() {
		switch {
		case writeFailed:
			reopen()
		case f == nil || w == nil:
			// An earlier reopen failed — keep retrying so the sink heals once
			// the path is writable again.
			f, w, size = fs.open()
		case dirty:
			if err := w.Flush(); err != nil {
				slog.Error("reqlog: flush", "path", fs.path, "err", err.Error())
				reopen()
				return
			}
			_ = f.Sync()
			pending = 0
			dirty = false
		}
	}

	for {
		select {
		case ev := <-fs.ch:
			writeOne(ev)
		case <-ticker.C:
			flushTick()
		case <-fs.stop:
			// Drain whatever's queued, then return.
			for {
				select {
				case ev := <-fs.ch:
					writeOne(ev)
				default:
					return
				}
			}
		}
	}
}

// openOrErr opens the append target, returning an error on failure. Used at
// startup so a bad/unwritable path is fatal rather than silently dropping events.
func (fs *FileSink) openOrErr() (*os.File, *bufio.Writer, int64, error) {
	// A hard kill can leave the file ending mid-line (bufio flushes on buffer
	// pressure, not line boundaries). Appending after that fragment would merge
	// it with the next event into one corrupt line, so truncate it first. Best
	// effort: the shipper also skips non-JSON lines, but repairing here keeps
	// the on-disk log clean for local consumers too.
	if repaired, err := repairTail(fs.path); err != nil {
		slog.Warn("reqlog: tail repair failed", "path", fs.path, "err", err.Error())
	} else if repaired {
		fs.dropped.Add(1) // the torn record is gone; keep loss observable
		slog.Warn("reqlog: truncated crash-torn partial line", "path", fs.path)
	}
	f, err := os.OpenFile(fs.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("reqlog: open %s: %w", fs.path, err)
	}
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	return f, bufio.NewWriter(f), size, nil
}

// repairTail truncates a trailing partial line (no final '\n') back to the
// last complete line. Reports whether anything was removed.
func repairTail(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := st.Size()
	if size == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return false, err
	}
	if last[0] == '\n' {
		return false, nil
	}
	// Scan backwards in chunks for the last newline; truncate just after it.
	const chunk = 64 * 1024
	end := size
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil {
			return false, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return true, f.Truncate(start + int64(i) + 1)
		}
		end = start
	}
	return true, f.Truncate(0) // no newline at all: the whole file is one torn line
}

// open is the best-effort reopen used after rotation: it logs and returns nil
// handles on failure (writeOne then counts the resulting drops, so loss stays
// observable).
func (fs *FileSink) open() (*os.File, *bufio.Writer, int64) {
	f, w, size, err := fs.openOrErr()
	if err != nil {
		slog.Error("reqlog: reopen log", "err", err.Error())
		return nil, nil, 0
	}
	return f, w, size
}

// rotate flushes + renames the current file to "<path>.1" and opens a fresh one.
// The shipper follows rotation via the inode in its cursor. The caller only
// invokes rotate when it's safe to (re)place <path>.1 — i.e. untailed (local
// roll, clobber is fine) or tailed with no .1 present — so the rename never
// overwrites an unshipped rotated file.
func (fs *FileSink) rotate(f *os.File, w *bufio.Writer) (*os.File, *bufio.Writer, int64) {
	_ = w.Flush()
	_ = f.Sync()
	_ = f.Close()
	if err := os.Rename(fs.path, fs.path+".1"); err != nil {
		slog.Error("reqlog: rotate", "err", err.Error())
	}
	f2, w2, size := fs.open()
	return f2, w2, size
}
