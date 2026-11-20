package mcp

import (
	"PhiRouter/internal/core"
	"PhiRouter/internal/media"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPProtocolAndFiles(t *testing.T) {
	store, err := media.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	path := filepath.Join(store.Dir, "out", "report.txt")
	_ = os.WriteFile(path, []byte("hello"), 0600)
	sent := 0
	h := New(Options{Token: "mcp-secret", Route: "pi:default", Media: store, Send: func(_ context.Context, e core.Envelope) error {
		sent++
		if e.Parts[0].Path != path || e.Route != "pi:default" {
			t.Fatal(e)
		}
		return nil
	}})
	call := func(body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer mcp-secret")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var response map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if response["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatal(response)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "send_file", "arguments": map[string]string{"path": path}}})
	call(string(body))
	response = call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_file","arguments":{"path":"/etc/passwd"}}}`)
	if sent != 1 || response["result"].(map[string]any)["isError"] != true {
		t.Fatal(response, sent)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer mcp-secret")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
