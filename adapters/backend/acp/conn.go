package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// conn is one running agent process speaking JSON-RPC 2.0 over stdio, one
// message per line. It knows nothing about ACP methods; the backend gives it
// a handler for incoming requests and notifications. It is self-contained so
// it can move to a shared package once a third stdio backend needs it.
type conn struct {
	cmd     *exec.Cmd
	writes  chan writeRequest
	exited  chan struct{} // closed once the process is gone and the reader stopped
	input   chan struct{} // closed to close the agent's stdin
	once    sync.Once
	seq     atomic.Int64
	mu      sync.Mutex
	pending map[int64]func(json.RawMessage, error)
	waitErr error
}

type writeRequest struct {
	ctx    context.Context
	data   []byte
	result chan error
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("agent error %d: %s", e.Code, e.Message) }

// handler receives what the agent sends: requests carry an id the handler
// must answer with reply, notifications have a nil id. It runs on the reader
// goroutine, so it must not wait for responses.
type handler func(id json.RawMessage, method string, params json.RawMessage, raw []byte)

func start(command, env []string, handle handler) (*conn, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = logWriter{}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	c := &conn{cmd: cmd, writes: make(chan writeRequest), exited: make(chan struct{}), input: make(chan struct{}), pending: map[int64]func(json.RawMessage, error){}}
	writerDone := make(chan struct{})
	readerDone := make(chan struct{})
	go func() { defer close(writerDone); c.writeLoop(stdin) }()
	go func() { defer close(readerDone); c.read(stdout, handle) }()
	go func() {
		<-readerDone
		err := cmd.Wait()
		c.mu.Lock()
		c.waitErr = err
		pending := c.pending
		c.pending = map[int64]func(json.RawMessage, error){}
		c.mu.Unlock()
		close(c.exited)
		<-writerDone
		for _, fn := range pending {
			fn(nil, core.ErrUnavailable)
		}
	}()
	return c, nil
}

// One writer per process keeps lines whole. A caller that gives up does not
// close stdin: a record may be half written and the stream must stay framed.
func (c *conn) writeLoop(w io.WriteCloser) {
	defer w.Close()
	for {
		select {
		case <-c.exited:
			return
		case <-c.input:
			return
		case req := <-c.writes:
			if err := req.ctx.Err(); err != nil {
				req.result <- err
				continue
			}
			_, err := w.Write(req.data)
			req.result <- err
			if err != nil {
				return
			}
		}
	}
}

func (c *conn) read(r io.Reader, handle handler) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line, handle)
		}
		if err != nil {
			return
		}
	}
}

func (c *conn) dispatch(line []byte, handle handler) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		slog.Warn("invalid JSON from ACP agent", "error", err)
		return
	}
	if m.Method != "" {
		handle(m.ID, m.Method, m.Params, line)
		return
	}
	var id int64
	if json.Unmarshal(m.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	fn := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if fn == nil {
		return
	}
	if m.Error != nil {
		fn(nil, m.Error)
		return
	}
	fn(m.Result, nil)
}

func (c *conn) write(ctx context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req := writeRequest{ctx: ctx, data: append(raw, '\n'), result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.exited:
		return core.ErrUnavailable
	case c.writes <- req:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.exited:
		return core.ErrUnavailable
	case err := <-req.result:
		return err
	}
}

// send writes a request and calls fn with its outcome, on the reader
// goroutine, or with ErrUnavailable if the process ends first.
func (c *conn) send(ctx context.Context, method string, params any, fn func(json.RawMessage, error)) error {
	id := c.seq.Add(1)
	c.mu.Lock()
	select {
	case <-c.exited:
		c.mu.Unlock()
		return core.ErrUnavailable
	default:
	}
	c.pending[id] = fn
	c.mu.Unlock()
	err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	return err
}

// call is send that waits for the result.
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	if err := c.send(ctx, method, params, func(raw json.RawMessage, err error) { ch <- result{raw, err} }); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err != nil || out == nil {
			return r.err
		}
		return json.Unmarshal(r.raw, out)
	}
}

func (c *conn) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *conn) reply(ctx context.Context, id json.RawMessage, result any) error {
	return c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *conn) fail(ctx context.Context, id json.RawMessage, code int, text string) error {
	return c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: code, Message: text}})
}

func (c *conn) kill() error { return c.cmd.Process.Kill() }

// closeInput closes the agent's stdin, which asks it to exit.
func (c *conn) closeInput() { c.once.Do(func() { close(c.input) }) }

// exitErr is the process exit status; valid once exited is closed.
func (c *conn) exitErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitErr == nil {
		return errors.New("agent exited")
	}
	return c.waitErr
}

type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	slog.Info("acp agent stderr", "text", string(p))
	return len(p), nil
}
