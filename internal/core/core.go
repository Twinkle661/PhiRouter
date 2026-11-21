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

// Reply answers a Dialog. OptionID is set for select dialogs, Text for input
// dialogs; Cancelled means the user dismissed the dialog.
type Reply struct {
	ID, OptionID, Text string
	Cancelled          bool
}

// DialogKind is the widget a dialog needs.
type DialogKind uint8

const (
	// DialogSelect asks the user to choose one of Options.
	DialogSelect DialogKind = iota
	// DialogInput asks for free text; Multiline hints at an editor.
	DialogInput
)

// OptionKind mirrors ACP permission option kinds so frontends can render
// approvals distinctly. OptionPlain is an ordinary choice.
type OptionKind uint8

const (
	OptionPlain OptionKind = iota
	OptionAllowOnce
	OptionAllowAlways
	OptionRejectOnce
	OptionRejectAlways
)

// Rejects reports whether choosing the option declines the request.
func (k OptionKind) Rejects() bool { return k == OptionRejectOnce || k == OptionRejectAlways }

type Option struct {
	ID, Label string
	Kind      OptionKind
}

// Dialog is an interactive request from the backend to the user: a choice,
// a confirmation (a select whose options allow or reject), a permission
// request, or free-text input. Frontends add a cancel control to select
// dialogs that have no rejecting option.
type Dialog struct {
	ID, Title, Message, Prefill string
	Kind                        DialogKind
	Options                     []Option
	Multiline                   bool
	Timeout                     time.Duration
}

// EventKind is the backend-neutral meaning of an Event. The vocabulary
// follows the Agent Client Protocol (session updates, prompt turns, stop
// reasons); adapters translate their wire protocol into it.
type EventKind uint8

const (
	EventExtension    EventKind = iota // backend-specific; only Raw is set, for hooks
	EventReady                         // the backend accepts prompts
	EventRestarting                    // the backend process is restarting; Text is the reason
	EventTurnStarted                   // the agent started working
	EventMessageChunk                  // Text is an increment of the agent's reply
	EventMessageEnd                    // an agent message is complete
	EventTurnEnded                     // the agent is idle again; StopReason says why
	EventToolCall                      // Tool is set
	EventDialog                        // Dialog is set; answer with Backend.Respond
	EventNotice                        // Text is an informational notice for the user
	EventError                         // Text describes a failure
	// EventDeliveryRefused comes from the router, not a backend: a frontend
	// refused a delivery with ErrSendNotAllowed. Text is the error.
	EventDeliveryRefused
)

// StopReason explains why a turn ended, as in ACP.
type StopReason uint8

const (
	StopUnknown StopReason = iota
	StopEndTurn
	StopMaxTokens
	StopMaxTurnRequests
	StopRefusal
	StopCancelled
)

type Event struct {
	Kind       EventKind
	Text       string
	StopReason StopReason
	Tool       *ToolCall
	Dialog     *Dialog
	// Raw is the backend's original record, for hooks. Its format belongs to
	// the backend type and may contain sensitive content.
	Raw json.RawMessage
}

// DeliveryMode is how the router asks a backend to take a prompt.
type DeliveryMode uint8

const (
	// DeliveryPrompt starts a turn on an idle backend.
	DeliveryPrompt DeliveryMode = iota
	// DeliverySteer adds the prompt to the running turn (Capabilities.Steer).
	DeliverySteer
	// DeliveryFollowUp runs the prompt after the running turn
	// (Capabilities.FollowUp).
	DeliveryFollowUp
)

// Capabilities tell the router what it may ask of a backend. The router
// queues prompts itself for busy backends without Steer or FollowUp and
// refuses commands for missing features.
type Capabilities struct {
	Steer      bool // takes DeliverySteer while busy
	FollowUp   bool // takes DeliveryFollowUp while busy
	Models     bool // supports Models and SetModel
	NewSession bool // supports NewSession
}

// Submission reports whether the backend handled a prompt immediately (for
// example a backend command) instead of starting or joining a turn.
type Submission struct{ Handled bool }

// Model identifies a model. Provider may be empty for backends that name
// models with a single identifier.
type Model struct{ Provider, ID string }

// String is the user-facing form, provider/id or id.
func (m Model) String() string {
	if m.Provider == "" {
		return m.ID
	}
	return m.Provider + "/" + m.ID
}

type BackendState struct {
	SessionID       string
	Model           Model
	PendingMessages int
}

// ToolStatus follows ACP tool call status.
type ToolStatus uint8

const (
	ToolPending ToolStatus = iota
	ToolInProgress
	ToolCompleted
	ToolFailed
)

// ToolCall reports progress of one tool call. Adapters report InProgress at
// most once per call, so it doubles as the start notice.
type ToolCall struct {
	ID, Title string
	Status    ToolStatus
	// Detail is a short, human-readable summary of the call's arguments chosen
	// by the backend (for example a shell command or a path). It may contain
	// sensitive content and is only set with ToolInProgress.
	Detail   string
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

// Backend is an agent harness adapter. It runs one conversation session.
type Backend interface {
	Capabilities() Capabilities
	// Run supervises the backend until ctx ends, emitting events in order.
	Run(context.Context, func(Event)) error
	// Prompt delivers user content. It returns once the backend accepted the
	// prompt; turn progress arrives as events.
	Prompt(context.Context, Envelope, DeliveryMode) (Submission, error)
	Status(context.Context) (BackendState, error)
	Models(context.Context) ([]Model, error)
	SetModel(context.Context, Model) error
	NewSession(context.Context) (bool, error) // Reports whether the session was created.
	Cancel(context.Context) error             // Discards backend-side pending work and stops the turn.
	Respond(context.Context, Reply) error
	Restart(context.Context) error
	Close(context.Context) error
}
type Inject func(context.Context, Envelope) error
type Hook interface {
	OnInbound(context.Context, Envelope) (Envelope, bool, error)
	OnOutbound(context.Context, Envelope) (Envelope, bool, error)
	// OnEvent observes every backend event, including EventExtension.
	OnEvent(context.Context, string, Event) error
}

// MediaStore keeps adapters independent from filesystem policy implementations.
type MediaStore interface {
	Save(context.Context, string, string, io.Reader, int64) (Part, error)
	Inspect(string) (Part, error)
	Open(string) (io.ReadCloser, error)
}

// ToolServer is PhiRouter's MCP endpoint as one backend should reach it.
// Backends register it with their agent in whatever way the agent supports,
// so the agent can call router tools such as send_file.
type ToolServer struct{ URL, Token, Route string }

// RouteHeader tells the MCP endpoint which backend is calling.
const RouteHeader = "X-PhiRouter-Route"

// Headers are the HTTP headers a request to the tool server must carry.
func (s ToolServer) Headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + s.Token, RouteHeader: s.Route}
}

var ErrUnavailable = errors.New("backend unavailable")

// ErrUnsupported reports that a backend lacks a capability.
var ErrUnsupported = errors.New("not supported by this backend")

// ErrSendNotAllowed reports that platform rules refused a delivery, for
// example a closed messaging window or a ban on unsolicited messages. It is
// not a transport failure and must not be retried as one. Adapters wrap it
// with details using fmt.Errorf("...: %w", ErrSendNotAllowed).
var ErrSendNotAllowed = errors.New("delivery not allowed by the platform")
