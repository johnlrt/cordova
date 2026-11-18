// Package c3route adapts authenticated C3 node sessions to the request
// coordinator's Router/Attempt interfaces. It only defines the transport
// boundary; the WebSocket multiplexer that implements Session lives elsewhere
// and is not wired into the gateway.
package c3route

import (
	"context"

	"cordova/server/internal/inference"
)

// Frame type names from the PLAN.md C3 registry.
const (
	EventAccepted  = "inference.accepted"
	EventDelta     = "inference.delta"
	EventComplete  = "inference.complete"
	EventCancelled = "inference.cancelled"
	EventError     = "error"
)

// Cancel reasons from the C3 registry.
const (
	ReasonClientCancelled = "client_cancelled"
	ReasonDeadline        = "deadline"
	ReasonNodeRevoked     = "node_revoked"
	ReasonOutputLimit     = "output_limit"
	ReasonServerShutdown  = "server_shutdown"
	ReasonSuperseded      = "superseded"
)

// StartFrame is the payload of `inference.start` (S→N). It carries no account
// identifiers.
type StartFrame struct {
	AttemptID      string
	LeaseID        string
	ModelID        string
	CatalogVersion string
	InputTokenIDs  []int32
	InputTokens    int
	MaxTokens      int
	Temperature    float64
	TopP           float64
	Stop           []string
	Seed           *int64
	DeadlineAt     string // RFC3339 UTC
	Kind           string // "user" or "shadow"
}

// CancelFrame is the payload of `inference.cancel` (S→N).
type CancelFrame struct {
	AttemptID string
	LeaseID   string
	Reason    string
}

// Event is one decoded N→S frame for an attempt. Generation is the session
// generation on which the frame was received; the router drops frames whose
// generation differs from the attempt's.
type Event struct {
	Generation uint64
	Type       string
	AttemptID  string
	LeaseID    string

	// delta
	ChunkIndex int64
	Text       string

	// complete
	LastChunkIndex *int64
	FinishReason   string
	ClaimedInput   int64 // diagnostic only
	ClaimedOutput  int64 // diagnostic only

	// error
	Code      string
	Message   string
	Retryable bool
}

// Session is one live, authenticated node connection. Generation is the fenced
// session version; a new connection of the same node must have a higher one.
type Session interface {
	Generation() uint64
	// Start sends inference.start and registers the attempt so its replies are
	// delivered on the returned stream.
	Start(ctx context.Context, frame StartFrame) (Stream, error)
}

// Stream is the per-attempt reply channel.
type Stream interface {
	// Events delivers frames for this attempt. It is closed when the session
	// ends; the router never blocks the transport longer than its own loop.
	Events() <-chan Event
	// Cancel sends inference.cancel. It must respect ctx.
	Cancel(ctx context.Context, frame CancelFrame) error
	// Close unregisters the attempt. It must be idempotent.
	Close()
}

// Encoder produces the exact model-template token IDs required by
// inference.start.
type Encoder interface {
	EncodeInput(ctx context.Context, modelID string, messages []inference.Message) ([]int32, error)
}
