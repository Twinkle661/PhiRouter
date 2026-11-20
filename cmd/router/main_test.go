package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPRegistrationPreservesEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"existing":{"command":"example"}},"custom":true}`), 0600)
	if err := registerMCP(path, "http://127.0.0.1:8081/mcp"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if servers["existing"] == nil || servers["phirouter"] == nil || cfg["custom"] != true {
		t.Fatal(string(raw))
	}
	if !strings.Contains(string(raw), "${ROUTER_MCP__TOKEN}") {
		t.Fatal("missing secret reference")
	}
	_ = os.WriteFile(path, []byte(`null`), 0600)
	if err := registerMCP(path, "endpoint"); err == nil {
		t.Fatal("null config accepted")
	}
}
