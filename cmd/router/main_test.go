package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

type noticeFrontend struct {
	core.Frontend
	t     *testing.T
	calls int
}

func (f *noticeFrontend) Send(ctx context.Context, chat string, e core.Envelope) error {
	f.calls++
	deadline, ok := ctx.Deadline()
	if ctx.Err() != nil || !ok || time.Until(deadline) > 5*time.Second {
		f.t.Error("notice needs a live, bounded context")
	}
	if chat != "42" || !e.System || e.Parts[0].Text != "Server shutting down" {
		f.t.Errorf("unexpected notice: %+v", e)
	}
	return errors.New("Telegram unavailable")
}
func TestLifecycleNoticeFailureIsNonfatal(t *testing.T) {
	f := &noticeFrontend{t: t}
	notifyLifecycle(f, 42, "Server shutting down")
	if f.calls != 1 {
		t.Fatal("notification not attempted")
	}
}
