package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a deterministic ACP agent.
func TestAgentProcess(t *testing.T) {
	if os.Getenv("PHIROUTER_ACP_CHILD") != "1" {
		return
	}
	in := bufio.NewReader(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	logf, _ := os.OpenFile(os.Getenv("PHIROUTER_ACP_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	read := func() map[string]any {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		_, _ = logf.Write(line)
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			os.Exit(2)
		}
		return m
	}
	update := func(u map[string]any) {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": u}})
	}
	respond := func(id any, result any) { _ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	options := func(current string) []any {
		return []any{map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": current, "options": []any{map[string]any{"group": "g", "name": "G", "options": []any{map[string]any{"value": "a", "name": "A"}, map[string]any{"value": "b", "name": "B"}}}}}}
	}
	text := func(m map[string]any) string {
		blocks, _ := m["params"].(map[string]any)["prompt"].([]any)
		first, _ := blocks[0].(map[string]any)
		s, _ := first["text"].(string)
		return s
	}
	for {
		m := read()
		id, method := m["id"], m["method"]
		switch method {
		case "initialize":
			respond(id, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": true}, "mcpCapabilities": map[string]any{"http": true}}})
		case "session/new":
			respond(id, map[string]any{"sessionId": "s1", "configOptions": options("a")})
		case "session/load":
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "replayed history"}})
			respond(id, map[string]any{"configOptions": options("a")})
		case "session/set_config_option":
			respond(id, map[string]any{"configOptions": options(m["params"].(map[string]any)["value"].(string))})
		case "session/prompt":
			switch text(m) {
			case "hello":
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Run ls", "status": "pending", "rawInput": map[string]any{"command": "ls"}})
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "in_progress"})
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "title": "Run ls", "status": "failed"})
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "fs1", "method": "fs/read_text_file", "params": map[string]any{"path": "/etc/passwd"}})
				read() // the client's error answer
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "hi"}})
				respond(id, map[string]any{"stopReason": "end_turn"})
			case "permission":
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "p1", "method": "session/request_permission", "params": map[string]any{"sessionId": "s1", "toolCall": map[string]any{"toolCallId": "t2", "title": "Edit file", "rawInput": map[string]any{"path": "main.go"}}, "options": []any{map[string]any{"optionId": "yes", "name": "Allow once", "kind": "allow_once"}, map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}}})
				answer := read()
				for answer["id"] != "p1" { // session/cancel may come first
					answer = read()
				}
				outcome := answer["result"].(map[string]any)["outcome"].(map[string]any)
				choice, _ := outcome["optionId"].(string)
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "outcome:" + outcome["outcome"].(string) + ":" + choice}})
				respond(id, map[string]any{"stopReason": "end_turn"})
			case "wait":
				for {
					if next := read(); next["method"] == "session/cancel" {
						break
					}
				}
				respond(id, map[string]any{"stopReason": "cancelled"})
			default:
				respond(id, map[string]any{"stopReason": "end_turn"})
			}
		}
	}
}

type harness struct {
	t      *testing.T
	b      *Backend
	events chan core.Event
	log    string
	ctx    context.Context
}

func run(t *testing.T, o Options) *harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &harness{t: t, events: make(chan core.Event, 256), log: filepath.Join(dir, "agent.jsonl")}
	o.Command = []string{exe, "-test.run=^TestAgentProcess$"}
	o.Env = []string{"PHIROUTER_ACP_CHILD=1", "PHIROUTER_ACP_LOG=" + h.log}
	o.Cwd = dir
	o.StatePath = filepath.Join(dir, "state", "session.json")
	o.Retry = 10 * time.Millisecond
	h.b = New(o)
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	done := make(chan error, 1)
	go func() { done <- h.b.Run(ctx, func(e core.Event) { h.events <- e }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("backend did not stop")
		}
	})
	h.next(core.EventReady)
	return h
}

// next returns the next event of a kind, skipping extensions only.
func (h *harness) next(kind core.EventKind) core.Event {
	h.t.Helper()
	for {
		select {
		case e := <-h.events:
			if e.Kind == kind {
				return e
			}
			if e.Kind != core.EventExtension {
				h.t.Fatalf("want kind %d, got %+v", kind, e)
			}
		case <-time.After(5 * time.Second):
			h.t.Fatalf("missing event kind %d", kind)
		}
	}
}
func (h *harness) prompt(text string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 2*time.Second)
	defer cancel()
	if _, err := h.b.Prompt(ctx, core.Envelope{Channel: true, Parts: []core.Part{{Type: core.PartText, Text: text}}}, core.DeliveryPrompt); err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) wire() string {
	raw, _ := os.ReadFile(h.log)
	return string(raw)
}

func TestSessionTurnsToolsAndResume(t *testing.T) {
	h := run(t, Options{Tools: core.ToolServer{URL: "http://127.0.0.1:8081/mcp", Token: "secret", Route: "coder"}})
	if wire := h.wire(); !strings.Contains(wire, `"mcpServers":[{"headers":[{"name":"Authorization","value":"Bearer secret"},{"name":"X-PhiRouter-Route","value":"coder"}],"name":"phirouter","type":"http","url":"http://127.0.0.1:8081/mcp"}]`) {
		t.Fatalf("tool server not offered: %s", wire)
	}
	h.prompt("hello")
	start := h.next(core.EventToolCall)
	if start.Tool.Status != core.ToolInProgress || start.Tool.Title != "Run ls" || start.Tool.Detail != "ls" {
		t.Fatalf("start: %+v", start.Tool)
	}
	if end := h.next(core.EventToolCall); end.Tool.Status != core.ToolFailed {
		t.Fatalf("end: %+v", end.Tool)
	}
	if chunk := h.next(core.EventMessageChunk); chunk.Text != "hi" {
		t.Fatal(chunk.Text)
	}
	if end := h.next(core.EventTurnEnded); end.StopReason != core.StopEndTurn {
		t.Fatal(end)
	}
	if !strings.Contains(h.wire(), `{"error":{"code":-32601,"message":"method not supported by PhiRouter: fs/read_text_file"},"id":"fs1"`) {
		t.Fatalf("file system request not refused: %s", h.wire())
	}
	if _, err := h.b.Prompt(h.ctx, core.Envelope{Parts: []core.Part{{Type: core.PartText, Text: "x"}}}, core.DeliverySteer); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal(err)
	}
	models, err := h.b.Models(h.ctx)
	if err != nil || len(models) != 2 || models[1].ID != "b" {
		t.Fatalf("models %+v %v", models, err)
	}
	if err = h.b.SetModel(h.ctx, core.Model{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.b.Status(h.ctx); st.SessionID != "s1" || st.Model.ID != "b" {
		t.Fatalf("status %+v", st)
	}
	// Cancelling ends the turn with the agent's stop reason.
	h.prompt("wait")
	if err = h.b.Cancel(h.ctx); err != nil {
		t.Fatal(err)
	}
	if end := h.next(core.EventTurnEnded); end.StopReason != core.StopCancelled {
		t.Fatal(end)
	}
	// A restart resumes the saved session without replaying its history.
	if err = h.b.Restart(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.next(core.EventRestarting)
	h.next(core.EventReady)
	if !strings.Contains(h.wire(), `"method":"session/load","params":{"cwd":`) || !strings.Contains(h.wire(), `"sessionId":"s1"`) {
		t.Fatalf("session not loaded: %s", h.wire())
	}
	h.prompt("ping")
	h.next(core.EventTurnEnded) // a replayed chunk would fail next()
}

func TestPermissionsAskAndPolicies(t *testing.T) {
	h := run(t, Options{})
	h.prompt("permission")
	d := h.next(core.EventDialog).Dialog
	if len(d.Options) != 2 || d.Options[0].Kind != core.OptionAllowOnce || !d.Options[1].Kind.Rejects() || !strings.Contains(d.Message, "main.go") {
		t.Fatalf("dialog %+v", d)
	}
	if err := h.b.Respond(h.ctx, core.Reply{ID: d.ID, OptionID: "no"}); err != nil {
		t.Fatal(err)
	}
	if chunk := h.next(core.EventMessageChunk); chunk.Text != "outcome:selected:no" {
		t.Fatal(chunk.Text)
	}
	h.next(core.EventTurnEnded)
	// Cancelling answers an open request as cancelled.
	h.prompt("permission")
	h.next(core.EventDialog)
	if err := h.b.Cancel(h.ctx); err != nil {
		t.Fatal(err)
	}
	if chunk := h.next(core.EventMessageChunk); chunk.Text != "outcome:cancelled:" {
		t.Fatal(chunk.Text)
	}
	h.next(core.EventTurnEnded)

	auto := run(t, Options{Permission: PermissionAllow})
	auto.prompt("permission")
	if chunk := auto.next(core.EventMessageChunk); chunk.Text != "outcome:selected:yes" {
		t.Fatal(chunk.Text)
	}
}

type fakeMedia map[string]core.Part

func (m fakeMedia) Save(context.Context, string, string, io.Reader, int64) (core.Part, error) {
	return core.Part{}, errors.New("unused")
}
func (m fakeMedia) Inspect(path string) (core.Part, error) { return m[path], nil }
func (m fakeMedia) Open(path string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(make([]byte, m[path].Size))), nil
}

func TestContentBlocks(t *testing.T) {
	media := fakeMedia{"/m/small.png": {Path: "/m/small.png", MIME: "image/png", Size: 3}, "/m/big.png": {Path: "/m/big.png", MIME: "image/png", Size: 50}, "/m/doc.pdf": {Path: "/m/doc.pdf", MIME: "application/pdf", Size: 9}}
	b := New(Options{Media: media, InlineMax: 10})
	b.agent.PromptCapabilities.Image = true
	blocks, err := b.content(core.Envelope{Parts: []core.Part{{Type: core.PartText, Text: "see"}, {Type: core.PartImage, Path: "/m/small.png"}, {Type: core.PartImage, Path: "/m/big.png"}, {Type: core.PartFile, Path: "/m/doc.pdf", Caption: "report"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(blocks)
	for _, want := range []string{`{"text":"see","type":"text"}`, `"type":"image"`, `{"mimeType":"image/png","name":"big.png","size":50,"type":"resource_link","uri":"file:///m/big.png"}`, `"uri":"file:///m/doc.pdf"`, `{"text":"report","type":"text"}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	b.agent.PromptCapabilities.Image = false
	blocks, _ = b.content(core.Envelope{Parts: []core.Part{{Type: core.PartImage, Path: "/m/small.png"}}})
	if raw, _ = json.Marshal(blocks); strings.Contains(string(raw), `"type":"image"`) {
		t.Fatal("inlined image the agent cannot read")
	}
}

func TestConfigValidation(t *testing.T) {
	c := DefaultConfig()
	c.Command = []string{"opencode", "acp"}
	c.Cwd = "/work"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Command = nil },
		func(c *Config) { c.Cwd = "relative" },
		func(c *Config) { c.Permission = "maybe" },
		func(c *Config) { c.InlineImageMaxMB = 6 },
	} {
		bad := *c
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
