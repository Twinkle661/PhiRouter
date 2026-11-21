// Package sendvoice implements the send_voice MCP tool: synthesize text and
// deliver it to the user as audio. The audio exists on disk only for the
// duration of the delivery.
package sendvoice

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"io"
	"log/slog"
	"strings"
)

// Synthesizer turns text into an audio stream and reports its file
// extension. style describes delivery (emotion, tone) and may be empty.
type Synthesizer interface {
	Synthesize(ctx context.Context, text, style string) (io.ReadCloser, string, error)
}

// Scratch is the media storage the tool needs: a confined outbound file that
// is removed after delivery.
type Scratch interface {
	SaveOut(ctx context.Context, name, mime string, r io.Reader, limit int64) (core.Part, error)
	Remove(path string) error
}

type Tool struct {
	Synth Synthesizer
	Media Scratch
	Send  core.Inject
	// Style exposes the optional style argument; StyleBase is prepended to it.
	Style     bool
	StyleBase string
}

// mimeTypes avoids depending on the container's MIME database.
var mimeTypes = map[string]string{".mp3": "audio/mpeg", ".opus": "audio/ogg", ".ogg": "audio/ogg"}

// maxAudio matches Telegram's bot upload limit.
const maxAudio = 50 << 20

func (Tool) Name() string { return "send_voice" }
func (t Tool) Description() string {
	d := "Speak text aloud: synthesize it with the configured voice and send it to the user as an audio message immediately."
	if t.Style {
		d += " Optionally describe how to say it (emotion, mood, pace) in style; do not describe voice characteristics."
	}
	return d
}
func (t Tool) InputSchema() map[string]any {
	props := map[string]any{"text": map[string]string{"type": "string", "description": "What to say."}}
	if t.Style {
		props["style"] = map[string]string{"type": "string", "description": "How to say it this time: emotion, mood or pace, e.g. \"sleepy and tender\"."}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"text"}, "additionalProperties": false}
}
func (t Tool) Call(ctx context.Context, route string, raw json.RawMessage) (string, error) {
	var args struct{ Text, Style string }
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Text) == "" {
		return "", invalidParams("Expected send_voice with nonempty text")
	}
	style := ""
	if t.Style {
		style = strings.TrimSpace(strings.Join(nonempty(t.StyleBase, args.Style), "\n"))
	}
	audio, ext, err := t.Synth.Synthesize(ctx, args.Text, style)
	if err != nil {
		return "", err
	}
	part, err := t.Media.SaveOut(ctx, "voice"+ext, mimeTypes[ext], audio, maxAudio)
	_ = audio.Close()
	if err != nil {
		return "", fmt.Errorf("could not store synthesized audio: %w", err)
	}
	part.Type = core.PartAudio
	defer func() {
		if err := t.Media.Remove(part.Path); err != nil {
			slog.Warn("could not remove voice file; media cleanup will", "error", err)
		}
	}()
	if err = t.Send(ctx, core.Envelope{Route: route, Parts: []core.Part{part}}); err != nil {
		return "", err
	}
	return "Voice message sent.", nil
}

func nonempty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			out = append(out, strings.TrimSpace(x))
		}
	}
	return out
}

type invalidParams string

func (e invalidParams) Error() string       { return string(e) }
func (e invalidParams) InvalidParams() bool { return true }
