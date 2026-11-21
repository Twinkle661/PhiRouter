package sendvoice

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"github.com/Twinkle661/PhiRouter/internal/media"
	"io"
	"os"
	"strings"
	"testing"
)

type fakeSynth struct{ text, style string }

func (f *fakeSynth) Synthesize(_ context.Context, text, style string) (io.ReadCloser, string, error) {
	if text == "fail" {
		return nil, "", errors.New("speech provider returned 401: no key")
	}
	f.text, f.style = text, style
	return io.NopCloser(strings.NewReader("ID3audio")), ".mp3", nil
}

func TestSendVoice(t *testing.T) {
	store, err := media.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	synth := &fakeSynth{}
	var sent core.Envelope
	tool := Tool{Synth: synth, Media: store, Style: true, StyleBase: "Warm, close to the mic.", Send: func(_ context.Context, e core.Envelope) error {
		sent = e
		data, err := os.ReadFile(e.Parts[0].Path)
		if err != nil || string(data) != "ID3audio" {
			t.Fatalf("audio not on disk during delivery: %v", err)
		}
		return nil
	}}
	text, err := tool.Call(context.Background(), "pi:default", json.RawMessage(`{"text":"good night","style":"sleepy"}`))
	if err != nil || text != "Voice message sent." {
		t.Fatal(text, err)
	}
	p := sent.Parts[0]
	if sent.Route != "pi:default" || p.Type != core.PartAudio || p.MIME != "audio/mpeg" || !strings.HasSuffix(p.Path, ".mp3") || !strings.Contains(p.Path, "/out/") {
		t.Fatalf("unexpected envelope: %+v", sent)
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatal("voice file kept after delivery")
	}
	if synth.text != "good night" || synth.style != "Warm, close to the mic.\nsleepy" {
		t.Fatalf("unexpected synthesis input %q %q", synth.text, synth.style)
	}
	// No per-message style: only the base is sent.
	if _, err = tool.Call(context.Background(), "pi:default", json.RawMessage(`{"text":"hi"}`)); err != nil || synth.style != "Warm, close to the mic." {
		t.Fatal(synth.style, err)
	}
	// Style disabled: never sent, and not offered in the schema.
	tool.Style = false
	if _, err = tool.Call(context.Background(), "pi:default", json.RawMessage(`{"text":"hi"}`)); err != nil || synth.style != "" {
		t.Fatal(synth.style, err)
	}
	if _, ok := tool.InputSchema()["properties"].(map[string]any)["style"]; ok {
		t.Fatal("style offered while disabled")
	}
	// Delivery failure still removes the file and reaches the agent.
	tool.Send = func(_ context.Context, e core.Envelope) error {
		sent = e
		return core.ErrSendNotAllowed
	}
	if _, err = tool.Call(context.Background(), "pi:default", json.RawMessage(`{"text":"hi"}`)); !errors.Is(err, core.ErrSendNotAllowed) {
		t.Fatal(err)
	}
	if _, err := os.Stat(sent.Parts[0].Path); !os.IsNotExist(err) {
		t.Fatal("voice file kept after failed delivery")
	}
	if _, err = tool.Call(context.Background(), "pi:default", json.RawMessage(`{"text":"fail"}`)); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatal(err)
	}
	var invalid interface{ InvalidParams() bool }
	if _, err = tool.Call(context.Background(), "pi:default", json.RawMessage(`{"text":"  "}`)); !errors.As(err, &invalid) {
		t.Fatal(err)
	}
}
