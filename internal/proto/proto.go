// Package proto defines the beam control protocol — NDJSON messages
// exchanged over a single dedicated yamux stream (PRD §8). The package
// is dependency-free on the rest of the codebase by design.
package proto

import (
	"bufio"
	"encoding/json"
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
	ClientVersion string `json:"client_version,omitempty"`
	ProtoVersion  int    `json:"proto_version"`
}

type HelloOK struct {
	Type         string `json:"type"` // "hello_ok"
	Slug         string `json:"slug"`
	BaseDomain   string `json:"base_domain"`
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
	Type    string `json:"type"` // "error"
	Code    string `json:"code"`
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

// Read reads one NDJSON message from r and returns its type discriminator
// plus the raw line. Callers json.Unmarshal raw into the matching struct.
// Returns io.EOF when the stream is cleanly closed.
func Read(r *bufio.Reader) (typ string, raw []byte, err error) {
	line, err := r.ReadBytes('\n')
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
