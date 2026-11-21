// Package core contains transport-independent contracts.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type PartType string

const (
	PartText  PartType = "text"
	PartImage PartType = "image"
	PartAudio PartType = "audio"
	PartFile  PartType = "file"
)

type Part struct {
	Type    PartType `json:"type" yaml:"type"`
	Text    string   `json:"text,omitempty" yaml:"text,omitempty"`
	Path    string   `json:"path,omitempty" yaml:"path,omitempty"`
	MIME    string   `json:"mime,omitempty" yaml:"mime,omitempty"`
	Size    int64    `json:"size,omitempty" yaml:"size,omitempty"`
	Caption string   `json:"caption,omitempty" yaml:"caption,omitempty"`
}
type Envelope struct {
	System  bool   `json:"-"`
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
	Tool       *ToolEvent
}

// DeliveryMode describes scheduling intent, not a backend wire command.
type DeliveryMode uint8

const (
	DeliveryStart DeliveryMode = iota
	DeliveryInterrupt
	DeliveryQueue
)

type Submission struct{ Handled bool }
type Model struct{ Provider, ID string }
type BackendState struct {
	SessionID       string
	Model           Model
	PendingMessages int
}
type ToolPhase uint8

const (
	ToolStarted ToolPhase = iota
	ToolFinished
)

type ToolEvent struct {
	Name     string
	Phase    ToolPhase
	Failed   bool
	Duration time.Duration
}
type Frontend interface {
	Run(context.Context, func(context.Context, Envelope) error) error
	Send(context.Context, string, Envelope) error
	Typing(context.Context, string) error
	Dialog(context.Context, string, Dialog) error
}
type Backend interface {
	Run(context.Context, func(Event)) error
	Submit(context.Context, Envelope, DeliveryMode) (Submission, error)
	Status(context.Context) (BackendState, error)
	Models(context.Context) ([]Model, error)
	SetModel(context.Context, Model) error
	NewSession(context.Context) (bool, error) // Reports whether the session was created.
	Abort(context.Context) error              // Discards pending work and stops generation.
	Respond(context.Context, Reply) error
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
	Save(context.Context, string, string, io.Reader, int64) (Part, error)
	Inspect(string) (Part, error)
	Open(string) (io.ReadCloser, error)
}

var ErrUnavailable = errors.New("backend unavailable")
