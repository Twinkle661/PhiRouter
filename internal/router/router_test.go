package router

import (
	"PhiRouter/internal/core"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeFrontend struct {
	mu      sync.Mutex
	texts   []string
	dialogs []core.Dialog
}

func (f *fakeFrontend) Run(ctx context.Context, _ func(context.Context, core.Envelope) error) error {
	<-ctx.Done()
	return nil
}
func (f *fakeFrontend) Send(_ context.Context, _ string, e core.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range e.Parts {
		f.texts = append(f.texts, p.Text)
	}
	return nil
}
func (f *fakeFrontend) Typing(context.Context, string) error { return nil }
func (f *fakeFrontend) Dialog(_ context.Context, _ string, d core.Dialog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialogs = append(f.dialogs, d)
	return nil
}

type submissionCall struct {
	Envelope core.Envelope
	Mode     core.DeliveryMode
}
type fakeBackend struct {
	mu    sync.Mutex
	calls []submissionCall
	fail  bool
}

func (f *fakeBackend) Run(context.Context, func(core.Event)) error { return nil }
func (f *fakeBackend) Submit(_ context.Context, e core.Envelope, mode core.DeliveryMode) (core.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return core.Submission{}, core.ErrUnavailable
	}
	f.calls = append(f.calls, submissionCall{e, mode})
	return core.Submission{}, nil
}
func (f *fakeBackend) Status(context.Context) (core.BackendState, error) {
	return core.BackendState{SessionID: "session-123", Model: core.Model{Provider: "test", ID: "model"}}, nil
}
func (f *fakeBackend) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{Provider: "test", ID: "model"}}, nil
}
func (f *fakeBackend) SetModel(context.Context, core.Model) error { return nil }
func (f *fakeBackend) NewSession(context.Context) (bool, error)   { return true, nil }
func (f *fakeBackend) Abort(context.Context) error                { return nil }
func (f *fakeBackend) Respond(context.Context, core.Reply) error  { return nil }
func (f *fakeBackend) Restart(context.Context) error              { return nil }
func (f *fakeBackend) Close(context.Context) error                { return nil }
func (f *fakeBackend) prompts() []submissionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]submissionCall(nil), f.calls...)
}
func setup(t *testing.T) (*Router, *fakeFrontend, *fakeBackend, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f, b := &fakeFrontend{}, &fakeBackend{}
	r := New(Options{Routes: []Route{{"fake:*", "test:default"}}, DefaultSource: "fake:1", Separators: []string{"\n", "<break />"}, QueueLimit: 4}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"test:default": b})
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.done })
	return r, f, b, ctx
}
func env(text string) core.Envelope {
	return core.Envelope{Route: "test:default", Source: "fake:1", Parts: []core.Part{{Type: core.PartText, Text: text}}}
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func TestSchedulingAndRecovery(t *testing.T) {
	r, _, b, ctx := setup(t)
	for _, e := range []core.Envelope{env("plugin1"), env("plugin2")} {
		if err := r.Inject(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Inbound(ctx, env("user1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Inbound(ctx, env("user2")); err != nil {
		t.Fatal(err)
	}
	r.Event(ctx, "test:default", core.Event{Type: "ready"})
	eventually(t, func() bool { return len(b.prompts()) == 4 })
	calls := b.prompts()
	want := []string{"user1", "user2", "plugin1", "plugin2"}
	kinds := []core.DeliveryMode{core.DeliveryStart, core.DeliveryInterrupt, core.DeliveryQueue, core.DeliveryQueue}
	for i, c := range calls {
		if c.Mode != kinds[i] || c.Envelope.Parts[0].Text != want[i] {
			t.Fatalf("call %d: %+v", i, c)
		}
	}
	// agent_end alone must not make pi idle while follow-ups remain.
	r.Event(ctx, "test:default", core.Event{Type: "agent_end"})
	if err := r.Inbound(ctx, env("interrupt")); err != nil {
		t.Fatal(err)
	}
	if got := b.prompts()[4].Mode; got != core.DeliveryInterrupt {
		t.Fatal(got)
	}
}
func TestBubbleBoundaries(t *testing.T) {
	b := Bubbles{Separators: []string{"\n", "<break />"}, Limit: 4096}
	if got := b.Feed("hello<bre", false); len(got) != 0 {
		t.Fatal(got)
	}
	got := b.Feed("ak />world\n\nlast", false)
	if strings.Join(got, "|") != "hello|world" {
		t.Fatal(got)
	}
	if got = b.Feed("", true); len(got) != 1 || got[0] != "last" {
		t.Fatal(got)
	}
	b = Bubbles{Separators: []string{"<break />"}, Limit: 4}
	got = b.Feed("😀😀😀<break />x", true)
	if strings.Join(got, "|") != "😀😀|😀|x" {
		t.Fatal(got)
	}
}
func TestShutdownFlushes(t *testing.T) {
	r, f, _, ctx := setup(t)
	r.Event(ctx, "test:default", core.Event{Type: "message_update", Text: "first\nsecond"})
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.texts) == 1 })
	if err := r.Send(ctx, env("media")); err != nil {
		t.Fatal(err)
	}
	r.Event(ctx, "test:default", core.Event{Type: "message_end"})
	eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Join(f.texts, "|") == "first|media|second"
	})
}

type badHook struct{}

func (badHook) OnInbound(_ context.Context, e core.Envelope) (core.Envelope, bool, error) {
	e.Parts[0].Text = "mutated"
	return e, false, errors.New("hook failure")
}
func (badHook) OnOutbound(_ context.Context, e core.Envelope) (core.Envelope, bool, error) {
	return e, false, nil
}
func (badHook) OnEvent(context.Context, string, core.Event) error { return nil }
func TestHookFailureDoesNotChangeMessage(t *testing.T) {
	r, _, b, ctx := setup(t)
	r.opt.Hooks = []core.Hook{badHook{}}
	r.Event(ctx, "test:default", core.Event{Type: "ready"})
	if err := r.Inbound(ctx, env("original")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(b.prompts()) == 1 })
	if got := b.prompts()[0].Envelope.Parts[0].Text; got != "original" {
		t.Fatal(got)
	}
}
func TestQueueLimitAndUnknownRoute(t *testing.T) {
	r, _, _, ctx := setup(t)
	for i := 0; i < 4; i++ {
		if err := r.Inject(ctx, env("queued")); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Inject(ctx, env("overflow")); err == nil {
		t.Fatal("expected queue full")
	}
	e := env("bad")
	e.Route = "missing"
	if err := r.Inject(ctx, e); err == nil {
		t.Fatal("expected unknown route")
	}
}
func TestUnknownSlashPreservesChannelPriorityWhenBusy(t *testing.T) {
	r, _, b, ctx := setup(t)
	r.Event(ctx, "test:default", core.Event{Type: "ready"})
	_ = r.Inbound(ctx, env("hello"))
	eventually(t, func() bool { return len(b.prompts()) == 1 })
	if err := r.Inbound(ctx, env("/extension")); err != nil {
		t.Fatal(err)
	}
	c := b.prompts()[1]
	if c.Mode != core.DeliveryInterrupt || c.Envelope.Parts[0].Text != "/extension" {
		t.Fatal(c)
	}
}

func TestGracefulShutdownActuallyFlushes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f, b := &fakeFrontend{}, &fakeBackend{}
	r := New(Options{DefaultSource: "fake:1"}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"test:default": b})
	go r.Run(ctx)
	r.Event(ctx, "test:default", core.Event{Type: "message_update", Text: "unfinished bubble"})
	// A status request is a barrier after the queued event is observed.
	eventually(t, func() bool { return len(r.events) == 0 })
	cancel()
	<-r.done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.texts) != 1 || f.texts[0] != "unfinished bubble" {
		t.Fatalf("lost buffered text: %v", f.texts)
	}
}

func TestToolOutputSummarizesOnlyStartAndEnd(t *testing.T) {
	for _, show := range []bool{false, true} {
		f, b := &fakeFrontend{}, &fakeBackend{}
		r := New(Options{DefaultSource: "fake:1", ShowTools: show}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"test:default": b})
		events := []core.Event{{Type: "tool_execution_start", Data: []byte(`{"secret":"argument"}`), Tool: &core.ToolEvent{Name: "bash", Phase: core.ToolStarted}}}
		for i := 0; i < 100; i++ {
			events = append(events, core.Event{Type: "tool_execution_update", Data: []byte(`{"partialResult":"large secret output"}`)})
		}
		events = append(events, core.Event{Type: "tool_execution_end", Data: []byte(`{"result":"secret"}`), Tool: &core.ToolEvent{Name: "bash", Phase: core.ToolFinished, Failed: true, Duration: time.Second}})
		for _, e := range events {
			r.event(context.Background(), "test:default", r.states["test:default"], e)
		}
		if !show {
			if len(f.texts) != 0 {
				t.Fatal(f.texts)
			}
			continue
		}
		if strings.Join(f.texts, "|") != "Tool started: bash|Tool failed: bash (1s)" {
			t.Fatalf("unexpected tool output: %v", f.texts)
		}
	}
}

func TestSessionCommandsShowDetails(t *testing.T) {
	f, b := &fakeFrontend{}, &fakeBackend{}
	r := New(Options{DefaultSource: "fake:1"}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"test:default": b})
	s := r.states["test:default"]
	s.ready = true
	for _, command := range []string{"/new session", "/status"} {
		handled, err := r.command(context.Background(), "test:default", s, env(command))
		if err != nil || !handled {
			t.Fatalf("%s: %v", command, err)
		}
		text := f.texts[len(f.texts)-1]
		for _, detail := range []string{"Session: session-123", "Provider: test", "Model: model"} {
			if !strings.Contains(text, detail) {
				t.Fatalf("missing %q in %q", detail, text)
			}
		}
	}
}
