// Package core contains transport-independent contracts.
package core

import (
	"context"
	"encoding/json"
	"errors"
)

type Part struct {
	Type    string `json:"type" yaml:"type"`
	Text    string `json:"text,omitempty" yaml:"text,omitempty"`
	Path    string `json:"path,omitempty" yaml:"path,omitempty"`
	MIME    string `json:"mime,omitempty" yaml:"mime,omitempty"`
	Size    int64  `json:"size,omitempty" yaml:"size,omitempty"`
	Caption string `json:"caption,omitempty" yaml:"caption,omitempty"`
}
type Envelope struct {
	Route   string `json:"route,omitempty"`
	Source  string `json:"-"`
	Channel bool   `json:"-"`
	Parts   []Part `json:"parts"`
	Reply   *Reply `json:"-"`
}
type Reply struct {
	ID, Value            string
	Confirmed, Cancelled bool
}
type Dialog struct {
	ID, Method, Title, Message, Prefill string
	Options                             []string
	TimeoutMS                           int64
}
type Event struct {
	Type, Text string
	Data       json.RawMessage
	Dialog     *Dialog
}
type Command struct {
	Type   string
	Fields map[string]any
}
type Frontend interface {
	Run(context.Context, func(context.Context, Envelope) error) error
	Send(context.Context, string, Envelope) error
	Typing(context.Context, string) error
	Dialog(context.Context, string, Dialog) error
}
type Backend interface {
	Run(context.Context, func(Event)) error
	Call(context.Context, Command) (json.RawMessage, error)
	Restart(context.Context) error
	Close(context.Context) error
}
type Inject func(context.Context, Envelope) error
type Hook interface {
	OnInbound(context.Context, Envelope) (Envelope, bool, error)
	OnOutbound(context.Context, Envelope) (Envelope, bool, error)
	OnEvent(context.Context, string, Event) error
}

// MediaStore keeps adapters independent from filesystem policy implementations.
type MediaStore interface {
	Save(context.Context, string, string, interface{ Read([]byte) (int, error) }, int64) (Part, error)
	Inspect(string) (Part, error)
	Open(string) (interface {
		Read([]byte) (int, error)
		Close() error
	}, error)
}

var ErrUnavailable = errors.New("backend unavailable")
