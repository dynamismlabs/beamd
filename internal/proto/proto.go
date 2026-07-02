// Package proto defines the beam control protocol — NDJSON messages
// exchanged over a single dedicated yamux stream (PRD §8). The package
// is dependency-free on the rest of the codebase by design.
package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

// ProtoVersion is the current control protocol version. Carried in
// hello / hello_ok so future changes don't need a flag-day.
const ProtoVersion = 1

// Message type discriminators.
const (
	TypeHello      = "hello"
	TypeHelloOK    = "hello_ok"
	TypeRegister   = "register"
	TypeRegistered = "registered"
	TypeUnregister = "unregister"
	TypeHeartbeat  = "heartbeat"
	TypeError      = "error"
)

// Error codes carried in Error.Code.
const (
	CodeBadToken    = "bad_token"
	CodeBadHello    = "bad_hello"
	CodeBadMessage  = "bad_message"
	CodeUnknownMsg  = "unknown_message"
	CodeInvalidName = "invalid_name"
	CodeNameTaken   = "name_taken"
	CodeOverLimit   = "over_limit"
	CodeShutdown    = "shutdown"
	CodeInternal    = "internal"
)

type Hello struct {
	Type          string `json:"type"` // "hello"
	Token         string `json:"token"`
	Scope         string `json:"scope,omitempty"` // requested org/scope; "" = the credential's default
	ClientVersion string `json:"client_version,omitempty"`
	ProtoVersion  int    `json:"proto_version"`
}

type HelloOK struct {
	Type         string `json:"type"` // "hello_ok"
	Slug         string `json:"slug"`
	BaseDomain   string `json:"base_domain"`
	Shape        string `json:"shape,omitempty"` // edge URL shape (hyphen|subdomain|flat); "" from older edges → hyphen
	ProtoVersion int    `json:"proto_version"`
}

type Register struct {
	Type string `json:"type"`           // "register"
	Name string `json:"name,omitempty"` // optional; server derives from Port if empty
	Port int    `json:"port"`
}

type Registered struct {
	Type string `json:"type"` // "registered"
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Unregister struct {
	Type string `json:"type"` // "unregister"
	Name string `json:"name"`
}

type Heartbeat struct {
	Type string `json:"type"` // "heartbeat"
}

type Error struct {
	Type string `json:"type"` // "error"
	Code string `json:"code"`
	// Name echoes the register this error is about, so the client can drop a
	// late error meant for an already-abandoned register instead of
	// misrouting it onto the next one. Empty for connection-scoped errors
	// (bad_hello, shutdown, …) and from older edges — the client treats an
	// empty Name as "deliver to whatever register is waiting" (prior behavior).
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// Write encodes msg as a single JSON object followed by '\n'.
func Write(w io.Writer, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// MaxLineBytes caps a single control message. Real messages are well under a
// kilobyte; the cap exists so a peer streaming bytes with no newline — before
// authenticating, on the edge side — can't grow the read buffer without bound.
const MaxLineBytes = 1 << 20 // 1 MiB

// ErrLineTooLong is returned when a control line exceeds MaxLineBytes. The
// stream is desynced at that point; callers must tear the session down.
var ErrLineTooLong = errors.New("proto: control line exceeds max length")

// Read reads one NDJSON message from r and returns its type discriminator
// plus the raw line. Callers json.Unmarshal raw into the matching struct.
// Returns io.EOF when the stream is cleanly closed.
func Read(r *bufio.Reader) (typ string, raw []byte, err error) {
	line, err := readLineCapped(r, MaxLineBytes)
	if err != nil {
		return "", nil, err
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return "", line, err
	}
	return env.Type, line, nil
}

// readLineCapped reads through '\n' like bufio.Reader.ReadBytes, but fails
// with ErrLineTooLong once the accumulated line passes max instead of growing
// without bound.
func readLineCapped(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > max {
			return nil, ErrLineTooLong
		}
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}
