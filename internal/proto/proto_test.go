package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRoundTripHello(t *testing.T) {
	in := Hello{Type: TypeHello, Token: "abc", ClientVersion: "0.1", ProtoVersion: ProtoVersion}
	var buf bytes.Buffer
	if err := Write(&buf, &in); err != nil {
		t.Fatalf("write: %v", err)
	}

	typ, raw, err := Read(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != TypeHello {
		t.Errorf("type = %q, want %q", typ, TypeHello)
	}
	var out Hello
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestMultipleMessages(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, &Register{Type: TypeRegister, Name: "api", Port: 3001}); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, &Heartbeat{Type: TypeHeartbeat}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(&buf)

	if typ, _, _ := Read(br); typ != TypeRegister {
		t.Errorf("msg 1 type = %q", typ)
	}
	if typ, _, _ := Read(br); typ != TypeHeartbeat {
		t.Errorf("msg 2 type = %q", typ)
	}
	if _, _, err := Read(br); err != io.EOF {
		t.Errorf("expected EOF after two messages, got %v", err)
	}
}

func TestRead_BadJSON(t *testing.T) {
	br := bufio.NewReader(bytes.NewReader([]byte("not-json\n")))
	if _, _, err := Read(br); err == nil {
		t.Fatal("expected json error")
	}
}

func TestRead_LineTooLongRejected(t *testing.T) {
	// A "line" larger than MaxLineBytes with no newline must fail with
	// ErrLineTooLong instead of buffering indefinitely.
	huge := bytes.Repeat([]byte("a"), MaxLineBytes+1)
	_, _, err := Read(bufio.NewReader(bytes.NewReader(huge)))
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestRead_LongButLegalLineOK(t *testing.T) {
	// Lines bigger than bufio's internal buffer but under the cap still parse.
	msg := Hello{Type: TypeHello, Token: strings.Repeat("t", 8192), ProtoVersion: ProtoVersion}
	var buf bytes.Buffer
	if err := Write(&buf, msg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	typ, raw, err := Read(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if typ != TypeHello {
		t.Errorf("type = %q, want hello", typ)
	}
	var got Hello
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Token != msg.Token {
		t.Error("token did not round-trip")
	}
}

// FuzzRead: the hello read is the FIRST thing an unauthenticated peer reaches,
// so Read must never panic or hang on arbitrary bytes — only return a value or
// an error (incl. ErrLineTooLong).
func FuzzRead(f *testing.F) {
	f.Add([]byte(`{"type":"hello"}` + "\n"))
	f.Add([]byte("not json\n"))
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(`{"type":`))
	f.Add(append(bytes.Repeat([]byte("x"), MaxLineBytes+10), '\n'))
	f.Fuzz(func(t *testing.T, data []byte) {
		typ, raw, err := Read(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			return // any error is acceptable; must not panic/hang
		}
		// On success the raw line must not exceed the cap.
		if len(raw) > MaxLineBytes {
			t.Fatalf("returned a line of %d bytes, over cap %d", len(raw), MaxLineBytes)
		}
		_ = typ
	})
}
