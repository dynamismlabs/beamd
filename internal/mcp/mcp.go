// Package mcp implements a Model Context Protocol stdio server that
// wraps the beamd agent's local HTTP API. Tools exposed (each maps to a
// `beamd` CLI command):
//   - expose_port(port, name?)  → `beamd open`
//   - remove_tunnel(name)       → `beamd close`
//   - list_tunnels()            → `beamd list`
//
// This is the primary integration surface for AI agents (PRD §10).
package mcp

import (
	"context"
	"encoding/json"
	"io"

	"github.com/dynamismlabs/beamd/internal/daemon"
)

const ProtocolVersion = "2024-11-05"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type Server struct {
	LC          *daemon.LocalClient
	In          io.Reader
	Out         io.Writer
	ServerName  string
	ServerVer   string
}

func New(lc *daemon.LocalClient, in io.Reader, out io.Writer, serverName, serverVer string) *Server {
	return &Server{LC: lc, In: in, Out: out, ServerName: serverName, ServerVer: serverVer}
}

// Run reads JSON-RPC requests until EOF or error and writes responses
// back. Returns nil on clean EOF; the caller should treat io.EOF as
// success.
func (s *Server) Run(ctx context.Context) error {
	dec := json.NewDecoder(s.In)
	enc := json.NewEncoder(s.Out)

	for {
		var req jsonRPCRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		isNotification := len(req.ID) == 0 || string(req.ID) == "null"

		result, rerr := s.dispatch(ctx, req.Method, req.Params)
		if isNotification {
			continue
		}
		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *jsonRPCError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.ServerName, "version": s.ServerVer},
		}, nil

	case "notifications/initialized", "initialized":
		return nil, nil

	case "tools/list":
		return map[string]any{"tools": tools()}, nil

	case "tools/call":
		return s.callTool(ctx, params)

	case "ping":
		return map[string]any{}, nil

	default:
		return nil, &jsonRPCError{Code: -32601, Message: "method not found: " + method}
	}
}

func tools() []map[string]any {
	return []map[string]any{
		{
			"name":        "expose_port",
			"description": "Expose a locally-running app on the given port as a public HTTPS URL via Beamd, and return the URL synchronously. Equivalent to the `beamd open <port> --as <name>` CLI command.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"port": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     65535,
						"description": "Local TCP port the app is listening on (loopback).",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Optional subdomain label (RFC 1123). Defaults to the port number.",
					},
				},
				"required": []string{"port"},
			},
		},
		{
			"name":        "remove_tunnel",
			"description": "Remove (tear down) a tunnel by name. Equivalent to the `beamd close <name>` CLI command.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "list_tunnels",
			"description": "List the currently exposed tunnels (name, port, url, healthy). Equivalent to the `beamd list` CLI command.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *jsonRPCError) {
	var p callToolParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &jsonRPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	switch p.Name {
	case "expose_port":
		var a struct {
			Port int    `json:"port"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "invalid arguments: " + err.Error()}
		}
		if a.Port < 1 || a.Port > 65535 {
			return toolError("port must be 1..65535"), nil
		}
		resp, err := s.LC.Open(ctx, a.Port, a.Name)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(resp.URL), nil

	case "remove_tunnel":
		var a struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "invalid arguments: " + err.Error()}
		}
		if a.Name == "" {
			return toolError("name is required"), nil
		}
		if _, err := s.LC.Close(ctx, a.Name); err != nil {
			return toolError(err.Error()), nil
		}
		return toolText("ok"), nil

	case "list_tunnels":
		items, err := s.LC.List(ctx)
		if err != nil {
			return toolError(err.Error()), nil
		}
		body, _ := json.Marshal(items)
		return toolText(string(body)), nil

	default:
		return nil, &jsonRPCError{Code: -32601, Message: "unknown tool: " + p.Name}
	}
}

func toolText(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "error: " + msg},
		},
		"isError": true,
	}
}
