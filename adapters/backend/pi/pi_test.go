package pi

import (
	"bufio"
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

// The test binary doubles as a deterministic JSONL child process.
func TestRPCProcess(t *testing.T) {
	if os.Getenv("PHIROUTER_TEST_CHILD") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var cmd map[string]any
		if json.Unmarshal(line, &cmd) != nil {
			os.Exit(2)
		}
		if path := os.Getenv("PHIROUTER_TEST_LOG"); path != "" {
			f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			_, _ = f.Write(line)
			_ = f.Close()
		}
		kind, _ := cmd["type"].(string)
		data := map[string]any{}
		switch kind {
		case "new_session":
			if os.Getenv("PHIROUTER_TEST_DELAY") == "1" {
				time.Sleep(200 * time.Millisecond)
			}
			data["cancelled"] = false
		case "get_state":
			data["sessionId"] = "session-123"
			data["sessionFile"] = "/tmp/test-session.jsonl"
			data["model"] = map[string]string{"provider": "test", "id": "expected"}
		case "get_available_models":
			data["models"] = []map[string]string{{"provider": "test", "id": "expected"}}
		case "prompt":
			data["disposition"] = "started"
		case "extension_ui_response":
			continue
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": cmd["id"], "type": "response", "command": kind, "success": true, "data": data})
		if kind == "prompt" {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": strings.Repeat("a", 100000) + "\u2028tail"}})
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "agent_settled"})
		}
	}
}
func TestSupervisionRestoresSessionAndLargeJSONL(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "commands.jsonl")
	b := New(Options{Command: []string{exe, "-test.run=^TestRPCProcess$"}, Env: []string{"PHIROUTER_TEST_CHILD=1", "PHIROUTER_TEST_LOG=" + log}, StatePath: filepath.Join(dir, "session")})
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan core.Event, 100)
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, func(e core.Event) { events <- e }) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("backend did not stop")
		}
	}()
	wait := func(kind core.EventKind) core.Event {
		t.Helper()
		for {
			select {
			case e := <-events:
				if e.Kind == kind {
					return e
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("missing event kind %d", kind)
			}
		}
	}
	wait(core.EventReady)
	callCtx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	if _, err = b.Prompt(callCtx, core.Envelope{Channel: true, Parts: []core.Part{{Type: core.PartText, Text: "hello"}}}, core.DeliveryPrompt); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Prompt(callCtx, core.Envelope{Parts: []core.Part{{Type: core.PartText, Text: "queued plugin"}}}, core.DeliveryFollowUp); err != nil {
		t.Fatal(err)
	}
	models, err := b.Models(callCtx)
	if err != nil || len(models) != 1 || models[0].ID != "expected" {
		t.Fatalf("models: %+v, %v", models, err)
	}
	if err = b.SetModel(callCtx, models[0]); err != nil {
		t.Fatal(err)
	}
	if err = b.Respond(callCtx, core.Reply{ID: "dialog-1", OptionID: "pick"}); err != nil {
		t.Fatal(err)
	}
	if err = b.Cancel(callCtx); err != nil {
		t.Fatal(err)
	}
	e := wait(core.EventMessageChunk)
	if len(e.Text) != 100007 {
		t.Fatalf("truncated delta: %d", len(e.Text))
	}
	if err = b.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	wait(core.EventRestarting)
	wait(core.EventReady)
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sessionPath":"/tmp/test-session.jsonl"`) {
		t.Fatalf("session not restored: %s", raw)
	}
	if !strings.Contains(string(raw), `"streamingBehavior":"followUp"`) {
		t.Fatalf("follow-up is not atomic: %s", raw)
	}
	clearAt, abortAt := strings.Index(string(raw), `"type":"clear_queue"`), strings.Index(string(raw), `"type":"abort"`)
	if clearAt < 0 || abortAt < clearAt {
		t.Fatalf("abort did not clear queue first: %s", raw)
	}
	if !strings.Contains(string(raw), `"modelId":"expected"`) || !strings.Contains(string(raw), `"id":"dialog-1"`) || !strings.Contains(string(raw), `"value":"pick"`) {
		t.Fatalf("typed command mapping failed: %s", raw)
	}
	saved, _ := os.ReadFile(filepath.Join(dir, "session"))
	if strings.TrimSpace(string(saved)) != "/tmp/test-session.jsonl" {
		t.Fatal(string(saved))
	}
}
func TestExtensionUIStringMessage(t *testing.T) {
	b := New(Options{})
	var got core.Event
	b.record([]byte(`{"type":"extension_ui_request","id":"a","method":"confirm","message":"Continue?","title":"Confirm"}`), func(e core.Event) { got = e })
	if got.Dialog == nil || got.Dialog.Message != "Continue?" {
		t.Fatalf("bad event: %+v", got)
	}
}

func TestCommandTimeoutDoesNotKillProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := New(Options{Command: []string{exe, "-test.run=^TestRPCProcess$"}, Env: []string{"PHIROUTER_TEST_CHILD=1", "PHIROUTER_TEST_DELAY=1"}})
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan core.Event, 100)
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, func(e core.Event) { events <- e }) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("backend did not shut down")
		}
	}()
	select {
	case e := <-events:
		if e.Kind != core.EventReady {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backend not ready")
	}
	b.mu.Lock()
	pid := b.cmd.Process.Pid
	b.mu.Unlock()
	slowCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err = b.NewSession(slowCtx)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	b.mu.Lock()
	pending := len(b.pending)
	b.mu.Unlock()
	if pending != 0 {
		t.Fatal("timed-out request retained")
	}
	nextCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	state, err := b.Status(nextCtx)
	if err != nil {
		t.Fatalf("process unusable after timeout: %v", err)
	}
	if state.Model.ID != "expected" || state.SessionID != "session-123" {
		t.Fatalf("late response matched a new request: %+v", state)
	}
	b.mu.Lock()
	same := b.cmd != nil && b.cmd.Process.Pid == pid
	b.mu.Unlock()
	if !same {
		t.Fatal("process restarted after command timeout")
	}
	select {
	case e := <-events:
		t.Fatalf("unexpected lifecycle event: %+v", e)
	default:
	}
}

func TestBlockedWriterAndWaitingCallerCanTimeOut(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	b := New(Options{})
	b.stdin = writer
	b.exited = make(chan struct{})
	b.writes = make(chan writeRequest)
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); writeLoop(writer, b.writes, b.exited) }()
	defer func() { _ = writer.Close(); close(b.exited); <-writerDone }()
	for _, kind := range []string{"get_state", "new_session"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := b.rpc(ctx, kind, nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: expected deadline, got %v", kind, err)
		}
	}
	// The first record was already being written. The second caller timed out
	// waiting for the writer and must never appear on the wire.
	serverDone := make(chan error, 1)
	go func() {
		br := bufio.NewReader(reader)
		for i := 0; i < 2; i++ {
			line, err := br.ReadBytes('\n')
			if err != nil {
				serverDone <- err
				return
			}
			var cmd struct{ Type, ID string }
			if err = json.Unmarshal(line, &cmd); err != nil {
				serverDone <- err
				return
			}
			if cmd.Type != "get_state" {
				serverDone <- errors.New("cancelled queued command was written")
				return
			}
			raw, _ := json.Marshal(map[string]any{"id": cmd.ID, "type": "response", "success": true, "data": map[string]any{"model": map[string]string{"id": "alive"}}})
			b.record(raw, func(core.Event) {})
		}
		serverDone <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, err := b.Status(ctx)
	if err != nil {
		t.Fatalf("writer was closed by timeout: %v", err)
	}
	if state.Model.ID != "alive" {
		t.Fatal(state)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestToolEventsAreNormalizedWithoutResults(t *testing.T) {
	b := New(Options{})
	var events []core.Event
	for _, line := range []string{`{"type":"tool_execution_start","toolName":"bash","args":{"command":"secret"}}`, `{"type":"tool_execution_update","toolName":"bash","partialResult":"large secret output"}`, `{"type":"tool_execution_end","toolName":"bash","isError":true,"durationMs":1234,"result":"secret"}`} {
		b.record([]byte(line), func(e core.Event) { events = append(events, e) })
	}
	if events[0].Tool == nil || events[0].Tool.Status != core.ToolInProgress || events[0].Tool.Detail != "secret" || events[1].Tool != nil || events[1].Kind != core.EventExtension || events[2].Tool == nil || events[2].Tool.Status != core.ToolFailed || events[2].Tool.Detail != "" || events[2].Tool.Duration != 1234*time.Millisecond {
		t.Fatalf("bad tool events: %+v", events)
	}
}

func TestToolDetail(t *testing.T) {
	for args, want := range map[string]string{
		`{"command":"git status","timeout":5}`: "git status",
		`{"path":"src/main.go","offset":10}`:   "src/main.go",
		`{"pattern":"TODO","path":"internal"}`: "TODO internal",
		`{"to": "x",  "n": 1}`:                 `{"to":"x","n":1}`,
		`{}`:                                   "",
		`not json`:                             "",
		``:                                     "",
	} {
		if got := toolDetail(json.RawMessage(args)); got != want {
			t.Errorf("toolDetail(%s) = %q, want %q", args, got, want)
		}
	}
}

func TestDialogsTranslateAndAnswerInKind(t *testing.T) {
	b := New(Options{})
	var events []core.Event
	for _, line := range []string{
		`{"type":"extension_ui_request","id":"c","method":"confirm","title":"Proceed?"}`,
		`{"type":"extension_ui_request","id":"s","method":"select","title":"Pick","options":["a","b"]}`,
		`{"type":"extension_ui_request","id":"e","method":"editor","title":"Edit","prefill":"x","timeout":1500}`,
		`{"type":"extension_ui_request","id":"n","method":"notify","message":"heads up"}`,
		`{"type":"extension_ui_request","id":"w","method":"setWidget"}`,
	} {
		b.record([]byte(line), func(e core.Event) { events = append(events, e) })
	}
	confirm, sel, edit := events[0].Dialog, events[1].Dialog, events[2].Dialog
	if confirm == nil || len(confirm.Options) != 2 || !confirm.Options[1].Kind.Rejects() {
		t.Fatalf("confirm: %+v", events[0])
	}
	if sel == nil || sel.Kind != core.DialogSelect || sel.Options[1].ID != "b" || sel.Options[1].Label != "b" {
		t.Fatalf("select: %+v", events[1])
	}
	if edit == nil || edit.Kind != core.DialogInput || !edit.Multiline || edit.Timeout != 1500*time.Millisecond {
		t.Fatalf("editor: %+v", events[2])
	}
	if events[3].Kind != core.EventNotice || events[3].Text != "heads up" || events[4].Kind != core.EventExtension || len(events[4].Raw) == 0 {
		t.Fatalf("notify/other: %+v %+v", events[3], events[4])
	}
	// Answers are written in the shape each pi method expects.
	reader, writer := io.Pipe()
	defer reader.Close()
	b.stdin = writer
	b.exited = make(chan struct{})
	b.writes = make(chan writeRequest)
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); writeLoop(writer, b.writes, b.exited) }()
	defer func() { _ = writer.Close(); close(b.exited); <-writerDone }()
	got := make(chan map[string]any, 3)
	go func() {
		br := bufio.NewReader(reader)
		for i := 0; i < 3; i++ {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(line, &m)
			got <- m
		}
	}()
	ctx := context.Background()
	for _, r := range []core.Reply{{ID: "c", OptionID: "yes"}, {ID: "s", OptionID: "b"}, {ID: "e", Text: "edited"}} {
		if err := b.Respond(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	c, s, e := <-got, <-got, <-got
	if c["confirmed"] != true || s["value"] != "b" || e["value"] != "edited" {
		t.Fatalf("answers: %v %v %v", c, s, e)
	}
}

func TestMCPRegistrationPreservesEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"existing":{"command":"example"}},"custom":true}`), 0600)
	if err := RegisterMCP(path, "http://127.0.0.1:8081/mcp"); err != nil {
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
	if !strings.Contains(string(raw), "${ROUTER_MCP__TOKEN}") || !strings.Contains(string(raw), core.RouteHeader) {
		t.Fatal("missing secret or route reference")
	}
	_ = os.WriteFile(path, []byte(`null`), 0600)
	if err := RegisterMCP(path, "endpoint"); err == nil {
		t.Fatal("null config accepted")
	}
}

func TestFromConfigUsesAgentDir(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	c := DefaultConfig()
	c.AgentDir = dir
	b, err := FromConfig(c, state, core.ToolServer{URL: "http://127.0.0.1:8081/mcp", Token: "secret", Route: "zero"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.opt.StatePath != filepath.Join(state, "session") || b.opt.InlineMax != 5*1024*1024 {
		t.Fatalf("options: %+v", b.opt)
	}
	env := strings.Join(b.opt.Env, " ")
	if !strings.Contains(env, "PI_CODING_AGENT_DIR="+dir) || !strings.Contains(env, "PHIROUTER_BACKEND=zero") {
		t.Fatal(env)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if strings.Contains(string(raw), "secret") {
		t.Fatal("token written to disk")
	}
}
