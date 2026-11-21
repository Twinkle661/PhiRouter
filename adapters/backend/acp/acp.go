// Package acp implements a backend for any agent speaking the Agent Client
// Protocol (https://agentclientprotocol.com) over stdio, such as
// `opencode acp`. PhiRouter acts as the ACP client: it creates one session,
// sends prompts, and forwards permission requests to the user.
package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the ACP major version this client speaks.
const ProtocolVersion = 1

// Permission policies answer session/request_permission.
const (
	PermissionAsk    = "ask"    // show the request to the user
	PermissionAllow  = "allow"  // pick the agent's allow-once option
	PermissionReject = "reject" // pick the agent's reject-once option
)

// Config is the acp backend's options block in the router configuration.
type Config struct {
	Command []string `yaml:"command"`
	// Cwd is the session's absolute working directory.
	Cwd string            `yaml:"cwd"`
	Env map[string]string `yaml:"env"`
	// Permission is ask, allow or reject.
	Permission string `yaml:"permission"`
	// InlineImageMaxMB caps images sent inline when the agent accepts
	// images; larger images and other files are sent as file links.
	InlineImageMaxMB int64 `yaml:"inline_image_max_mb"`
}

func DefaultConfig() *Config {
	return &Config{Permission: PermissionAsk, InlineImageMaxMB: 5}
}
func (c *Config) Validate() error {
	if len(c.Command) == 0 || c.Command[0] == "" {
		return errors.New("command is required")
	}
	if !filepath.IsAbs(c.Cwd) {
		return errors.New("cwd must be an absolute path")
	}
	switch c.Permission {
	case PermissionAsk, PermissionAllow, PermissionReject:
	default:
		return errors.New("permission must be ask, allow or reject")
	}
	if c.InlineImageMaxMB < 0 || c.InlineImageMaxMB > 5 {
		return errors.New("inline_image_max_mb must be between 0 and 5")
	}
	return nil
}

type Options struct {
	Command    []string
	Env        []string
	Cwd        string
	Permission string
	InlineMax  int64
	// StatePath stores the session ID so restarts resume it with
	// session/load when the agent supports that.
	StatePath string
	Tools     core.ToolServer
	Media     core.MediaStore
	// Retry is the first restart delay; zero means one second.
	Retry time.Duration
}

// FromConfig builds a backend whose state lives in stateDir.
func FromConfig(c *Config, stateDir string, tools core.ToolServer, media core.MediaStore) *Backend {
	var env []string
	for k, v := range c.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return New(Options{Command: c.Command, Env: env, Cwd: c.Cwd, Permission: c.Permission, InlineMax: c.InlineImageMaxMB * 1024 * 1024, StatePath: filepath.Join(stateDir, "session.json"), Tools: tools, Media: media})
}

type Backend struct {
	opt Options

	mu         sync.Mutex
	conn       *conn
	agent      agentCapabilities
	session    string
	model      *configOption
	turn       bool
	loading    bool
	tools      map[string]time.Time       // started tool calls
	permission map[string]json.RawMessage // dialog ID -> request ID
	emit       func(core.Event)           // Run's ordered publisher
}

func New(o Options) *Backend {
	if o.Retry <= 0 {
		o.Retry = time.Second
	}
	if o.Permission == "" {
		o.Permission = PermissionAsk
	}
	return &Backend{opt: o, tools: map[string]time.Time{}, permission: map[string]json.RawMessage{}}
}

// Capabilities are static: ACP has no steering or follow-up queue, so the
// router holds messages while a turn runs. Agents without a model option
// answer Models with ErrUnsupported.
func (b *Backend) Capabilities() core.Capabilities {
	return core.Capabilities{Models: true, NewSession: true}
}

type agentCapabilities struct {
	LoadSession        bool `json:"loadSession"`
	PromptCapabilities struct {
		Image bool `json:"image"`
	} `json:"promptCapabilities"`
	MCPCapabilities struct {
		HTTP bool `json:"http"`
	} `json:"mcpCapabilities"`
}

type selectOption struct {
	Value   string         `json:"value"`
	Name    string         `json:"name"`
	Group   string         `json:"group"`
	Options []selectOption `json:"options"`
}

type configOption struct {
	ID           string          `json:"id"`
	Category     string          `json:"category"`
	Type         string          `json:"type"`
	CurrentValue json.RawMessage `json:"currentValue"`
	Options      []selectOption  `json:"options"`
}

// values flattens grouped select options.
func (o *configOption) values() []string {
	var out []string
	var walk func([]selectOption)
	walk = func(opts []selectOption) {
		for _, opt := range opts {
			if len(opt.Options) > 0 {
				walk(opt.Options)
			} else if opt.Value != "" {
				out = append(out, opt.Value)
			}
		}
	}
	walk(o.Options)
	return out
}
func (o *configOption) current() string {
	var v string
	_ = json.Unmarshal(o.CurrentValue, &v)
	return v
}

// modelOption picks the select option in the model category.
func modelOption(options []configOption) *configOption {
	for i := range options {
		if options[i].Category == "model" && options[i].Type == "select" {
			o := options[i]
			return &o
		}
	}
	return nil
}

func (b *Backend) current() (*conn, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil || b.session == "" {
		return nil, "", core.ErrUnavailable
	}
	return b.conn, b.session, nil
}

func (b *Backend) Run(ctx context.Context, emit func(core.Event)) error {
	events := make(chan core.Event, 1024)
	eventCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case e := <-events:
				emit(e)
			case <-eventCtx.Done():
				return
			}
		}
	}()
	publish := func(e core.Event) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	}
	b.mu.Lock()
	b.emit = publish
	b.mu.Unlock()
	delay, attempt := b.opt.Retry, 0
	for ctx.Err() == nil {
		attempt++
		started := time.Now()
		err := b.runOnce(ctx, publish)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			delay, attempt = b.opt.Retry, 1
		}
		publish(core.Event{Kind: core.EventRestarting, Text: fmt.Sprintf("Backend crashed, restarting (attempt %d): %v", attempt, err)})
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
		delay = min(delay*2, 60*time.Second)
	}
	return nil
}

// runOnce starts the agent, opens the session and waits for the process to
// end. It returns why the process is gone.
func (b *Backend) runOnce(ctx context.Context, publish func(core.Event)) error {
	c, err := start(b.opt.Command, b.opt.Env, func(id json.RawMessage, method string, params json.RawMessage, raw []byte) {
		b.incoming(ctx, id, method, params, raw, publish)
	})
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.conn, b.session, b.turn, b.model = c, "", false, nil
	b.tools = map[string]time.Time{}
	b.permission = map[string]json.RawMessage{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.conn, b.session, b.turn = nil, "", false
		b.mu.Unlock()
	}()
	if err = b.open(ctx, c); err != nil {
		_ = c.kill()
		<-c.exited
		return err
	}
	publish(core.Event{Kind: core.EventReady})
	select {
	case <-c.exited:
		return c.exitErr()
	case <-ctx.Done():
		_ = b.Close(context.Background())
		return nil
	}
}

// open negotiates the protocol and resumes or creates the session.
func (b *Backend) open(ctx context.Context, c *conn) error {
	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var init struct {
		ProtocolVersion   int               `json:"protocolVersion"`
		AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	}
	params := map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "phirouter", "title": "PhiRouter"},
	}
	if err := c.call(initCtx, "initialize", params, &init); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if init.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("agent speaks ACP version %d, want %d", init.ProtocolVersion, ProtocolVersion)
	}
	b.mu.Lock()
	b.agent = init.AgentCapabilities
	b.mu.Unlock()
	if !init.AgentCapabilities.MCPCapabilities.HTTP {
		slog.Warn("ACP agent does not accept HTTP MCP servers; router tools are unavailable to it", "command", b.opt.Command[0])
	}
	if saved := b.saved(); saved != "" && init.AgentCapabilities.LoadSession {
		loadCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		err := b.load(loadCtx, c, saved)
		if err == nil {
			return nil
		}
		slog.Warn("ACP session/load failed; starting a new session", "error", err)
	}
	_, err := b.newSession(initCtx, c)
	return err
}

func (b *Backend) mcpServers() []any {
	b.mu.Lock()
	http := b.agent.MCPCapabilities.HTTP
	b.mu.Unlock()
	if !http || b.opt.Tools.URL == "" {
		return []any{}
	}
	var headers []map[string]string
	h := b.opt.Tools.Headers()
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		headers = append(headers, map[string]string{"name": name, "value": h[name]})
	}
	return []any{map[string]any{"type": "http", "name": "phirouter", "url": b.opt.Tools.URL, "headers": headers}}
}

// load resumes a session. The agent replays its history as session/update
// notifications before answering; those are not new and are dropped.
func (b *Backend) load(ctx context.Context, c *conn, session string) error {
	b.mu.Lock()
	b.loading = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.loading = false
		b.mu.Unlock()
	}()
	var result struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	if err := c.call(ctx, "session/load", map[string]any{"sessionId": session, "cwd": b.opt.Cwd, "mcpServers": b.mcpServers()}, &result); err != nil {
		return err
	}
	b.mu.Lock()
	b.session, b.model = session, modelOption(result.ConfigOptions)
	b.mu.Unlock()
	return nil
}

func (b *Backend) newSession(ctx context.Context, c *conn) (string, error) {
	var result struct {
		SessionID     string         `json:"sessionId"`
		ConfigOptions []configOption `json:"configOptions"`
	}
	if err := c.call(ctx, "session/new", map[string]any{"cwd": b.opt.Cwd, "mcpServers": b.mcpServers()}, &result); err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	if result.SessionID == "" {
		return "", errors.New("session/new: agent returned no session ID")
	}
	b.mu.Lock()
	b.session, b.model, b.turn = result.SessionID, modelOption(result.ConfigOptions), false
	b.mu.Unlock()
	if err := b.persist(result.SessionID); err != nil {
		slog.Error("saving ACP session failed", "error", err)
	}
	return result.SessionID, nil
}

type savedState struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// saved returns the stored session for the configured cwd.
func (b *Backend) saved() string {
	if b.opt.StatePath == "" {
		return ""
	}
	raw, err := os.ReadFile(b.opt.StatePath)
	if err != nil {
		return ""
	}
	var s savedState
	if json.Unmarshal(raw, &s) != nil || s.Cwd != b.opt.Cwd {
		return ""
	}
	return s.SessionID
}
func (b *Backend) persist(session string) error {
	if b.opt.StatePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(b.opt.StatePath), 0700); err != nil {
		return err
	}
	raw, _ := json.Marshal(savedState{SessionID: session, Cwd: b.opt.Cwd})
	tmp := b.opt.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, b.opt.StatePath)
}

// incoming handles requests and notifications from the agent.
func (b *Backend) incoming(ctx context.Context, id json.RawMessage, method string, params json.RawMessage, raw []byte, publish func(core.Event)) {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	switch method {
	case "session/update":
		b.update(params, raw, publish)
	case "session/request_permission":
		b.requestPermission(ctx, c, id, params, raw, publish)
	default:
		if id != nil && c != nil {
			// The client advertises no file system or terminal access.
			go func() { _ = c.fail(ctx, id, -32601, "method not supported by PhiRouter: "+method) }()
			return
		}
		publish(core.Event{Kind: core.EventExtension, Raw: append(json.RawMessage(nil), raw...)})
	}
}

func (b *Backend) update(params json.RawMessage, raw []byte, publish func(core.Event)) {
	var n struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string `json:"sessionUpdate"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			ConfigOptions []configOption  `json:"configOptions"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &n); err != nil {
		publish(core.Event{Kind: core.EventError, Text: "Invalid session update from agent"})
		return
	}
	b.mu.Lock()
	if b.loading || n.SessionID != b.session {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	u := n.Update
	e := core.Event{Kind: core.EventExtension, Raw: append(json.RawMessage(nil), raw...)}
	switch u.Kind {
	case "agent_message_chunk":
		if u.Content.Type == "text" {
			e.Kind, e.Text = core.EventMessageChunk, u.Content.Text
		}
	case "tool_call", "tool_call_update":
		if tool := b.tool(u.ToolCallID, u.Title, u.Status, u.RawInput); tool != nil {
			e.Kind, e.Tool = core.EventToolCall, tool
		}
	case "config_option_update":
		b.mu.Lock()
		if m := modelOption(u.ConfigOptions); m != nil {
			b.model = m
		}
		b.mu.Unlock()
	}
	publish(e)
}

// tool turns ACP tool call progress into at most one start and one end per
// call. Titles from later updates are remembered for the end notice.
func (b *Backend) tool(id, title, status string, input json.RawMessage) *core.ToolCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	startedAt, started := b.tools[id]
	switch status {
	case "", "pending", "in_progress":
		if started {
			return nil
		}
		b.tools[id] = time.Now()
		return &core.ToolCall{ID: id, Title: title, Status: core.ToolInProgress, Detail: detail(input)}
	case "completed", "failed":
		if !started {
			startedAt = time.Now()
		}
		delete(b.tools, id)
		t := &core.ToolCall{ID: id, Title: title, Status: core.ToolCompleted, Duration: time.Since(startedAt)}
		if status == "failed" {
			t.Status = core.ToolFailed
		}
		return t
	}
	return nil
}

// detail summarizes tool input like the pi adapter: well-known fields when
// present, otherwise compact JSON.
func detail(input json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(input, &fields) != nil || len(fields) == 0 {
		return ""
	}
	var parts []string
	for _, key := range []string{"command", "pattern", "path", "url", "query"} {
		if s, ok := fields[key].(string); ok && strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) != nil {
		return ""
	}
	return compact.String()
}

var permissionKinds = map[string]core.OptionKind{
	"allow_once":    core.OptionAllowOnce,
	"allow_always":  core.OptionAllowAlways,
	"reject_once":   core.OptionRejectOnce,
	"reject_always": core.OptionRejectAlways,
}

func (b *Backend) requestPermission(ctx context.Context, c *conn, id, params json.RawMessage, raw []byte, publish func(core.Event)) {
	if id == nil || c == nil {
		return
	}
	var req struct {
		ToolCall struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &req); err != nil || len(req.Options) == 0 {
		go func() { _ = c.fail(ctx, id, -32602, "invalid permission request") }()
		return
	}
	d := core.Dialog{ID: "permission:" + string(id), Title: "Permission requested", Message: req.ToolCall.Title}
	if detail := detail(req.ToolCall.RawInput); detail != "" {
		d.Message = strings.TrimSpace(d.Message + "\n" + detail)
	}
	want := map[string]core.OptionKind{PermissionAllow: core.OptionAllowOnce, PermissionReject: core.OptionRejectOnce}[b.opt.Permission]
	for _, o := range req.Options {
		kind := permissionKinds[o.Kind]
		if b.opt.Permission != PermissionAsk && kind == want {
			go func() { _ = c.reply(ctx, id, selected(o.OptionID)) }()
			return
		}
		d.Options = append(d.Options, core.Option{ID: o.OptionID, Label: o.Name, Kind: kind})
	}
	// Policies fall back to asking when the agent offers no matching option.
	b.mu.Lock()
	b.permission[d.ID] = append(json.RawMessage(nil), id...)
	b.mu.Unlock()
	publish(core.Event{Kind: core.EventDialog, Dialog: &d, Raw: append(json.RawMessage(nil), raw...)})
}

func selected(option string) map[string]any {
	return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option}}
}

var cancelledOutcome = map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}

func (b *Backend) Respond(ctx context.Context, r core.Reply) error {
	b.mu.Lock()
	c, id := b.conn, b.permission[r.ID]
	delete(b.permission, r.ID)
	b.mu.Unlock()
	if id == nil {
		return errors.New("permission request expired")
	}
	if c == nil {
		return core.ErrUnavailable
	}
	if r.Cancelled || r.OptionID == "" {
		return c.reply(ctx, id, cancelledOutcome)
	}
	return c.reply(ctx, id, selected(r.OptionID))
}

var stopReasons = map[string]core.StopReason{
	"end_turn":          core.StopEndTurn,
	"max_tokens":        core.StopMaxTokens,
	"max_turn_requests": core.StopMaxTurnRequests,
	"refusal":           core.StopRefusal,
	"cancelled":         core.StopCancelled,
}

// Prompt sends session/prompt and returns once it is written. The response
// arrives when the turn ends and becomes EventTurnEnded, after all of the
// turn's updates because both come through the reader in order.
func (b *Backend) Prompt(ctx context.Context, e core.Envelope, mode core.DeliveryMode) (core.Submission, error) {
	if mode != core.DeliveryPrompt {
		return core.Submission{}, fmt.Errorf("ACP prompt while busy: %w", core.ErrUnsupported)
	}
	c, session, err := b.current()
	if err != nil {
		return core.Submission{}, err
	}
	blocks, err := b.content(e)
	if err != nil {
		return core.Submission{}, err
	}
	b.mu.Lock()
	if b.turn {
		b.mu.Unlock()
		return core.Submission{}, errors.New("a prompt is already running")
	}
	b.turn = true
	b.mu.Unlock()
	err = c.send(ctx, "session/prompt", map[string]any{"sessionId": session, "prompt": blocks}, func(raw json.RawMessage, err error) {
		b.turnEnded(c, raw, err)
	})
	if err != nil {
		b.mu.Lock()
		b.turn = false
		b.mu.Unlock()
		return core.Submission{}, err
	}
	return core.Submission{}, nil
}

// turnEnded reports a prompt's result. It runs on the reader goroutine.
func (b *Backend) turnEnded(c *conn, raw json.RawMessage, err error) {
	b.mu.Lock()
	if b.conn != c {
		b.mu.Unlock()
		return
	}
	b.turn = false
	emit := b.emit
	b.mu.Unlock()
	if errors.Is(err, core.ErrUnavailable) || emit == nil {
		return // the process is gone; EventRestarting follows
	}
	if err != nil {
		emit(core.Event{Kind: core.EventError, Text: err.Error()})
		emit(core.Event{Kind: core.EventTurnEnded})
		return
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(raw, &result)
	emit(core.Event{Kind: core.EventTurnEnded, StopReason: stopReasons[result.StopReason], Raw: append(json.RawMessage(nil), raw...)})
}

// content maps parts to ACP content blocks: text, inline images when the
// agent accepts them, and file links for everything else.
func (b *Backend) content(e core.Envelope) ([]any, error) {
	b.mu.Lock()
	images := b.agent.PromptCapabilities.Image
	b.mu.Unlock()
	var blocks []any
	for _, p := range e.Parts {
		if p.Type == core.PartText {
			blocks = append(blocks, map[string]string{"type": "text", "text": p.Text})
			continue
		}
		if b.opt.Media == nil {
			return nil, errors.New("attachments need a media store")
		}
		q, err := b.opt.Media.Inspect(p.Path)
		if err != nil {
			return nil, err
		}
		if p.Type == core.PartImage && images && b.opt.InlineMax > 0 && q.Size <= b.opt.InlineMax {
			f, err := b.opt.Media.Open(q.Path)
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(f, b.opt.InlineMax+1))
			f.Close()
			if err != nil {
				return nil, err
			}
			if int64(len(data)) <= b.opt.InlineMax {
				blocks = append(blocks, map[string]string{"type": "image", "mimeType": q.MIME, "data": base64.StdEncoding.EncodeToString(data)})
				continue
			}
		}
		link := map[string]any{"type": "resource_link", "uri": "file://" + q.Path, "name": filepath.Base(q.Path), "size": q.Size}
		if q.MIME != "" {
			link["mimeType"] = q.MIME
		}
		blocks = append(blocks, link)
		if p.Caption != "" {
			blocks = append(blocks, map[string]string{"type": "text", "text": p.Caption})
		}
	}
	return blocks, nil
}

// Cancel sends session/cancel and, as ACP requires, answers open permission
// requests as cancelled. The running prompt then ends with "cancelled".
func (b *Backend) Cancel(ctx context.Context) error {
	c, session, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	open := b.permission
	b.permission = map[string]json.RawMessage{}
	b.mu.Unlock()
	if err = c.notify(ctx, "session/cancel", map[string]string{"sessionId": session}); err != nil {
		return err
	}
	for _, id := range open {
		if err = c.reply(ctx, id, cancelledOutcome); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Status(ctx context.Context) (core.BackendState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil || b.session == "" {
		return core.BackendState{}, core.ErrUnavailable
	}
	st := core.BackendState{SessionID: b.session}
	if b.model != nil {
		st.Model = core.Model{ID: b.model.current()}
	}
	return st, nil
}

// Models lists the values of the agent's model config option. ACP models
// have a single identifier, so Provider is empty.
func (b *Backend) Models(ctx context.Context) ([]core.Model, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil, core.ErrUnavailable
	}
	if b.model == nil {
		return nil, fmt.Errorf("agent offers no model option: %w", core.ErrUnsupported)
	}
	var out []core.Model
	for _, v := range b.model.values() {
		out = append(out, core.Model{ID: v})
	}
	return out, nil
}

func (b *Backend) SetModel(ctx context.Context, m core.Model) error {
	c, session, err := b.current()
	if err != nil {
		return err
	}
	b.mu.Lock()
	option := b.model
	b.mu.Unlock()
	if option == nil {
		return fmt.Errorf("agent offers no model option: %w", core.ErrUnsupported)
	}
	var result struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	if err = c.call(ctx, "session/set_config_option", map[string]any{"sessionId": session, "configId": option.ID, "value": m.String()}, &result); err != nil {
		return err
	}
	if next := modelOption(result.ConfigOptions); next != nil {
		b.mu.Lock()
		b.model = next
		b.mu.Unlock()
	}
	return nil
}

func (b *Backend) NewSession(ctx context.Context) (bool, error) {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		return false, core.ErrUnavailable
	}
	if _, err := b.newSession(ctx, c); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Backend) Restart(ctx context.Context) error {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		return errors.New("backend is already restarting")
	}
	return c.kill()
}

func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		return nil
	}
	c.closeInput()
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return c.kill()
	case <-time.After(8 * time.Second):
		return c.kill()
	}
}
