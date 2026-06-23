package reqlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Shipper is the hosted-only tail-and-ship goroutine. It follows the append-only
// log from a persisted (inode, byte-offset) cursor, batches complete lines, and
// bulk-POSTs them to the control plane. The cursor advances ONLY on a 2xx, so a
// control-plane outage or edge restart replays the window losslessly (the file is
// the buffer; the control plane dedupes on request_id). It deletes the rotated
// <path>.1 once fully shipped; the sink defers its next rotation while .1 still
// exists, so even a multi-rotation backlog during a long outage never overwrites
// an unshipped file. Ships only complete, newline-terminated lines — a partial
// trailing line is held until its newline arrives, so a flush mid-write never
// ships a truncated JSON object.
type Shipper struct {
	logPath    string
	cursorPath string
	webhookURL string
	secret     string
	batchSize  int
	flush      time.Duration
	client     *http.Client
}

// ShipperConfig configures a Shipper.
type ShipperConfig struct {
	LogPath    string
	CursorPath string
	WebhookURL string
	Secret     string
	BatchSize  int           // default 500
	Flush      time.Duration // default 1s
}

// NewShipper builds a Shipper. WebhookURL must be set (hosted-only).
func NewShipper(cfg ShipperConfig) *Shipper {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.Flush <= 0 {
		cfg.Flush = time.Second
	}
	return &Shipper{
		logPath:    cfg.LogPath,
		cursorPath: cfg.CursorPath,
		webhookURL: cfg.WebhookURL,
		secret:     cfg.Secret,
		batchSize:  cfg.BatchSize,
		flush:      cfg.Flush,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

type cursor struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

// Run drains on a ticker until ctx is cancelled (then one final drain).
func (s *Shipper) Run(ctx context.Context) {
	cur := s.loadCursor()
	ticker := time.NewTicker(s.flush)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cur = s.drain(cur)
		case <-ctx.Done():
			s.drain(cur)
			return
		}
	}
}

// drain ships every complete line available, advancing + persisting the cursor
// after each successful batch. Stops on the first ship error (retry next tick).
func (s *Shipper) drain(cur cursor) cursor {
	// Follow rotation: if the file we were reading rotated away, finish the
	// rotated copy (<path>.1) from our offset FIRST. Only advance to the new file
	// once .1 is fully shipped — otherwise a control-plane outage mid-rotation
	// would lose .1's un-shipped tail (request-events-spec §4.3: do not advance
	// on non-2xx).
	ino, ok := inodeOf(s.logPath)
	if !ok {
		return cur // log not present yet
	}
	if cur.Inode != 0 && cur.Inode != ino {
		if !s.drainFile(s.logPath+".1", &cur) {
			return cur // .1 not fully shipped — keep the cursor, retry .1 next tick
		}
		// .1 is fully shipped: remove it so the sink can rotate again. The sink
		// defers rotation while .1 exists (lossless under a slow/down control
		// plane), so this Remove is what lets rotation resume after a backlog.
		_ = os.Remove(s.logPath + ".1")
		cur = cursor{Inode: ino, Offset: 0}
		s.saveCursor(cur)
	}
	if cur.Inode == 0 {
		cur.Inode = ino
	}
	s.drainFile(s.logPath, &cur)
	return cur
}

// drainFile ships complete lines from path starting at cur.Offset, advancing cur
// (and persisting) per successful batch. Returns true when the file is FULLY
// drained (nothing left to ship), false when it stopped on a ship error (so the
// caller must not advance past it). A missing/unreadable file counts as drained
// — for the rotated <path>.1 that means its leftover is already gone (best-effort
// loss); for the live file the next tick simply retries.
func (s *Shipper) drainFile(path string, cur *cursor) bool {
	for {
		lines, consumed, err := readBatch(path, cur.Offset, s.batchSize)
		if err != nil {
			return true
		}
		if len(lines) == 0 {
			return true
		}
		if !s.ship(lines) {
			return false // keep the cursor; retry the same window next tick
		}
		cur.Offset += consumed
		s.saveCursor(*cur)
	}
}

// readBatch reads up to max complete (newline-terminated) lines from path
// starting at offset. Returns the lines, the bytes consumed (whole lines only —
// a trailing partial is left for next time), and any open error.
func readBatch(path string, offset int64, max int) ([][]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	r := bufio.NewReader(f)
	var lines [][]byte
	var consumed int64
	for len(lines) < max {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // EOF or partial line (no trailing newline) — stop, don't consume it
		}
		consumed += int64(len(line))
		trimmed := bytes.TrimRight(line, "\n")
		if len(trimmed) > 0 {
			lines = append(lines, trimmed)
		}
	}
	return lines, consumed, nil
}

// ship POSTs a batch as {"events":[…]} and reports whether it was accepted (2xx).
func (s *Shipper) ship(lines [][]byte) bool {
	var buf bytes.Buffer
	buf.WriteString(`{"events":[`)
	for i, l := range lines {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(l)
	}
	buf.WriteString("]}")

	req, err := http.NewRequest(http.MethodPost, s.webhookURL, &buf)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.secret)
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Debug("reqlog: ship failed", "err", err.Error())
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !ok {
		slog.Warn("reqlog: ship rejected", "status", resp.StatusCode)
	}
	return ok
}

func (s *Shipper) loadCursor() cursor {
	var c cursor
	b, err := os.ReadFile(s.cursorPath)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

func (s *Shipper) saveCursor(c cursor) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := s.cursorPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.cursorPath) // atomic replace
}

// inodeOf returns the file's inode (for rotation-following). Works on linux +
// darwin (the edge's targets).
func inodeOf(path string) (uint64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(sys.Ino), true
}
