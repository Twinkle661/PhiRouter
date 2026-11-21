// Package mcp implements a stateless Streamable HTTP tool server. It owns the
// protocol (authentication, JSON-RPC, routing headers); the tools themselves
// are supplied by the composition root.
package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Tool is one MCP tool. Implementations live outside this package and need
// not import it: the interface is satisfied structurally.
type Tool interface {
	Name() string
	Description() string
	// InputSchema is the JSON Schema of the tool's arguments.
	InputSchema() map[string]any
	// Call runs the tool for the backend identified by route. The returned
	// text is shown to the agent. An error is reported to the agent as a tool
	// failure, unless it reports InvalidParams() == true, in which case it is
	// returned as a JSON-RPC invalid-params error.
	Call(ctx context.Context, route string, args json.RawMessage) (string, error)
}

type Options struct {
	Token, Route string
	Routes       map[string]bool
	Tools        []Tool
}
type request struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func New(o Options) http.Handler {
	tools := map[string]Tool{}
	list := make([]any, 0, len(o.Tools))
	names := make([]string, 0, len(o.Tools))
	for _, t := range o.Tools {
		tools[t.Name()] = t
		list = append(list, map[string]any{"name": t.Name(), "description": t.Description(), "inputSchema": t.InputSchema()})
		names = append(names, t.Name())
	}
	instructions := "Deliver content to the user's chat with these tools: " + strings.Join(names, ", ") + ". Files must be inside the shared media directory."
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if o.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+o.Token)) != 1 {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Forbidden origin", 403)
				return
			}
		}
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			http.Error(w, "Method not allowed", 405)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "Expected application/json", 415)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req request
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			rpc(w, nil, nil, -32700, "Parse error")
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF || req.Version != "2.0" || req.Method == "" {
			rpc(w, req.ID, nil, -32600, "Invalid request")
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		switch req.Method {
		case "initialize":
			var params struct{ ProtocolVersion string }
			_ = json.Unmarshal(req.Params, &params)
			version := "2025-03-26"
			if params.ProtocolVersion == "2025-06-18" || params.ProtocolVersion == "2025-11-25" {
				version = params.ProtocolVersion
			}
			rpc(w, req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "PhiRouter", "version": "0.1.0"}, "instructions": instructions}, 0, "")
		case "ping":
			rpc(w, req.ID, map[string]any{}, 0, "")
		case "tools/list":
			rpc(w, req.ID, map[string]any{"tools": list}, 0, "")
		case "tools/call":
			var params struct {
				Name      string
				Arguments json.RawMessage
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				rpc(w, req.ID, nil, -32602, "Expected a tool name and arguments")
				return
			}
			tool, ok := tools[params.Name]
			if !ok {
				rpc(w, req.ID, nil, -32602, "Unknown tool")
				return
			}
			route := o.Route
			if len(o.Routes) > 0 {
				route = r.Header.Get(core.RouteHeader)
				if !o.Routes[route] {
					rpc(w, req.ID, nil, -32602, "Unknown backend return route")
					return
				}
			}
			text, err := tool.Call(r.Context(), route, params.Arguments)
			var invalid interface{ InvalidParams() bool }
			if errors.As(err, &invalid) && invalid.InvalidParams() {
				rpc(w, req.ID, nil, -32602, err.Error())
				return
			}
			if err != nil {
				text = err.Error()
			}
			rpc(w, req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": err != nil}, 0, "")
		default:
			rpc(w, req.ID, nil, -32601, "Method not found")
		}
	})
}
func rpc(w http.ResponseWriter, id json.RawMessage, result any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if code != 0 {
		response["error"] = map[string]any{"code": code, "message": message}
	} else {
		response["result"] = result
	}
	_ = json.NewEncoder(w).Encode(response)
}
