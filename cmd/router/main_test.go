package main

import (
	"context"
	"errors"
	"github.com/Twinkle661/PhiRouter/adapters/frontend/telegram"
	"github.com/Twinkle661/PhiRouter/adapters/tts/openaispeech"
	"github.com/Twinkle661/PhiRouter/internal/config"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"github.com/Twinkle661/PhiRouter/internal/media"
	"github.com/Twinkle661/PhiRouter/internal/router"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	body := "frontends:\n  tg:\n    type: telegram\n    owner: \"1\"\n    options:\n      token: t\nroutes:\n  - from: \"tg:*\"\n    to: default\napi:\n  token: a\nmcp:\n  token: b\n  tools:\n    send_voice:\n      enabled: true\n      provider:\n        options:\n          model: openai/gpt-4o-mini-tts-2025-12-15\n          voice: nova\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__OPTIONS__API_KEY", "key")
	c, err := config.Load(path, filepath.Join(dir, ".env"), registries())
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

func TestBuildBackendsFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "frontends:\n  tg:\n    type: telegram\n    owner: \"1\"\n    options:\n      token: t\nroutes:\n  - from: \"tg:*\"\n    to: zero\n  - from: \"tg:2\"\n    to: coder\napi:\n  token: a\nmcp:\n  token: b\nstate:\n  dir: " + filepath.Join(dir, "state") + "\nbackends:\n  zero:\n    type: pi\n    options:\n      agent_dir: " + filepath.Join(dir, "pi") + "\n  coder:\n    type: acp\n    busy_policy: interrupt\n    options:\n      command: [opencode, acp]\n      cwd: /work\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path, filepath.Join(dir, ".env"), registries())
	if err != nil {
		t.Fatal(err)
	}
	backends, policies, err := buildBackends(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 2 || backends["zero"].Capabilities() == backends["coder"].Capabilities() {
		t.Fatalf("backends: %+v", backends)
	}
	if policies["coder"] != router.BusyInterrupt || policies["zero"] != router.BusyQueue {
		t.Fatalf("policies: %+v", policies)
	}
	if _, err = os.Stat(filepath.Join(dir, "pi", "mcp.json")); err != nil {
		t.Fatal("pi tool server not registered:", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("ROUTER_FRONTENDS__TG__OPTIONS__TOKEN", "1:t")
	t.Setenv("ROUTER_FRONTENDS__TG__OWNER", "1")
	t.Setenv("ROUTER_API__TOKEN", "a")
	t.Setenv("ROUTER_MCP__TOKEN", "b")
	c, err := config.Load(filepath.Join("..", "..", "config.example.yaml"), filepath.Join(t.TempDir(), ".env"), registries())
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Backends["default"]; b.Type != "pi" || len(c.Backends) != 1 || c.Routes[0].To != "default" {
		t.Fatalf("%+v", c.Backends)
	}
}
