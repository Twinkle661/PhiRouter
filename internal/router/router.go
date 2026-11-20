package router

import (
	"PhiRouter/internal/core"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type Route struct{ From, To string }
type Options struct {
	Routes        []Route
	DefaultSource string
	Separators    []string
	ShowTools     bool
	QueueLimit    int
	Hooks         []core.Hook
}
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
	backend     core.Backend
	ready, busy bool
	source      string
	queue       []core.Envelope
	last        *core.Envelope
	bubbles     Bubbles
	models      map[string]string
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
	r := &Router{opt: o, frontends: f, states: map[string]*state{}, in: make(chan request, 64), events: make(chan backendEvent, 1024), done: make(chan struct{})}
	for key, backend := range b {
		r.states[key] = &state{backend: backend, source: o.DefaultSource, bubbles: Bubbles{Separators: o.Separators, Limit: 4096}, models: map[string]string{}}
	}
	for _, route := range o.Routes {
		if s := r.states[route.To]; s != nil && !strings.HasSuffix(route.From, ":*") {
			s.source = route.From
		}
	}
	return r
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
					req.result <- r.send(ctx, s.source, req.e)
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
	}
	if e.Reply != nil {
		return r.reply(ctx, s, *e.Reply)
	}
	if e.Channel && len(e.Parts) > 0 && e.Parts[0].Type == "text" {
		handled, err := r.command(ctx, key, s, e)
		if handled {
			return err
		}
	}
	return r.dispatch(ctx, s, e)
}
func (r *Router) dispatch(ctx context.Context, s *state, e core.Envelope) error {
	original := clone(e)
	if !s.ready {
		if len(s.queue) >= r.opt.QueueLimit {
			return errors.New("backend queue is full")
		}
		s.queue = append(s.queue, e)
		if e.Channel {
			r.system(ctx, s, "Backend restarting, message queued.")
		}
		return nil
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
	kind := "prompt"
	if s.busy {
		if e.Channel {
			kind = "steer"
		} else {
			kind = "follow_up"
		}
	}
	fields := map[string]any{"envelope": e}
	for _, p := range e.Parts {
		if p.Type == "text" && strings.HasPrefix(p.Text, "/") && e.Channel {
			kind = "prompt"
			if s.busy {
				fields["streamingBehavior"] = "steer"
			}
			break
		}
	}
	data, err := r.call(ctx, s, core.Command{Type: kind, Fields: fields})
	if err != nil {
		if errors.Is(err, core.ErrUnavailable) {
			s.ready = false
			return r.dispatch(ctx, s, original)
		}
		return err
	}
	var result struct{ Disposition string }
	_ = json.Unmarshal(data, &result)
	if result.Disposition != "handled" {
		s.busy = true
	}
	if e.Channel {
		copy := original
		s.last = &copy
	}
	return nil
}
func (r *Router) call(ctx context.Context, s *state, c core.Command) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return s.backend.Call(ctx, c)
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
	if err != nil {
		slog.Error("frontend send failed", "source", source, "error", err)
	}
	return err
}
func (r *Router) system(ctx context.Context, s *state, text string) {
	if err := r.send(ctx, s.source, core.Envelope{Parts: []core.Part{{Type: "text", Text: "[system] " + text}}}); err != nil {
		slog.Error("system message delivery failed", "message", text, "error", err)
	}
}
func (r *Router) flush(ctx context.Context, s *state, text string, final bool) {
	for _, bubble := range s.bubbles.Feed(text, final) {
		if err := r.send(ctx, s.source, core.Envelope{Parts: []core.Part{{Type: "text", Text: bubble}}}); err != nil {
			slog.Error("bubble delivery failed", "error", err)
		}
	}
}
func (r *Router) event(ctx context.Context, key string, s *state, e core.Event) {
	for _, h := range r.opt.Hooks {
		copy := e
		copy.Data = append(json.RawMessage(nil), e.Data...)
		if e.Dialog != nil {
			d := *e.Dialog
			d.Options = append([]string(nil), d.Options...)
			copy.Dialog = &d
		}
		if err := h.OnEvent(ctx, key, copy); err != nil {
			slog.Error("event hook failed", "error", err)
		}
	}
	switch e.Type {
	case "ready":
		s.ready = true
		s.busy = false
		q := s.queue
		s.queue = nil
		for _, channel := range []bool{true, false} {
			for _, msg := range q {
				if msg.Channel == channel {
					if err := r.dispatch(ctx, s, msg); err != nil {
						r.system(ctx, s, err.Error())
					}
				}
			}
		}
	case "restarting":
		s.ready = false
		s.busy = false
		s.models = map[string]string{}
		r.flush(ctx, s, "", true)
		r.system(ctx, s, e.Text)
	case "agent_start":
		s.busy = true
		if _, err := r.call(ctx, s, core.Command{Type: "get_state"}); err != nil {
			r.system(ctx, s, err.Error())
		}
	case "agent_settled":
		s.busy = false
		r.flush(ctx, s, "", true)
		if _, err := r.call(ctx, s, core.Command{Type: "get_state"}); err != nil {
			r.system(ctx, s, err.Error())
		}
	case "message_update":
		r.flush(ctx, s, e.Text, false)
	case "message_end", "agent_end":
		r.flush(ctx, s, "", true)
	case "error":
		r.system(ctx, s, e.Text)
	case "extension_ui_request":
		if e.Dialog == nil {
			return
		}
		d := *e.Dialog
		switch d.Method {
		case "notify":
			r.system(ctx, s, d.Message)
		case "select", "confirm", "input", "editor":
			f, chat := r.front(s.source)
			if f != nil {
				if err := f.Dialog(ctx, chat, d); err != nil {
					r.system(ctx, s, err.Error())
					_ = r.reply(ctx, s, core.Reply{ID: d.ID, Cancelled: true})
				}
			}
		default:
			slog.Debug("extension UI", "method", d.Method)
		}
	default:
		if r.opt.ShowTools && strings.HasPrefix(e.Type, "tool_execution_") {
			r.system(ctx, s, string(e.Data))
		}
	}
}
func (r *Router) reply(ctx context.Context, s *state, reply core.Reply) error {
	if strings.HasPrefix(reply.ID, "model:") {
		if reply.Cancelled {
			return nil
		}
		value, ok := s.models[reply.Value]
		if !ok {
			return errors.New("model selection expired")
		}
		return r.setModel(ctx, s, value)
	}
	_, err := r.call(ctx, s, core.Command{Type: "extension_ui_response", Fields: map[string]any{"id": reply.ID, "value": reply.Value, "confirmed": reply.Confirmed, "cancelled": reply.Cancelled}})
	return err
}
func (r *Router) setModel(ctx context.Context, s *state, value string) error {
	provider, model, ok := strings.Cut(value, "/")
	if !ok || provider == "" || model == "" {
		return errors.New("use /model provider/model-id")
	}
	_, err := r.call(ctx, s, core.Command{Type: "set_model", Fields: map[string]any{"provider": provider, "modelId": model}})
	if err == nil {
		r.system(ctx, s, "Model selected: "+value)
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
			r.system(ctx, s, fmt.Sprintf("Backend restarting. Queue: %d.", len(s.queue)))
			return true, nil
		}
		data, err := r.call(ctx, s, core.Command{Type: "get_state"})
		if err != nil {
			return true, err
		}
		var st struct {
			Model               struct{ Provider, ID string }
			PendingMessageCount int
		}
		_ = json.Unmarshal(data, &st)
		r.system(ctx, s, fmt.Sprintf("Backend ready. Busy: %t. Model: %s/%s. Queue: %d.", s.busy, st.Model.Provider, st.Model.ID, len(s.queue)+st.PendingMessageCount))
		return true, nil
	case "/restart":
		if s.ready {
			_, err := r.call(ctx, s, core.Command{Type: "get_state"})
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
	default:
		return false, nil
	}
	switch name {
	case "/abort":
		s.queue = nil
		if _, err := r.call(ctx, s, core.Command{Type: "clear_queue"}); err != nil {
			return true, err
		}
		_, err := r.call(ctx, s, core.Command{Type: "abort"})
		return true, err
	case "/new":
		data, err := r.call(ctx, s, core.Command{Type: "new_session"})
		if err != nil {
			return true, err
		}
		var result struct{ Cancelled bool }
		_ = json.Unmarshal(data, &result)
		if result.Cancelled {
			r.system(ctx, s, "New session cancelled.")
			return true, nil
		}
		s.last = nil
		s.busy = false
		s.models = map[string]string{}
		_, err = r.call(ctx, s, core.Command{Type: "get_state"})
		if err == nil {
			r.system(ctx, s, "New session started.")
		}
		return true, err
	case "/model":
		if len(args) > 1 {
			return true, r.setModel(ctx, s, args[1])
		}
		data, err := r.call(ctx, s, core.Command{Type: "get_available_models"})
		if err != nil {
			return true, err
		}
		var result struct {
			Models []struct{ Provider, ID string }
		}
		if err = json.Unmarshal(data, &result); err != nil {
			return true, err
		}
		d := core.Dialog{ID: "model:" + key, Method: "select", Title: "Choose a model"}
		s.models = map[string]string{}
		for _, m := range result.Models {
			value := m.Provider + "/" + m.ID
			d.Options = append(d.Options, value)
			s.models[value] = value
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
