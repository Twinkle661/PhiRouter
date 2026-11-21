package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"log/slog"
	"strings"
	"time"
)

type Route struct{ From, To string }
type TimestampOptions struct {
	Enabled  bool
	Location *time.Location
	Format   string
}

type Options struct {
	Timestamp TimestampOptions
	Routes    []Route
	// Owners maps frontend instance names to their bound user's address. A
	// backend reached only through wildcard routes initially replies there.
	Owners     map[string]string
	Separators []string
	// ShowTools posts a notice when a tool starts and when one fails.
	// ToolDetail appends the backend's argument summary to start notices,
	// clipped to ToolDetailLimit runes (default 120).
	ShowTools       bool
	ToolDetail      bool
	ToolDetailLimit int
	QueueLimit      int
	// BusyPolicy chooses, per backend, how a user message is handled while
	// a backend that cannot steer is busy. Missing entries mean BusyQueue.
	BusyPolicy map[string]BusyPolicy
	Hooks      []core.Hook
}

// BusyPolicy applies to user messages arriving while a backend without
// Capabilities.Steer is in a turn.
type BusyPolicy uint8

const (
	// BusyQueue holds the message until the turn ends.
	BusyQueue BusyPolicy = iota
	// BusyInterrupt cancels the turn; the message is sent once it ends.
	BusyInterrupt
)

type request struct {
	e        core.Envelope
	outbound bool
	result   chan error
}
type backendEvent struct {
	route string
	e     core.Event
}
type state struct {
	key         string
	backend     core.Backend
	caps        core.Capabilities
	policy      BusyPolicy
	ready, busy bool
	// cancelling is set while an interrupt waits for the turn to end.
	cancelling bool
	// draining is set while held messages are re-dispatched; re-held
	// messages neither interrupt nor notify again.
	draining bool
	source   string
	replyRef string
	queue    []core.Envelope
	last     *core.Envelope
	bubbles  Bubbles
	models   map[string]core.Model
}
type Router struct {
	opt       Options
	frontends map[string]core.Frontend
	states    map[string]*state
	in        chan request
	events    chan backendEvent
	done      chan struct{}
}

func New(o Options, f map[string]core.Frontend, b map[string]core.Backend) *Router {
	if o.QueueLimit < 1 {
		o.QueueLimit = 256
	}
	if o.ToolDetailLimit < 1 {
		o.ToolDetailLimit = 120
	}
	r := &Router{opt: o, frontends: f, states: map[string]*state{}, in: make(chan request, 64), events: make(chan backendEvent, 1024), done: make(chan struct{})}
	for key, backend := range b {
		r.states[key] = &state{key: key, backend: backend, caps: backend.Capabilities(), policy: o.BusyPolicy[key], bubbles: Bubbles{Separators: o.Separators}, models: map[string]core.Model{}}
	}
	// Exact routes seed the return address; otherwise the first wildcard
	// route's frontend owner is used until inbound traffic arrives.
	for _, route := range o.Routes {
		if s := r.states[route.To]; s != nil && s.source == "" {
			if name, ok := strings.CutSuffix(route.From, ":*"); ok && o.Owners[name] != "" {
				s.source = name + ":" + o.Owners[name]
			}
		}
	}
	for _, route := range o.Routes {
		if s := r.states[route.To]; s != nil && !strings.HasSuffix(route.From, ":*") {
			s.source = route.From
		}
	}
	return r
}

// Receiver returns the inbound callback for a frontend instance. Adapters
// report bare addresses; the router qualifies them with the instance name.
func (r *Router) Receiver(frontend string) func(context.Context, core.Envelope) error {
	return func(ctx context.Context, e core.Envelope) error {
		e.Source = frontend + ":" + e.Source
		return r.Inbound(ctx, e)
	}
}
func (r *Router) submit(ctx context.Context, e core.Envelope, out bool) error {
	req := request{e, out, make(chan error, 1)}
	select {
	case <-r.done:
		return errors.New("router stopped")
	case <-ctx.Done():
		return ctx.Err()
	case r.in <- req:
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return errors.New("router stopped")
	}
}
func (r *Router) Inbound(ctx context.Context, e core.Envelope) error {
	e.Channel = true
	return r.submit(ctx, e, false)
}
func (r *Router) Inject(ctx context.Context, e core.Envelope) error {
	e.Channel = false
	e.Source = ""
	e.Reply = nil
	return r.submit(ctx, e, false)
}
func (r *Router) Send(ctx context.Context, e core.Envelope) error { return r.submit(ctx, e, true) }
func (r *Router) Event(ctx context.Context, route string, e core.Event) {
	select {
	case r.events <- backendEvent{route, e}:
	case <-ctx.Done():
	case <-r.done:
	}
}
func (r *Router) Run(ctx context.Context) error {
	defer close(r.done)
	tick := time.NewTicker(4 * time.Second)
	defer tick.Stop()
	for {
		select {
		case req := <-r.in:
			if req.outbound {
				s := r.states[req.e.Route]
				if s == nil {
					req.result <- errors.New("unknown route")
				} else {
					req.result <- r.deliver(ctx, s, req.e)
				}
			} else {
				req.result <- r.accept(ctx, req.e)
			}
		case ev := <-r.events:
			if s := r.states[ev.route]; s != nil {
				r.event(ctx, ev.route, s, ev.e)
			}
		case <-tick.C:
			for _, s := range r.states {
				if s.busy {
					f, chat := r.front(s.source)
					if f != nil {
						if err := f.Typing(ctx, chat); err != nil {
							slog.Error("typing failed", "error", err)
						}
					}
				}
			}
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			for _, s := range r.states {
				r.flush(flushCtx, s, "", true)
			}
			cancel()
			return nil
		}
	}
}
func (r *Router) front(source string) (core.Frontend, string) {
	name, chat, _ := strings.Cut(source, ":")
	return r.frontends[name], chat
}
func (r *Router) resolve(e core.Envelope) (string, error) {
	if !e.Channel {
		if r.states[e.Route] != nil {
			for _, rule := range r.opt.Routes {
				if rule.To == e.Route {
					return e.Route, nil
				}
			}
		}
		return "", errors.New("unknown route")
	}
	for _, rule := range r.opt.Routes {
		if rule.From == e.Source {
			return rule.To, nil
		}
	}
	for _, rule := range r.opt.Routes {
		prefix, ok := strings.CutSuffix(rule.From, "*")
		if ok && strings.HasPrefix(e.Source, prefix) {
			return rule.To, nil
		}
	}
	return "", errors.New("no route for channel")
}
func (r *Router) accept(ctx context.Context, e core.Envelope) error {
	key, err := r.resolve(e)
	if err != nil {
		return err
	}
	e.Route = key
	s := r.states[key]
	if e.Channel {
		s.source = e.Source
		if e.Reply == nil {
			s.replyRef = e.ReplyRef
		}
	}
	if e.Reply != nil {
		return r.reply(ctx, s, *e.Reply)
	}
	if e.Channel && len(e.Parts) > 0 && e.Parts[0].Type == core.PartText {
		handled, err := r.command(ctx, key, s, e)
		if handled {
			return err
		}
	}
	if e.Channel && r.opt.Timestamp.Enabled && !slashCommand(e) {
		sent := e.Time
		if sent.IsZero() {
			sent = time.Now()
		}
		e = r.timestamp(e, sent)
	}
	return r.dispatch(ctx, s, e)
}

// slashCommand reports whether e is a slash command the router did not handle
// itself (e.g. an agent skill or prompt template). These are forwarded unprefixed
// so the backend still recognises them.
func slashCommand(e core.Envelope) bool {
	return len(e.Parts) > 0 && e.Parts[0].Type == core.PartText && strings.HasPrefix(strings.TrimSpace(e.Parts[0].Text), "/")
}

// timestamp runs once on acceptance, before queueing; retries reuse this prefix.
// sent is the frontend-reported send time, falling back to acceptance time.
func (r *Router) timestamp(e core.Envelope, sent time.Time) core.Envelope {
	e = clone(e)
	loc := r.opt.Timestamp.Location
	if loc == nil {
		loc = time.UTC
	}
	prefix := sent.In(loc).Format(r.opt.Timestamp.Format)
	if len(e.Parts) > 0 && e.Parts[0].Type == core.PartText {
		e.Parts[0].Text = prefix + e.Parts[0].Text
	} else {
		e.Parts = append([]core.Part{{Type: core.PartText, Text: prefix}}, e.Parts...)
	}
	return e
}

// joins reports whether a busy backend takes e directly: user messages by
// steering or following up, injections by following up.
func (s *state) joins(e core.Envelope) (core.DeliveryMode, bool) {
	if e.Channel && s.caps.Steer {
		return core.DeliverySteer, true
	}
	if s.caps.FollowUp && !(e.Channel && s.policy == BusyInterrupt) {
		return core.DeliveryFollowUp, true
	}
	return 0, false
}

func (r *Router) dispatch(ctx context.Context, s *state, e core.Envelope) error {
	original := clone(e)
	mode := core.DeliveryPrompt
	if s.ready && s.busy {
		var ok bool
		if mode, ok = s.joins(e); !ok {
			return r.hold(ctx, s, e)
		}
	}
	if !s.ready {
		return r.hold(ctx, s, e)
	}
	for _, h := range r.opt.Hooks {
		next, drop, err := h.OnInbound(ctx, clone(e))
		if err != nil {
			slog.Error("inbound hook failed", "error", err)
			continue
		}
		if drop {
			return nil
		}
		e = next
	}
	result, err := call(ctx, func(ctx context.Context) (core.Submission, error) { return s.backend.Prompt(ctx, e, mode) })
	if err != nil {
		if errors.Is(err, core.ErrUnavailable) {
			s.ready = false
			return r.dispatch(ctx, s, original)
		}
		return err
	}
	if !result.Handled {
		s.busy = true
	}
	if e.Channel {
		copy := original
		s.last = &copy
	}
	return nil
}

// hold queues e in the router until the backend is ready and idle. Under
// BusyInterrupt a user message first cancels the running turn.
func (r *Router) hold(ctx context.Context, s *state, e core.Envelope) error {
	if len(s.queue) >= r.opt.QueueLimit {
		return errors.New("backend queue is full")
	}
	interrupt := s.ready && e.Channel && s.policy == BusyInterrupt && !s.draining
	if interrupt && !s.cancelling {
		if err := action(ctx, s.backend.Cancel); err != nil {
			return err
		}
		s.cancelling = true
	}
	s.queue = append(s.queue, e)
	if !e.Channel || s.draining {
		return nil
	}
	switch {
	case !s.ready:
		r.system(ctx, s, "Your message is queued ᗜ⩊ᗜ\n\nThe backend is starting or reconnecting. I’ll send it when ready.")
	case interrupt:
		r.system(ctx, s, "Stopping the current reply ᗜ⩊ᗜ\n\nYour message goes next.")
	default:
		r.system(ctx, s, "Your message is queued ᗜ⩊ᗜ\n\nI’ll send it when the current reply finishes.")
	}
	return nil
}

// drain re-dispatches held messages, user messages first. Messages that
// still cannot go out are held again in order.
func (r *Router) drain(ctx context.Context, s *state) {
	q := s.queue
	s.queue = nil
	s.draining = true
	defer func() { s.draining = false }()
	for _, channel := range []bool{true, false} {
		for _, msg := range q {
			if msg.Channel == channel {
				if err := r.dispatch(ctx, s, msg); err != nil {
					r.system(ctx, s, err.Error())
				}
			}
		}
	}
}
func call[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return fn(ctx)
}
func action(ctx context.Context, fn func(context.Context) error) error {
	_, err := call(ctx, func(ctx context.Context) (struct{}, error) { return struct{}{}, fn(ctx) })
	return err
}

// deliver sends to a conversation's current address, attaching its latest
// inbound reference. Platform refusals are reported to hooks, not retried.
func (r *Router) deliver(ctx context.Context, s *state, e core.Envelope) error {
	if e.ReplyRef == "" {
		e.ReplyRef = s.replyRef
	}
	err := r.send(ctx, s.source, e)
	if errors.Is(err, core.ErrSendNotAllowed) {
		r.observe(ctx, s.key, core.Event{Kind: core.EventDeliveryRefused, Text: err.Error()})
	}
	return err
}
func (r *Router) send(ctx context.Context, source string, e core.Envelope) error {
	for _, h := range r.opt.Hooks {
		next, drop, err := h.OnOutbound(ctx, clone(e))
		if err != nil {
			slog.Error("outbound hook failed", "error", err)
			continue
		}
		if drop {
			return nil
		}
		e = next
	}
	f, chat := r.front(source)
	if f == nil {
		return fmt.Errorf("unknown frontend %s", source)
	}
	err := f.Send(ctx, chat, e)
	if errors.Is(err, core.ErrSendNotAllowed) {
		slog.Warn("frontend refused delivery", "source", source, "error", err)
	} else if err != nil {
		slog.Error("frontend send failed", "source", source, "error", err)
	}
	return err
}
func (r *Router) system(ctx context.Context, s *state, text string) {
	if err := r.deliver(ctx, s, core.Envelope{System: true, Parts: []core.Part{{Type: core.PartText, Text: text}}}); err != nil {
		slog.Error("system message delivery failed", "message", text, "error", err)
	}
}
func (r *Router) flush(ctx context.Context, s *state, text string, final bool) {
	for _, bubble := range s.bubbles.Feed(text, final) {
		if err := r.deliver(ctx, s, core.Envelope{Parts: []core.Part{{Type: core.PartText, Text: bubble}}}); err != nil {
			slog.Error("bubble delivery failed", "error", err)
		}
	}
}
func (r *Router) event(ctx context.Context, key string, s *state, e core.Event) {
	r.observe(ctx, key, e)
	r.handle(ctx, s, e)
}

// observe gives each hook its own copy of an event.
func (r *Router) observe(ctx context.Context, key string, e core.Event) {
	for _, h := range r.opt.Hooks {
		copy := e
		copy.Raw = append(json.RawMessage(nil), e.Raw...)
		if e.Tool != nil {
			tool := *e.Tool
			copy.Tool = &tool
		}
		if e.Dialog != nil {
			d := *e.Dialog
			d.Options = append([]core.Option(nil), d.Options...)
			copy.Dialog = &d
		}
		if err := h.OnEvent(ctx, key, copy); err != nil {
			slog.Error("event hook failed", "error", err)
		}
	}
}
func (r *Router) handle(ctx context.Context, s *state, e core.Event) {
	switch e.Kind {
	case core.EventReady:
		s.ready, s.busy, s.cancelling = true, false, false
		r.drain(ctx, s)
	case core.EventRestarting:
		s.ready, s.busy, s.cancelling = false, false, false
		s.models = map[string]core.Model{}
		r.flush(ctx, s, "", true)
		r.system(ctx, s, e.Text)
	case core.EventTurnStarted:
		s.busy = true
	case core.EventTurnEnded:
		s.busy, s.cancelling = false, false
		r.flush(ctx, s, "", true)
		r.drain(ctx, s)
	case core.EventMessageChunk:
		r.flush(ctx, s, e.Text, false)
	case core.EventMessageEnd:
		r.flush(ctx, s, "", true)
	case core.EventNotice, core.EventError:
		r.system(ctx, s, e.Text)
	case core.EventDialog:
		if e.Dialog == nil {
			return
		}
		f, chat := r.front(s.source)
		if f != nil {
			if err := f.Dialog(ctx, chat, *e.Dialog); err != nil {
				r.system(ctx, s, err.Error())
				_ = r.reply(ctx, s, core.Reply{ID: e.Dialog.ID, Cancelled: true})
			}
		}
	}
	if r.opt.ShowTools && e.Tool != nil {
		if text := r.toolSummary(*e.Tool); text != "" {
			r.system(ctx, s, text)
		}
	}
}
func (r *Router) reply(ctx context.Context, s *state, reply core.Reply) error {
	if strings.HasPrefix(reply.ID, "model:") {
		if reply.Cancelled {
			return nil
		}
		model, ok := s.models[reply.OptionID]
		if !ok {
			return errors.New("model selection expired")
		}
		return r.setModel(ctx, s, model)
	}
	return action(ctx, func(ctx context.Context) error { return s.backend.Respond(ctx, reply) })
}

// parseModel reads "provider/id" or a bare id.
func parseModel(value string) (core.Model, error) {
	provider, id, ok := strings.Cut(value, "/")
	if !ok {
		provider, id = "", value
	}
	if id == "" || (ok && provider == "") {
		return core.Model{}, errors.New("use /model provider/model-id or /model model-id")
	}
	return core.Model{Provider: provider, ID: id}, nil
}
func (r *Router) setModel(ctx context.Context, s *state, m core.Model) error {
	err := action(ctx, func(ctx context.Context) error { return s.backend.SetModel(ctx, m) })
	if err == nil {
		r.system(ctx, s, "Model updated ₍^ >ヮ<^₎ .ᐟ.ᐟ\n\n"+modelDetails(m))
	}
	return err
}
func (r *Router) command(ctx context.Context, key string, s *state, e core.Envelope) (bool, error) {
	text := strings.TrimSpace(e.Parts[0].Text)
	args := strings.Fields(text)
	if len(args) == 0 {
		return false, nil
	}
	name := strings.Split(args[0], "@")[0]
	switch name {
	case "/status":
		if !s.ready {
			r.system(ctx, s, fmt.Sprintf("Backend reconnecting ᗜ⩊ᗜ\n\nQueued messages: %d", len(s.queue)))
			return true, nil
		}
		st, err := call(ctx, s.backend.Status)
		if err != nil {
			return true, err
		}
		r.system(ctx, s, fmt.Sprintf("Current session 𑄝\n\n%s\nActivity: %s\nQueued messages: %d", sessionDetails(st), activity(s.busy), len(s.queue)+st.PendingMessages))
		return true, nil
	case "/restart":
		if s.ready {
			_, err := call(ctx, s.backend.Status)
			if err != nil {
				return true, err
			}
			s.ready = false
			return true, s.backend.Restart(ctx)
		}
		r.system(ctx, s, "Backend is already restarting.")
		return true, nil
	case "/retry":
		if s.last == nil {
			return true, errors.New("no user message to retry")
		}
		copy := *s.last
		copy.Source = e.Source
		return true, r.dispatch(ctx, s, copy)
	case "/abort", "/new", "/model":
		if !s.ready {
			return true, errors.New("backend restarting; command unavailable")
		}
		if (name == "/new" && !s.caps.NewSession) || (name == "/model" && !s.caps.Models) {
			return true, fmt.Errorf("%s: %w", name, core.ErrUnsupported)
		}
	default:
		return false, nil
	}
	switch name {
	case "/abort":
		s.queue = nil
		err := action(ctx, s.backend.Cancel)
		if err == nil {
			r.system(ctx, s, "Generation stopped ᗜ⩊ᗜ\n\nPending messages have been cleared.")
		}
		return true, err
	case "/new":
		created, err := call(ctx, s.backend.NewSession)
		if err != nil {
			return true, err
		}
		if !created {
			r.system(ctx, s, "New session cancelled.")
			return true, nil
		}
		s.last = nil
		s.busy, s.cancelling = false, false
		s.models = map[string]core.Model{}
		st, statusErr := call(ctx, s.backend.Status)
		if statusErr != nil {
			r.system(ctx, s, "New session started ₍^ >ヮ<^₎ .ᐟ.ᐟ\n\nSession details are temporarily unavailable. Try /status.")
			return true, nil
		}
		r.system(ctx, s, "New session started ₍^ >ヮ<^₎ .ᐟ.ᐟ\n\n"+sessionDetails(st))
		return true, nil
	case "/model":
		if len(args) > 1 {
			m, err := parseModel(args[1])
			if err != nil {
				return true, err
			}
			return true, r.setModel(ctx, s, m)
		}
		models, err := call(ctx, s.backend.Models)
		if err != nil {
			return true, err
		}
		d := core.Dialog{ID: "model:" + key, Kind: core.DialogSelect, Title: "Choose a model"}
		s.models = map[string]core.Model{}
		for _, m := range models {
			value := m.String()
			d.Options = append(d.Options, core.Option{ID: value, Label: value})
			s.models[value] = m
		}
		if len(d.Options) == 0 {
			return true, errors.New("no available models")
		}
		f, chat := r.front(s.source)
		return true, f.Dialog(ctx, chat, d)
	}
	return true, nil
}

func clone(e core.Envelope) core.Envelope {
	e.Parts = append([]core.Part(nil), e.Parts...)
	if e.Reply != nil {
		reply := *e.Reply
		e.Reply = &reply
	}
	return e
}

// Tool summaries exclude results and incremental updates, and include the
// argument summary only with ToolDetail. Successful completions are not
// posted: the agent's reply already marks the end. Hooks receive full events.
func (r *Router) toolSummary(tool core.ToolCall) string {
	name := clip(tool.Title, 80)
	if name == "" {
		name = "unknown"
	}
	switch tool.Status {
	case core.ToolInProgress:
		text := "Tool started: " + name
		if detail := clip(tool.Detail, r.opt.ToolDetailLimit); r.opt.ToolDetail && detail != "" {
			text += " · " + detail
		}
		return text
	case core.ToolFailed:
		text := "Tool failed: " + name
		if tool.Duration > 0 {
			text += fmt.Sprintf(" (%s)", tool.Duration.Round(time.Millisecond))
		}
		return text
	default:
		return ""
	}
}

// clip collapses whitespace to single spaces and keeps at most limit runes.
func clip(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > limit {
		s = string(runes[:limit]) + "…"
	}
	return s
}

func sessionDetails(st core.BackendState) string {
	display := func(value string) string {
		if value == "" {
			return "Unavailable"
		}
		return value
	}
	if st.Model.Provider == "" && st.Model.ID != "" {
		return fmt.Sprintf("Session: %s\nModel: %s", display(st.SessionID), st.Model.ID)
	}
	return fmt.Sprintf("Session: %s\nProvider: %s\nModel: %s", display(st.SessionID), display(st.Model.Provider), display(st.Model.ID))
}

func modelDetails(m core.Model) string {
	if m.Provider == "" {
		return "Model: " + m.ID
	}
	return "Provider: " + m.Provider + "\nModel: " + m.ID
}
func activity(busy bool) string {
	if busy {
		return "Generating a reply"
	}
	return "Ready for your message"
}
