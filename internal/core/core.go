// Package core contains transport-independent contracts.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	// Time is when the user sent the message, as reported by the frontend.
	// Zero means unknown; the router then uses its own acceptance time.
	Time time.Time `json:"-"`
	// ReplyRef is an opaque frontend reference to a message. Inbound, the
	// frontend sets it to the user's message; outbound, the router sets it to
	// the latest inbound reference for that conversation, for platforms that
	// only allow (or prefer) replies attached to a user message.
	ReplyRef string `json:"-"`
	Parts    []Part `json:"parts"`
	Reply    *Reply `json:"-"`
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

// Frontend is a chat platform adapter. Addresses (the string arguments and
// Envelope.Source) are opaque, adapter-defined strings without the instance
// prefix; the router adds and strips "<instance>:" itself.
type Frontend interface {
	// Run receives platform events until ctx ends.
	Run(context.Context, func(context.Context, Envelope) error) error
	// Send delivers an envelope. The adapter enforces platform limits such as
	// message length, splitting text as needed. It returns ErrSendNotAllowed
	// when platform rules refuse the delivery.
	Send(context.Context, string, Envelope) error
	// Typing may be a no-op on platforms without typing indicators.
	Typing(context.Context, string) error
	// Dialog presents an interactive prompt. Adapters without suitable widgets
	// degrade (for example to numbered text) rather than fail.
	Dialog(context.Context, string, Dialog) error
}

// IngressFrontend is implemented by frontends that receive platform events
// over HTTP (webhooks). The router serves the handler at the instance's
// configured ingress path; the adapter verifies platform signatures itself.
type IngressFrontend interface {
	Frontend
	Ingress() http.Handler
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

// ErrSendNotAllowed reports that platform rules refused a delivery, for
// example a closed messaging window or a ban on unsolicited messages. It is
// not a transport failure and must not be retried as one. Adapters wrap it
// with details using fmt.Errorf("...: %w", ErrSendNotAllowed).
var ErrSendNotAllowed = errors.New("delivery not allowed by the platform")
