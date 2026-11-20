package pi

import (
	"PhiRouter/internal/core"
	"bufio"
	"context"
	"encoding/json"
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
		case "get_state":
			data["sessionFile"] = "/tmp/test-session.jsonl"
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
	wait := func(kind string) core.Event {
		t.Helper()
		for {
			select {
			case e := <-events:
				if e.Type == kind {
					return e
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("missing %s event", kind)
			}
		}
	}
	wait("ready")
	callCtx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	if _, err = b.Call(callCtx, core.Command{Type: "prompt", Fields: map[string]any{"message": "hello"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Call(callCtx, core.Command{Type: "follow_up", Fields: map[string]any{"message": "queued plugin"}}); err != nil {
		t.Fatal(err)
	}
	e := wait("message_update")
	if len(e.Text) != 100007 {
		t.Fatalf("truncated delta: %d", len(e.Text))
	}
	if err = b.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	wait("restarting")
	wait("ready")
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
