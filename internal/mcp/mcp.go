// Package mcp implements a stateless Streamable HTTP tool server.
package mcp

import (
	"PhiRouter/internal/core"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Options struct {
	Token, Route string
	Routes       map[string]bool
	Media        core.MediaStore
	Send         core.Inject
}
type request struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func New(o Options) http.Handler {
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
			rpc(w, req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "PhiRouter", "version": "0.1.0"}, "instructions": "Write files inside the shared media directory, then call send_file to deliver them to the user."}, 0, "")
		case "ping":
			rpc(w, req.ID, map[string]any{}, 0, "")
		case "tools/list":
			rpc(w, req.ID, map[string]any{"tools": []any{map[string]any{"name": "send_file", "description": "Send a file from the shared media directory to the user immediately.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}, "caption": map[string]string{"type": "string"}}, "required": []string{"path"}, "additionalProperties": false}}}}, 0, "")
		case "tools/call":
			var params struct {
				Name      string
				Arguments struct{ Path, Caption string }
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.Name != "send_file" || params.Arguments.Path == "" {
				rpc(w, req.ID, nil, -32602, "Expected send_file with path and optional caption")
				return
			}
			target := o
			if len(o.Routes) > 0 {
				target.Route = r.Header.Get("X-PhiRouter-Route")
				if !o.Routes[target.Route] {
					rpc(w, req.ID, nil, -32602, "Unknown backend return route")
					return
				}
			}
			err := send(r.Context(), target, params.Arguments.Path, params.Arguments.Caption)
			text := "File sent."
			if err != nil {
				text = err.Error()
			}
			rpc(w, req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": err != nil}, 0, "")
		default:
			rpc(w, req.ID, nil, -32601, "Method not found")
		}
	})
}
func send(ctx context.Context, o Options, path, caption string) error {
	p, err := o.Media.Inspect(path)
	if err != nil {
		return fmt.Errorf("invalid attachment: %w", err)
	}
	p.Caption = caption
	return o.Send(ctx, core.Envelope{Route: o.Route, Parts: []core.Part{p}})
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
