// Package pi implements the pi 1.6 RPC protocol and subprocess supervision.
package pi

import (
	"bufio"
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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is the pi backend's options block in the router configuration.
type Config struct {
	Command []string `yaml:"command"`
	// SessionFile pins a pi session file; empty resumes the last session.
	SessionFile string `yaml:"session_file"`
	// AgentDir is pi's agent directory. Empty means ~/.pi/agent; anything
	// else is also passed to pi as PI_CODING_AGENT_DIR.
	AgentDir string `yaml:"agent_dir"`
	// Attachment is the text line describing each attached file.
	Attachment string `yaml:"attachment"`
	// InlineImageMaxMB caps images sent inline to the model; 0 disables.
	InlineImageMaxMB int64 `yaml:"inline_image_max_mb"`
}

func DefaultConfig() *Config {
	return &Config{Command: []string{"pi", "--mode", "rpc"}, Attachment: "[attachment: {path} ({mime}, {size})]", InlineImageMaxMB: 5}
}
func (c *Config) Validate() error {
	if len(c.Command) == 0 || c.Command[0] == "" {
		return errors.New("command is required")
	}
	if c.InlineImageMaxMB < 0 || c.InlineImageMaxMB > 5 {
		return errors.New("inline_image_max_mb must be between 0 and 5")
	}
	return nil
}

// ResolveAgentDir returns the agent directory pi will use for c.
func (c *Config) ResolveAgentDir() (string, error) {
	if c.AgentDir != "" {
		return c.AgentDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// FromConfig builds a backend whose session pointer lives in stateDir. It
// registers the router's tool server in the agent directory's mcp.json (read
// by pi-mcp-adapter); the token reaches pi through the environment, never
// the file.
func FromConfig(c *Config, stateDir string, tools core.ToolServer, media core.MediaStore) (*Backend, error) {
	dir, err := c.ResolveAgentDir()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = RegisterMCP(filepath.Join(dir, "mcp.json"), tools.URL); err != nil {
		return nil, err
	}
	env := []string{"ROUTER_MCP__TOKEN=" + tools.Token, "PHIROUTER_BACKEND=" + tools.Route}
	if c.AgentDir != "" {
		env = append(env, "PI_CODING_AGENT_DIR="+c.AgentDir)
	}
	return New(Options{Command: c.Command, Env: env, StatePath: filepath.Join(stateDir, "session"), SessionFile: c.SessionFile, Attachment: c.Attachment, InlineMax: c.InlineImageMaxMB * 1024 * 1024, Media: media}), nil
}

// RegisterMCP adds the phirouter entry to an mcp.json, keeping other entries.
func RegisterMCP(path, endpoint string) error {
	config := map[string]any{}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(raw, &config); err != nil {
			return fmt.Errorf("invalid existing mcp.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if config == nil {
		return fmt.Errorf("mcp.json must contain an object")
	}
	servers, ok := config["mcpServers"].(map[string]any)
	if !ok && config["mcpServers"] != nil {
		return fmt.Errorf("mcpServers must be an object")
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers["phirouter"] = map[string]any{"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${ROUTER_MCP__TOKEN}", core.RouteHeader: "${PHIROUTER_BACKEND}"}}
	config["mcpServers"] = servers
	raw, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".phirouter.tmp"
	if err = os.WriteFile(tmp, append(raw, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type Options struct {
	Command                            []string
	Env                                []string
	StatePath, SessionFile, Attachment string
	InlineMax                          int64
	Media                              core.MediaStore
}
type Backend struct {
	opt     Options
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writes  chan writeRequest
	pending map[string]chan response
	seq     atomic.Uint64
	session string
	ready   bool
	exited  chan struct{}
	// dialogs remembers the pi method of each open dialog, to answer in kind.
	dialogs map[string]string
}
type response struct {
	Data json.RawMessage
	Err  error
}

func New(o Options) *Backend {
	return &Backend{opt: o, pending: map[string]chan response{}, session: o.SessionFile, dialogs: map[string]string{}}
}

type writeRequest struct {
	ctx    context.Context
	data   []byte
	result chan error
}

// One writer per process preserves JSONL framing. Cancelling a caller does not
// close stdin: an in-flight record may still finish, and its result is uncertain.
func writeLoop(w io.WriteCloser, queue <-chan writeRequest, done <-chan struct{}) {
	defer w.Close()
	for {
		select {
		case <-done:
			return
		case req := <-queue:
			if err := req.ctx.Err(); err != nil {
				req.result <- err
				continue
			}
			n, err := w.Write(req.data)
			if err == nil && n != len(req.data) {
				err = io.ErrShortWrite
			}
			req.result <- err
			if err != nil {
				return
			}
		}
	}
}

// rpc is private to the pi adapter; wire command names never cross core.
func (b *Backend) rpc(ctx context.Context, kind string, fields map[string]any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record := map[string]any{"type": kind}
	for k, v := range fields {
		record[k] = v
	}
	id := fmt.Sprintf("r%d", b.seq.Add(1))
	if kind != "extension_ui_response" {
		record["id"] = id
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	ch := make(chan response, 1)
	b.mu.Lock()
	queue, done := b.writes, b.exited
	if b.stdin == nil {
		b.mu.Unlock()
		return nil, core.ErrUnavailable
	}
	if kind != "extension_ui_response" {
		b.pending[id] = ch
	}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, id); b.mu.Unlock() }()
	req := writeRequest{ctx: ctx, data: append(raw, '\n'), result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return nil, core.ErrUnavailable
	case queue <- req:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return nil, core.ErrUnavailable
	case err = <-req.result:
		if err != nil {
			return nil, fmt.Errorf("write RPC: %w", err)
		}
	}
	if kind == "extension_ui_response" {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.Data, r.Err
	}
}

// Capabilities: pi steers running turns and queues follow-ups itself.
func (b *Backend) Capabilities() core.Capabilities {
	return core.Capabilities{Steer: true, FollowUp: true, Models: true, NewSession: true}
}
func (b *Backend) Prompt(ctx context.Context, e core.Envelope, mode core.DeliveryMode) (core.Submission, error) {
	message, images, err := b.prompt(e)
	if err != nil {
		return core.Submission{}, err
	}
	behavior := "followUp"
	switch mode {
	case core.DeliveryPrompt:
		// pi ignores the behavior when idle; steer keeps a racing user
		// message ahead of queued injections.
		if e.Channel {
			behavior = "steer"
		}
	case core.DeliverySteer:
		behavior = "steer"
	case core.DeliveryFollowUp:
		if strings.HasPrefix(strings.TrimSpace(message), "/") {
			return core.Submission{}, errors.New("plugin slash commands cannot be queued while the backend is busy")
		}
	default:
		return core.Submission{}, errors.New("unsupported delivery mode")
	}
	fields := map[string]any{"message": message, "streamingBehavior": behavior}
	if len(images) > 0 {
		fields["images"] = images
	}
	data, err := b.rpc(ctx, "prompt", fields)
	if err != nil {
		return core.Submission{}, err
	}
	var result struct{ Disposition string }
	if err = json.Unmarshal(data, &result); err != nil {
		return core.Submission{}, err
	}
	return core.Submission{Handled: result.Disposition == "handled"}, nil
}
func (b *Backend) Status(ctx context.Context) (core.BackendState, error) {
	data, err := b.rpc(ctx, "get_state", nil)
	if err != nil {
		return core.BackendState{}, err
	}
	var result struct {
		Model               core.Model
		PendingMessageCount int
		SessionID           string `json:"sessionId"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return core.BackendState{}, err
	}
	return core.BackendState{SessionID: result.SessionID, Model: result.Model, PendingMessages: result.PendingMessageCount}, nil
}
func (b *Backend) Models(ctx context.Context) ([]core.Model, error) {
	data, err := b.rpc(ctx, "get_available_models", nil)
	if err != nil {
		return nil, err
	}
	var result struct{ Models []core.Model }
	err = json.Unmarshal(data, &result)
	return result.Models, err
}
func (b *Backend) SetModel(ctx context.Context, m core.Model) error {
	if m.Provider == "" || m.ID == "" {
		return errors.New("use /model provider/model-id")
	}
	_, err := b.rpc(ctx, "set_model", map[string]any{"provider": m.Provider, "modelId": m.ID})
	return err
}
func (b *Backend) NewSession(ctx context.Context) (bool, error) {
	data, err := b.rpc(ctx, "new_session", nil)
	if err != nil {
		return false, err
	}
	var result struct{ Cancelled bool }
	if err = json.Unmarshal(data, &result); err != nil {
		return false, err
	}
	return !result.Cancelled, nil
}
func (b *Backend) Cancel(ctx context.Context) error {
	if _, err := b.rpc(ctx, "clear_queue", nil); err != nil {
		return err
	}
	_, err := b.rpc(ctx, "abort", nil)
	return err
}

// Respond answers an extension UI request in the shape its method expects.
func (b *Backend) Respond(ctx context.Context, r core.Reply) error {
	b.mu.Lock()
	method := b.dialogs[r.ID]
	delete(b.dialogs, r.ID)
	b.mu.Unlock()
	fields := map[string]any{"id": r.ID, "cancelled": r.Cancelled}
	switch method {
	case "confirm":
		fields["confirmed"] = !r.Cancelled && r.OptionID == confirmYes
	case "input", "editor":
		fields["value"] = r.Text
	default:
		fields["value"] = r.OptionID
	}
	_, err := b.rpc(ctx, "extension_ui_response", fields)
	return err
}

const confirmYes, confirmNo = "yes", "no"

// dialog translates a pi extension UI request. Option IDs of select dialogs
// are pi's option strings, so the answer is the chosen string.
func dialog(id, method, title, message, prefill string, options []string, timeoutMS int64) core.Dialog {
	d := core.Dialog{ID: id, Title: title, Message: message, Prefill: prefill, Timeout: time.Duration(timeoutMS) * time.Millisecond}
	switch method {
	case "confirm":
		d.Options = []core.Option{{ID: confirmYes, Label: "Yes", Kind: core.OptionAllowOnce}, {ID: confirmNo, Label: "No", Kind: core.OptionRejectOnce}}
	case "input", "editor":
		d.Kind = core.DialogInput
		d.Multiline = method == "editor"
	default:
		for _, o := range options {
			d.Options = append(d.Options, core.Option{ID: o, Label: o})
		}
	}
	return d
}

func (b *Backend) prompt(e core.Envelope) (string, []map[string]string, error) {
	var text []string
	var images []map[string]string
	for _, p := range e.Parts {
		if p.Type == core.PartText {
			text = append(text, p.Text)
			continue
		}
		q, err := b.opt.Media.Inspect(p.Path)
		if err != nil {
			return "", nil, err
		}
		line := strings.NewReplacer("{path}", q.Path, "{mime}", q.MIME, "{size}", fmt.Sprintf("%.1f MB", float64(q.Size)/1e6)).Replace(b.opt.Attachment)
		text = append(text, line)
		if p.Type == core.PartImage && q.Size <= b.opt.InlineMax && b.opt.InlineMax > 0 {
			f, err := b.opt.Media.Open(q.Path)
			if err != nil {
				return "", nil, err
			}
			data, err := io.ReadAll(io.LimitReader(f, b.opt.InlineMax+1))
			f.Close()
			if err != nil {
				return "", nil, err
			}
			if int64(len(data)) <= b.opt.InlineMax {
				images = append(images, map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": q.MIME})
			}
		}
	}
	return strings.Join(text, "\n"), images, nil
}
func (b *Backend) Run(ctx context.Context, emit func(core.Event)) error {
	if b.session == "" && b.opt.StatePath != "" {
		data, err := os.ReadFile(b.opt.StatePath)
		if err == nil {
			b.session = strings.TrimSpace(string(data))
		} else if !os.IsNotExist(err) {
			return err
		}
	}
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
	delay := time.Second
	attempt := 0
	for ctx.Err() == nil {
		attempt++
		started := time.Now()
		cmd := exec.Command(b.opt.Command[0], b.opt.Command[1:]...)
		cmd.Env = append(os.Environ(), b.opt.Env...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = &logWriter{}
		err = cmd.Start()
		if err == nil {
			b.mu.Lock()
			b.cmd = cmd
			b.stdin = stdin
			b.exited = make(chan struct{})
			b.writes = make(chan writeRequest)
			writerDone := make(chan struct{})
			go func(queue <-chan writeRequest, done <-chan struct{}) {
				defer close(writerDone)
				writeLoop(stdin, queue, done)
			}(b.writes, b.exited)
			b.mu.Unlock()
			readDone := make(chan struct{})
			go func() { defer close(readDone); b.read(stdout, publish) }()
			wait := make(chan error, 1)
			go func() {
				<-readDone
				err := cmd.Wait()
				b.mu.Lock()
				close(b.exited)
				b.mu.Unlock()
				<-writerDone
				wait <- err
			}()
			callCtx := ctx // Session restoration may legitimately wait for extension UI.
			b.mu.Lock()
			session := b.session
			b.mu.Unlock()
			if session != "" {
				var data json.RawMessage
				data, err = b.rpc(callCtx, "switch_session", map[string]any{"sessionPath": session})
				if err == nil {
					var result struct{ Cancelled bool }
					_ = json.Unmarshal(data, &result)
					if result.Cancelled {
						err = errors.New("session restoration cancelled")
					}
				}
			}
			if err == nil {
				_, err = b.rpc(callCtx, "get_state", nil)
			}
			if err == nil {
				b.mu.Lock()
				b.ready = true
				b.mu.Unlock()
				publish(core.Event{Kind: core.EventReady})
				select {
				case err = <-wait:
				case <-ctx.Done():
					shutdownCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
					_ = b.Close(shutdownCtx)
					c()
					<-wait
					return nil
				}
			} else {
				_ = cmd.Process.Kill()
				<-wait
			}
			b.mu.Lock()
			b.stdin = nil
			b.cmd = nil
			b.ready = false
			b.dialogs = map[string]string{} // unanswered requests died with the process
			for _, ch := range b.pending {
				select {
				case ch <- response{Err: core.ErrUnavailable}:
				default:
				}
			}
			b.mu.Unlock()
		} else {
			_ = stdin.Close()
			_ = stdout.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
			attempt = 1
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
func (b *Backend) read(r io.Reader, emit func(core.Event)) {
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.stdin = nil
		for _, ch := range b.pending {
			select {
			case ch <- response{Err: core.ErrUnavailable}:
			default:
			}
		}
	}()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			b.record(line, emit)
		}
		if err != nil {
			return
		}
	}
}
func (b *Backend) record(line []byte, emit func(core.Event)) {
	var m struct {
		Type, ID, Command     string
		Success               bool
		Error                 string
		Data                  json.RawMessage
		AssistantMessageEvent struct{ Type, Delta string }
		Message               json.RawMessage
	}
	if err := json.Unmarshal(line, &m); err != nil {
		emit(core.Event{Kind: core.EventError, Text: "Invalid JSON from backend"})
		return
	}
	if m.Type == "response" {
		if m.Success && m.Command == "get_state" {
			var state struct{ SessionFile string }
			if json.Unmarshal(m.Data, &state) == nil && state.SessionFile != "" {
				b.mu.Lock()
				b.session = state.SessionFile
				b.mu.Unlock()
				if b.opt.StatePath != "" {
					if err := b.persist(state.SessionFile); err != nil {
						emit(core.Event{Kind: core.EventError, Text: fmt.Sprintf("Save session: %v", err)})
					}
				}
			}
		}
		b.mu.Lock()
		ch := b.pending[m.ID]
		b.mu.Unlock()
		if ch != nil {
			r := response{Data: m.Data}
			if !m.Success {
				r.Err = errors.New(m.Error)
			}
			select {
			case ch <- r:
			default:
			}
		}
		return
	}
	// Each pi record becomes one event; records without router meaning are
	// passed through as extensions so hooks still observe them.
	e := core.Event{Kind: core.EventExtension, Raw: append(json.RawMessage(nil), line...)}
	switch m.Type {
	case "agent_start":
		e.Kind = core.EventTurnStarted
		go b.refreshState(emit)
	case "agent_settled":
		e.Kind, e.StopReason = core.EventTurnEnded, core.StopEndTurn
		go b.refreshState(emit)
	case "message_update":
		if m.AssistantMessageEvent.Type == "text_delta" {
			e.Kind, e.Text = core.EventMessageChunk, m.AssistantMessageEvent.Delta
		}
	case "message_end", "agent_end":
		e.Kind = core.EventMessageEnd
	case "extension_ui_request":
		var d struct {
			ID, Method, Title, Message, Prefill string
			Options                             []string
			Timeout                             int64
		}
		if err := json.Unmarshal(line, &d); err != nil {
			emit(core.Event{Kind: core.EventError, Text: "Invalid extension UI request"})
			return
		}
		switch d.Method {
		case "notify":
			e.Kind, e.Text = core.EventNotice, d.Message
		case "select", "confirm", "input", "editor":
			dlg := dialog(d.ID, d.Method, d.Title, d.Message, d.Prefill, d.Options, d.Timeout)
			b.mu.Lock()
			b.dialogs[d.ID] = d.Method
			b.mu.Unlock()
			e.Kind, e.Dialog = core.EventDialog, &dlg
		}
	case "tool_execution_start", "tool_execution_end":
		var tool struct {
			ToolCallID string
			ToolName   string
			Args       json.RawMessage
			IsError    bool
			DurationMS float64
		}
		if json.Unmarshal(line, &tool) == nil {
			e.Kind = core.EventToolCall
			e.Tool = &core.ToolCall{ID: tool.ToolCallID, Title: tool.ToolName, Status: core.ToolInProgress, Detail: toolDetail(tool.Args)}
			if m.Type == "tool_execution_end" {
				status := core.ToolCompleted
				if tool.IsError {
					status = core.ToolFailed
				}
				e.Tool = &core.ToolCall{ID: tool.ToolCallID, Title: tool.ToolName, Status: status, Duration: time.Duration(tool.DurationMS * float64(time.Millisecond))}
			}
		}
	}
	emit(e)
	if m.Type == "message_end" {
		var msg struct{ ErrorMessage string }
		if json.Unmarshal(m.Message, &msg) == nil && msg.ErrorMessage != "" {
			emit(core.Event{Kind: core.EventError, Text: msg.ErrorMessage})
		}
	}
}

// refreshState asks pi for its state after turn boundaries, which also
// persists a newly created session file. It runs off the reader goroutine
// because the response arrives through it.
func (b *Backend) refreshState(emit func(core.Event)) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := b.Status(ctx); err != nil && !errors.Is(err, core.ErrUnavailable) {
		emit(core.Event{Kind: core.EventError, Text: err.Error()})
	}
}

// toolDetail summarizes tool arguments: the well-known fields of pi's
// built-in tools (bash, read, edit, write, grep, find, ls) when present,
// otherwise the compact JSON object. Length limits are the router's concern.
func toolDetail(args json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(args, &fields) != nil || len(fields) == 0 {
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
	if json.Compact(&compact, args) != nil {
		return ""
	}
	return compact.String()
}
func (b *Backend) persist(session string) error {
	if err := os.MkdirAll(filepath.Dir(b.opt.StatePath), 0700); err != nil {
		return err
	}
	tmp := b.opt.StatePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(session+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, b.opt.StatePath)
}
func (b *Backend) Restart(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return errors.New("backend is already restarting")
	}
	return b.cmd.Process.Kill()
}
func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	w, cmd, done := b.stdin, b.cmd, b.exited
	b.mu.Unlock()
	if cmd == nil {
		return nil
	}
	if w != nil {
		_ = w.Close()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return cmd.Process.Kill()
	}
}

type logWriter struct{}

func (*logWriter) Write(p []byte) (int, error) {
	slog.Debug("pi stderr", "text", string(p))
	return len(p), nil
}
