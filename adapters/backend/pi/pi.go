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
}
type response struct {
	Data json.RawMessage
	Err  error
}

func New(o Options) *Backend {
	return &Backend{opt: o, pending: map[string]chan response{}, session: o.SessionFile}
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

func (b *Backend) Submit(ctx context.Context, e core.Envelope, mode core.DeliveryMode) (core.Submission, error) {
	message, images, err := b.prompt(e)
	if err != nil {
		return core.Submission{}, err
	}
	behavior := "followUp"
	switch mode {
	case core.DeliveryStart:
		if e.Channel {
			behavior = "steer"
		}
	case core.DeliveryInterrupt:
		behavior = "steer"
	case core.DeliveryQueue:
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
func (b *Backend) Abort(ctx context.Context) error {
	if _, err := b.rpc(ctx, "clear_queue", nil); err != nil {
		return err
	}
	_, err := b.rpc(ctx, "abort", nil)
	return err
}
func (b *Backend) Respond(ctx context.Context, r core.Reply) error {
	_, err := b.rpc(ctx, "extension_ui_response", map[string]any{"id": r.ID, "value": r.Value, "confirmed": r.Confirmed, "cancelled": r.Cancelled})
	return err
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
				publish(core.Event{Type: "ready"})
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
		publish(core.Event{Type: "restarting", Text: fmt.Sprintf("Backend crashed, restarting (attempt %d): %v", attempt, err)})
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
		emit(core.Event{Type: "error", Text: "Invalid JSON from backend"})
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
						emit(core.Event{Type: "error", Text: fmt.Sprintf("Save session: %v", err)})
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
	e := core.Event{Type: m.Type, Data: append(json.RawMessage(nil), line...)}
	if m.Type == "message_update" && m.AssistantMessageEvent.Type == "text_delta" {
		e.Text = m.AssistantMessageEvent.Delta
	}
	if m.Type == "extension_ui_request" {
		var d struct {
			ID, Method, Title, Message, Prefill string
			Options                             []string
			Timeout                             int64
		}
		if err := json.Unmarshal(line, &d); err != nil {
			emit(core.Event{Type: "error", Text: "Invalid extension UI request"})
			return
		}
		e.Dialog = &core.Dialog{ID: d.ID, Method: d.Method, Title: d.Title, Message: d.Message, Prefill: d.Prefill, Options: d.Options, TimeoutMS: d.Timeout}
	}
	if m.Type == "tool_execution_start" || m.Type == "tool_execution_end" {
		var tool struct {
			ToolName   string
			Args       json.RawMessage
			IsError    bool
			DurationMS float64
		}
		if json.Unmarshal(line, &tool) == nil {
			e.Tool = &core.ToolEvent{Name: tool.ToolName, Phase: core.ToolStarted, Detail: toolDetail(tool.Args)}
			if m.Type == "tool_execution_end" {
				e.Tool = &core.ToolEvent{Name: tool.ToolName, Phase: core.ToolFinished, Failed: tool.IsError, Duration: time.Duration(tool.DurationMS * float64(time.Millisecond))}
			}
		}
	}
	emit(e)
	if m.Type == "message_end" {
		var msg struct{ ErrorMessage string }
		if json.Unmarshal(m.Message, &msg) == nil && msg.ErrorMessage != "" {
			emit(core.Event{Type: "error", Text: msg.ErrorMessage})
		}
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
