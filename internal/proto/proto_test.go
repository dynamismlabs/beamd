package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
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
