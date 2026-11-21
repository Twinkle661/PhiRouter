package router

import (
	"context"
	"errors"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"strings"
	"sync"
	"testing"
)

func TestInstancesSeedQualifyAndCarryReplyRef(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, b := &fakeFrontend{}, &fakeFrontend{}
	r := New(Options{
		Routes: []Route{{"a:*", "test:one"}, {"b:7", "test:two"}},
		Owners: map[string]string{"a": "1", "b": "2"},
	}, map[string]core.Frontend{"a": a, "b": b}, map[string]core.Backend{"test:one": &fakeBackend{}, "test:two": &fakeBackend{}})
	if got := r.states["test:one"].source; got != "a:1" {
		t.Fatalf("wildcard route should default to the owner, got %q", got)
	}
	if got := r.states["test:two"].source; got != "b:7" {
		t.Fatalf("exact route should win, got %q", got)
	}
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.done })
	// The backend is not ready, so the router answers with a queued notice.
	if err := r.Receiver("a")(ctx, core.Envelope{Source: "9", ReplyRef: "m1", Parts: []core.Part{{Type: core.PartText, Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if r.states["test:one"].source != "a:9" {
		t.Fatalf("source not qualified: %q", r.states["test:one"].source)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.addresses) != 1 || a.addresses[0] != "9" || a.refs[0] != "m1" {
		t.Fatalf("reply not addressed to the inbound message: %v %v", a.addresses, a.refs)
	}
}

type eventHook struct {
	mu     sync.Mutex
	events []core.Event
}

func (h *eventHook) OnInbound(_ context.Context, e core.Envelope) (core.Envelope, bool, error) {
	return e, false, nil
}
func (h *eventHook) OnOutbound(_ context.Context, e core.Envelope) (core.Envelope, bool, error) {
	return e, false, nil
}
func (h *eventHook) OnEvent(_ context.Context, _ string, e core.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
	return nil
}

func TestSendNotAllowedIsReportedToHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f, h := &fakeFrontend{refuse: true}, &eventHook{}
	r := New(Options{Routes: fakeRoutes, Owners: fakeOwners, Hooks: []core.Hook{h}}, map[string]core.Frontend{"fake": f}, map[string]core.Backend{"test:default": &fakeBackend{}})
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.done })
	err := r.Send(ctx, core.Envelope{Route: "test:default", Parts: []core.Part{{Type: core.PartFile, Path: "/data/media/out/x"}}})
	if !errors.Is(err, core.ErrSendNotAllowed) {
		t.Fatalf("refusal not surfaced to the caller: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.events) != 1 || h.events[0].Type != "send_not_allowed" || !strings.Contains(h.events[0].Text, "window closed") {
		t.Fatalf("hooks not notified: %+v", h.events)
	}
}

func TestBubblesLeaveLengthToFrontends(t *testing.T) {
	b := Bubbles{Separators: []string{"\n"}}
	long := strings.Repeat("x", 5000)
	if out := b.Feed(long, false); len(out) != 0 {
		t.Fatalf("router split by length: %d bubbles", len(out))
	}
	if out := b.Feed("", true); len(out) != 1 || out[0] != long {
		t.Fatal("unbounded bubble not delivered whole")
	}
}
