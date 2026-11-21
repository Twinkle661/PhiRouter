package router

import (
	"context"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestTimestampFormattingAndMedia(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	r := New(Options{Timestamp: TimestampOptions{Enabled: true, Location: loc, Format: "[2006-01-02 15:04:05 MST] "}}, nil, nil)
	for _, tc := range []struct {
		month time.Month
		want  string
	}{
		{time.January, "[2026-01-02 12:30:00 GMT] hello"},
		{time.July, "[2026-07-02 13:30:00 BST] hello"},
	} {
		e := env("hello")
		got := r.timestamp(e, time.Date(2026, tc.month, 2, 12, 30, 0, 0, time.UTC))
		if got.Parts[0].Text != tc.want || e.Parts[0].Text != "hello" {
			t.Fatalf("unexpected prefix or input mutation: %+v", got)
		}
	}
	r.opt.Timestamp.Format = "15:04\n"
	media := core.Part{Type: core.PartImage, Path: "/image.png", Caption: "caption"}
	got := r.timestamp(core.Envelope{Parts: []core.Part{media}}, time.Date(2026, 1, 2, 12, 30, 0, 0, time.UTC))
	if len(got.Parts) != 2 || got.Parts[0].Text != "12:30\n" || got.Parts[1] != media {
		t.Fatalf("media changed: %+v", got)
	}
}

func TestTimestampAcceptanceQueueRecoveryAndRetry(t *testing.T) {
	ctx := context.Background()
	b, f := &fakeBackend{}, &fakeFrontend{}
	r := New(Options{Routes: []Route{{"fake:*", "default"}}, Timestamp: TimestampOptions{Enabled: true, Format: "[2006-01-02T15:04:05.000000000Z07:00] "}}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"default": b})
	s := r.states["default"]
	user := env("hello")
	user.Channel = true
	if err := r.accept(ctx, user); err != nil {
		t.Fatal(err)
	}
	stamped := s.queue[0].Parts[0].Text
	if stamped == "hello" {
		t.Fatal("queued message missing timestamp")
	}
	r.event(ctx, "default", s, core.Event{Kind: core.EventReady})
	retry := env("/retry")
	retry.Channel = true
	if err := r.accept(ctx, retry); err != nil {
		t.Fatal(err)
	}
	b.fail = true
	if err := r.accept(ctx, retry); err != nil {
		t.Fatal(err)
	}
	b.fail = false
	r.event(ctx, "default", s, core.Event{Kind: core.EventReady})
	for _, call := range b.prompts() {
		if call.Envelope.Parts[0].Text != stamped {
			t.Fatalf("timestamp changed on retry: %+v", call)
		}
	}
	if len(b.prompts()) != 3 {
		t.Fatal("missing retries")
	}
	if err := r.accept(ctx, env("injected")); err != nil {
		t.Fatal(err)
	}
	if b.prompts()[3].Envelope.Parts[0].Text != "injected" {
		t.Fatal("injection stamped")
	}
	status := env("/status")
	status.Channel = true
	if err := r.accept(ctx, status); err != nil {
		t.Fatal(err)
	}
	if len(b.prompts()) != 4 {
		t.Fatal("command forwarded")
	}
	r.opt.Timestamp.Enabled = false
	if err := r.accept(ctx, user); err != nil {
		t.Fatal(err)
	}
	if b.prompts()[4].Envelope.Parts[0].Text != "hello" {
		t.Fatal("disabled timestamp applied")
	}
}

func TestTimestampUsesSendTimeAndSkipsSlashCommands(t *testing.T) {
	ctx := context.Background()
	b, f := &fakeBackend{}, &fakeFrontend{}
	r := New(Options{Routes: []Route{{"fake:*", "default"}}, Timestamp: TimestampOptions{Enabled: true, Format: "[15:04] "}}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"default": b})
	r.event(ctx, "default", r.states["default"], core.Event{Kind: core.EventReady})
	sent := env("backlog")
	sent.Channel = true
	sent.Time = time.Date(2026, 11, 21, 3, 15, 0, 0, time.UTC)
	if err := r.accept(ctx, sent); err != nil {
		t.Fatal(err)
	}
	skill := env("  /skill:poem moon")
	skill.Channel = true
	if err := r.accept(ctx, skill); err != nil {
		t.Fatal(err)
	}
	p := b.prompts()
	if len(p) != 2 {
		t.Fatalf("prompts: %+v", p)
	}
	if got := p[0].Envelope.Parts[0].Text; got != "[03:15] backlog" {
		t.Fatalf("send time not used: %q", got)
	}
	if got := p[1].Envelope.Parts[0].Text; got != "  /skill:poem moon" {
		t.Fatalf("slash command stamped: %q", got)
	}
}
