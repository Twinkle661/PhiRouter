package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Twinkle661/PhiRouter/adapters/frontend/telegram"
	"github.com/Twinkle661/PhiRouter/adapters/tts/openaispeech"
	"github.com/Twinkle661/PhiRouter/internal/config"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"github.com/Twinkle661/PhiRouter/internal/media"
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
	notifyLifecycle(f, "42", "Server shutting down")
	if f.calls != 1 {
		t.Fatal("notification not attempted")
	}
}

func TestBuildFrontendsRejectsUnsupportedIngress(t *testing.T) {
	opts := telegram.DefaultConfig()
	opts.Token = "x"
	f := config.Frontend{Type: "telegram", Owner: "42", Options: opts}
	c := config.Config{Frontends: map[string]config.Frontend{"tg": f}}
	built, owners, hooks, err := buildFrontends(c, nil)
	if err != nil || built["tg"] == nil || owners["tg"] != "42" || len(hooks) != 0 {
		t.Fatalf("telegram not built: %v", err)
	}
	f.Ingress.Path = "/hooks/tg"
	c.Frontends["tg"] = f
	if _, _, _, err = buildFrontends(c, nil); err == nil || !strings.Contains(err.Error(), "does not support ingress") {
		t.Fatalf("unsupported ingress accepted: %v", err)
	}
}

func TestSendVoiceWiring(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "frontends:\n  tg:\n    type: telegram\n    owner: \"1\"\n    options:\n      token: t\nroutes:\n  - from: \"tg:*\"\n    to: pi:default\napi:\n  token: a\nmcp:\n  token: b\n  tools:\n    send_voice:\n      enabled: true\n      provider:\n        options:\n          model: openai/gpt-4o-mini-tts-2025-12-15\n          voice: nova\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__OPTIONS__API_KEY", "key")
	c, err := config.Load(path, filepath.Join(dir, ".env"), frontendRegistry(), providerRegistry())
	if err != nil {
		t.Fatal(err)
	}
	o := c.MCP.Tools.SendVoice.Provider.Options.(*openaispeech.Options)
	if o.BaseURL != "https://openrouter.ai/api/v1" || o.ResponseFormat != "mp3" || o.StyleField != "instructions" || o.APIKey != "key" {
		t.Fatalf("defaults not applied: %+v", o)
	}
	store, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tools, err := buildTools(c, store, func(context.Context, core.Envelope) error { return nil })
	if err != nil || len(tools) != 2 || tools[1].Name() != "send_voice" {
		t.Fatal(tools, err)
	}
	c.MCP.Tools.SendVoice.Enabled = false
	if tools, _ = buildTools(c, store, nil); len(tools) != 1 {
		t.Fatal(tools)
	}
}
