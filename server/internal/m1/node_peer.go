package m1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"cordova/server/internal/domain"

	"github.com/gorilla/websocket"
)

var (
	ErrNodeSessionUnavailable = errors.New("node session is unavailable")
	ErrNodeExchangeLimit      = errors.New("node session has too many pending exchanges")
)

const maxNodeExchanges = 128

// nodePeer is the live, authenticated C3 transport for one node. It owns the
// server-to-node sequence and the request-ID multiplexer; callers never write
// directly to the WebSocket or share its sequence counter.
type nodePeer struct {
	identity  nodeIdentity
	sessionID string
	conn      *websocket.Conn

	writeMu sync.Mutex
	nextSeq uint32

	mu      sync.Mutex
	closed  bool
	pending map[string]*nodeExchange
}

type nodeExchange struct {
	peer      *nodePeer
	requestID string
	events    chan nodeWireEnvelope
	once      sync.Once
	kind      string
	modelID   string
	commandID string
	operation string
	attemptID string
	leaseID   string
	accepted  bool
	nextChunk uint32
	terminal  bool
}

func newNodePeer(conn *websocket.Conn, identity nodeIdentity, sessionID string, nextSequence uint32) *nodePeer {
	return &nodePeer{conn: conn, identity: identity, sessionID: sessionID, nextSeq: nextSequence,
		pending: make(map[string]*nodeExchange)}
}

func (p *nodePeer) begin(ctx context.Context, typ string, payload any) (*nodeExchange, error) {
	if ctx == nil || ctx.Err() != nil || !validNodeText(typ, 64) || p == nil {
		return nil, ErrNodeSessionUnavailable
	}
	requestID, err := domain.NewID()
	if err != nil {
		return nil, err
	}
	exchange, err := newNodeExchange(p, requestID.String(), typ, payload)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrNodeSessionUnavailable
	}
	if len(p.pending) >= maxNodeExchanges {
		p.mu.Unlock()
		return nil, ErrNodeExchangeLimit
	}
	p.pending[requestID.String()] = exchange
	p.mu.Unlock()

	requestIDString := requestID.String()
	if err := p.send(typ, &requestIDString, payload); err != nil {
		p.finish(requestIDString)
		return nil, err
	}
	return exchange, nil
}

func newNodeExchange(peer *nodePeer, requestID, typ string, payload any) (*nodeExchange, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrNodeSessionUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil || fields == nil {
		return nil, ErrNodeSessionUnavailable
	}
	readText := func(name string) string {
		var value string
		_ = json.Unmarshal(fields[name], &value)
		return value
	}
	exchange := &nodeExchange{peer: peer, requestID: requestID, events: make(chan nodeWireEnvelope, 64), kind: typ}
	switch typ {
	case "model.query":
		exchange.modelID = readText("model_id")
		if !validModelSlug(exchange.modelID) || !validID(readText("catalog_version")) {
			return nil, ErrNodeSessionUnavailable
		}
	case "model.load", "model.download":
		exchange.modelID, exchange.commandID, exchange.operation = readText("model_id"), readText("command_id"), typ
		if !validModelSlug(exchange.modelID) || !validID(exchange.commandID) || !validID(readText("catalog_version")) || readText("deadline_at") == "" {
			return nil, ErrNodeSessionUnavailable
		}
	case "inference.start":
		exchange.modelID, exchange.attemptID, exchange.leaseID = readText("model_id"), readText("attempt_id"), readText("lease_id")
		if !validModelSlug(exchange.modelID) || !validID(exchange.attemptID) || !validID(exchange.leaseID) || !validID(readText("catalog_version")) {
			return nil, ErrNodeSessionUnavailable
		}
	case "inference.cancel":
		exchange.attemptID, exchange.leaseID = readText("attempt_id"), readText("lease_id")
		if !validID(exchange.attemptID) || !validID(exchange.leaseID) {
			return nil, ErrNodeSessionUnavailable
		}
	default:
		return nil, ErrNodeSessionUnavailable
	}
	return exchange, nil
}

func (p *nodePeer) send(typ string, requestID *string, payload any) error {
	if p == nil || p.conn == nil {
		return ErrNodeSessionUnavailable
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrNodeSessionUnavailable
	}
	return writeNodeEnvelope(p.conn, &p.nextSeq, typ, requestID, &p.sessionID, payload)
}

func (p *nodePeer) sendError(requestID *string, operation, code, message string, retryable bool) {
	if p == nil || p.conn == nil {
		return
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed || p.nextSeq == 0 {
		return
	}
	sendNodeWireError(p.conn, p.nextSeq, &p.sessionID, requestID, operation, code, message, retryable)
	p.nextSeq++
}

func (p *nodePeer) deliver(frame nodeWireEnvelope) bool {
	if validateNodeReply(frame) != nil {
		return false
	}
	requestID := nullableUUID(frame.RequestID)
	if requestID == nil {
		return false
	}
	p.mu.Lock()
	exchange := p.pending[*requestID]
	if p.closed || exchange == nil || exchange.terminal || len(exchange.events) >= cap(exchange.events) || !exchange.accept(frame) {
		p.mu.Unlock()
		return false
	}
	select {
	case exchange.events <- frame:
		if exchange.terminal {
			delete(p.pending, *requestID)
		}
		p.mu.Unlock()
		return true
	default:
		p.mu.Unlock()
		return false
	}
}

func (p *nodePeer) hasPending(requestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.pending[requestID] != nil
}

func (p *nodePeer) finish(requestID string) {
	p.mu.Lock()
	delete(p.pending, requestID)
	p.mu.Unlock()
}

func (p *nodePeer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for requestID, exchange := range p.pending {
		delete(p.pending, requestID)
		close(exchange.events)
	}
	p.mu.Unlock()
}

func (e *nodeExchange) accept(frame nodeWireEnvelope) bool {
	if frame.Type == "error" {
		var payload struct {
			OperationType string  `json:"operation_type"`
			AttemptID     *string `json:"attempt_id"`
			LeaseID       *string `json:"lease_id"`
			CommandID     *string `json:"command_id"`
		}
		if decodeNodePayload(frame.Payload, &payload, "operation_type", "attempt_id", "lease_id", "command_id") != nil || payload.OperationType != e.kind {
			return false
		}
		if e.attemptID != "" && (payload.AttemptID == nil || *payload.AttemptID != e.attemptID || payload.LeaseID == nil || *payload.LeaseID != e.leaseID) {
			return false
		}
		if e.commandID != "" && (payload.CommandID == nil || *payload.CommandID != e.commandID) {
			return false
		}
		e.terminal = true
		return true
	}
	switch e.kind {
	case "model.query":
		if frame.Type != "model.state" {
			return false
		}
		var payload nodeModelStatePayload
		if decodeNodePayload(frame.Payload, &payload, "model_id", "state", "artifact_sha256", "active_slots", "max_slots", "reason_code") != nil || payload.ModelID != e.modelID {
			return false
		}
		e.terminal = true
		return true
	case "model.load", "model.download":
		switch frame.Type {
		case "model.accepted":
			var payload struct {
				ModelID   string `json:"model_id"`
				CommandID string `json:"command_id"`
				Operation string `json:"operation"`
			}
			if decodeNodePayload(frame.Payload, &payload, "model_id", "command_id", "operation") != nil || payload.ModelID != e.modelID || payload.CommandID != e.commandID || payload.Operation != strings.TrimPrefix(e.operation, "model.") || e.accepted {
				return false
			}
			e.accepted = true
			return true
		case "model.progress":
			var payload struct {
				ModelID        string `json:"model_id"`
				CommandID      string `json:"command_id"`
				Phase          string `json:"phase"`
				CompletedBytes uint64 `json:"completed_bytes"`
				TotalBytes     uint64 `json:"total_bytes"`
			}
			if !e.accepted || decodeNodePayload(frame.Payload, &payload, "model_id", "command_id", "phase", "completed_bytes", "total_bytes") != nil || payload.ModelID != e.modelID || payload.CommandID != e.commandID {
				return false
			}
			return true
		case "model.ready", "model.reject":
			var payload struct {
				ModelID               string  `json:"model_id"`
				CommandID             *string `json:"command_id"`
				ArtifactSHA256        string  `json:"artifact_sha256"`
				TokenizerSHA256       string  `json:"tokenizer_sha256"`
				TemplateSHA256        string  `json:"template_sha256"`
				MaxSlots              uint32  `json:"max_slots"`
				VerifiedCapacityBytes uint64  `json:"verified_capacity_bytes"`
				Code                  string  `json:"code"`
				Retryable             bool    `json:"retryable"`
			}
			if !e.accepted || decodeNodePayload(frame.Payload, &payload, "model_id", "command_id") != nil || payload.ModelID != e.modelID || payload.CommandID == nil || *payload.CommandID != e.commandID {
				return false
			}
			if frame.Type == "model.ready" && (!validNodeHash(payload.ArtifactSHA256) || !validNodeHash(payload.TokenizerSHA256) ||
				!validNodeHash(payload.TemplateSHA256) || payload.MaxSlots == 0 || payload.MaxSlots > 64) {
				return false
			}
			if frame.Type == "model.reject" && !validModelRejectCode(payload.Code) {
				return false
			}
			e.terminal = true
			return true
		default:
			return false
		}
	case "inference.start":
		var payload struct {
			AttemptID           string  `json:"attempt_id"`
			LeaseID             string  `json:"lease_id"`
			ChunkIndex          uint32  `json:"chunk_index"`
			Text                string  `json:"text"`
			LastChunkIndex      *uint32 `json:"last_chunk_index"`
			FinishReason        string  `json:"finish_reason"`
			ClaimedInputTokens  uint32  `json:"claimed_input_tokens"`
			ClaimedOutputTokens uint32  `json:"claimed_output_tokens"`
		}
		switch frame.Type {
		case "inference.accepted":
			if decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id") != nil || payload.AttemptID != e.attemptID || payload.LeaseID != e.leaseID || e.accepted {
				return false
			}
			e.accepted = true
			return true
		case "inference.delta":
			if !e.accepted || decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id", "chunk_index", "text") != nil || payload.AttemptID != e.attemptID || payload.LeaseID != e.leaseID || payload.ChunkIndex != e.nextChunk || e.nextChunk == ^uint32(0) {
				return false
			}
			e.nextChunk++
			return true
		case "inference.complete":
			if !e.accepted || decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id", "last_chunk_index", "finish_reason", "claimed_input_tokens", "claimed_output_tokens") != nil || payload.AttemptID != e.attemptID || payload.LeaseID != e.leaseID || (payload.FinishReason != "stop" && payload.FinishReason != "length") {
				return false
			}
			if e.nextChunk == 0 && payload.LastChunkIndex != nil || e.nextChunk > 0 && (payload.LastChunkIndex == nil || *payload.LastChunkIndex != e.nextChunk-1) {
				return false
			}
			e.terminal = true
			return true
		default:
			return false
		}
	case "inference.cancel":
		if frame.Type != "inference.cancelled" {
			return false
		}
		var payload struct {
			AttemptID string `json:"attempt_id"`
			LeaseID   string `json:"lease_id"`
		}
		if decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id") != nil || payload.AttemptID != e.attemptID || payload.LeaseID != e.leaseID {
			return false
		}
		e.terminal = true
		return true
	default:
		return false
	}
}

func (e *nodeExchange) Next(ctx context.Context) (nodeWireEnvelope, error) {
	if e == nil || ctx == nil {
		return nodeWireEnvelope{}, ErrNodeSessionUnavailable
	}
	select {
	case frame, ok := <-e.events:
		if !ok {
			return nodeWireEnvelope{}, ErrNodeSessionUnavailable
		}
		return frame, nil
	case <-ctx.Done():
		return nodeWireEnvelope{}, ctx.Err()
	}
}

func (e *nodeExchange) Close() {
	if e == nil || e.peer == nil {
		return
	}
	e.once.Do(func() { e.peer.finish(e.requestID) })
}

func validateNodeReply(frame nodeWireEnvelope) error {
	if nullableUUID(frame.RequestID) == nil {
		return errors.New("node response omitted request id")
	}
	switch frame.Type {
	case "model.state":
		var payload nodeModelStatePayload
		if decodeNodePayload(frame.Payload, &payload, "model_id", "state", "artifact_sha256", "active_slots", "max_slots", "reason_code") != nil || !validNodeModelState(payload) {
			return errors.New("node model state response is invalid")
		}
	case "model.accepted":
		var payload struct {
			ModelID   string `json:"model_id"`
			CommandID string `json:"command_id"`
			Operation string `json:"operation"`
		}
		if decodeNodePayload(frame.Payload, &payload, "model_id", "command_id", "operation") != nil ||
			!validModelSlug(payload.ModelID) || !validID(payload.CommandID) || (payload.Operation != "load" && payload.Operation != "download") {
			return errors.New("node model acceptance is invalid")
		}
	case "model.progress":
		var payload struct {
			ModelID        string `json:"model_id"`
			CommandID      string `json:"command_id"`
			Phase          string `json:"phase"`
			CompletedBytes uint64 `json:"completed_bytes"`
			TotalBytes     uint64 `json:"total_bytes"`
		}
		if decodeNodePayload(frame.Payload, &payload, "model_id", "command_id", "phase", "completed_bytes", "total_bytes") != nil ||
			!validModelSlug(payload.ModelID) || !validID(payload.CommandID) ||
			(payload.Phase != "downloading" && payload.Phase != "verifying" && payload.Phase != "loading") ||
			payload.TotalBytes == 0 || payload.CompletedBytes > payload.TotalBytes {
			return errors.New("node model progress is invalid")
		}
	case "model.ready":
		var payload struct {
			ModelID               string `json:"model_id"`
			CommandID             string `json:"command_id"`
			ArtifactSHA256        string `json:"artifact_sha256"`
			TokenizerSHA256       string `json:"tokenizer_sha256"`
			TemplateSHA256        string `json:"template_sha256"`
			MaxSlots              uint32 `json:"max_slots"`
			VerifiedCapacityBytes uint64 `json:"verified_capacity_bytes"`
		}
		if decodeNodePayload(frame.Payload, &payload, "model_id", "command_id", "artifact_sha256", "tokenizer_sha256", "template_sha256", "max_slots", "verified_capacity_bytes") != nil ||
			!validModelSlug(payload.ModelID) || !validID(payload.CommandID) || !validNodeHash(payload.ArtifactSHA256) ||
			!validNodeHash(payload.TokenizerSHA256) || !validNodeHash(payload.TemplateSHA256) || payload.MaxSlots == 0 || payload.MaxSlots > 64 {
			return errors.New("node model readiness is invalid")
		}
	case "model.reject":
		var payload struct {
			ModelID   string  `json:"model_id"`
			CommandID *string `json:"command_id"`
			Code      string  `json:"code"`
			Retryable bool    `json:"retryable"`
		}
		if decodeNodePayload(frame.Payload, &payload, "model_id", "command_id", "code", "retryable") != nil ||
			!validModelSlug(payload.ModelID) || !validOptionalNodeID(payload.CommandID) || !validModelRejectCode(payload.Code) {
			return errors.New("node model rejection is invalid")
		}
	case "inference.accepted", "inference.cancelled":
		var payload struct {
			AttemptID string `json:"attempt_id"`
			LeaseID   string `json:"lease_id"`
		}
		if decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id") != nil || !validID(payload.AttemptID) || !validID(payload.LeaseID) {
			return errors.New("node inference acknowledgement is invalid")
		}
	case "inference.delta":
		var payload struct {
			AttemptID  string `json:"attempt_id"`
			LeaseID    string `json:"lease_id"`
			ChunkIndex uint32 `json:"chunk_index"`
			Text       string `json:"text"`
		}
		if decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id", "chunk_index", "text") != nil ||
			!validID(payload.AttemptID) || !validID(payload.LeaseID) || len(payload.Text) == 0 || len(payload.Text) > 16<<10 || !validNodeText(payload.Text, 16<<10) {
			return errors.New("node inference delta is invalid")
		}
	case "inference.complete":
		var payload struct {
			AttemptID           string  `json:"attempt_id"`
			LeaseID             string  `json:"lease_id"`
			LastChunkIndex      *uint32 `json:"last_chunk_index"`
			FinishReason        string  `json:"finish_reason"`
			ClaimedInputTokens  uint32  `json:"claimed_input_tokens"`
			ClaimedOutputTokens uint32  `json:"claimed_output_tokens"`
		}
		if decodeNodePayload(frame.Payload, &payload, "attempt_id", "lease_id", "last_chunk_index", "finish_reason", "claimed_input_tokens", "claimed_output_tokens") != nil ||
			!validID(payload.AttemptID) || !validID(payload.LeaseID) || (payload.FinishReason != "stop" && payload.FinishReason != "length") ||
			payload.ClaimedInputTokens > 1_000_000 || payload.ClaimedOutputTokens > 1_000_000 {
			return errors.New("node inference completion is invalid")
		}
	case "error":
		var payload struct {
			OperationType string  `json:"operation_type"`
			AttemptID     *string `json:"attempt_id"`
			LeaseID       *string `json:"lease_id"`
			CommandID     *string `json:"command_id"`
		}
		var wireError nodeWireError
		if decodeNodePayload(frame.Payload, &payload, "operation_type", "attempt_id", "lease_id", "command_id") != nil ||
			!validNodeText(payload.OperationType, 64) || !validOptionalNodeID(payload.AttemptID) || !validOptionalNodeID(payload.LeaseID) ||
			!validOptionalNodeID(payload.CommandID) || decodeNodePayload(frame.Error, &wireError, "code", "message", "retryable") != nil ||
			!validNodeErrorCode(wireError.Code) || !validNodeText(wireError.Message, 256) {
			return errors.New("node operation error is invalid")
		}
	default:
		return errors.New("unsupported node response type")
	}
	return nil
}

func validModelRejectCode(code string) bool {
	switch code {
	case "insufficient_memory", "disk_full", "unsupported_backend", "catalog_mismatch", "download_failed", "load_timeout", "busy":
		return true
	default:
		return false
	}
}

func (s *nodeService) attachPeer(peer *nodePeer) {
	s.peerMu.Lock()
	if s.peers == nil {
		s.peers = make(map[string]*nodePeer)
	}
	s.peers[peer.identity.NodeID] = peer
	s.peerMu.Unlock()
}

func (s *nodeService) detachPeer(peer *nodePeer) {
	peer.close()
	s.peerMu.Lock()
	if s.peers[peer.identity.NodeID] == peer {
		delete(s.peers, peer.identity.NodeID)
	}
	s.peerMu.Unlock()
}

func (s *nodeService) beginNodeExchange(ctx context.Context, nodeID, typ string, payload any) (*nodeExchange, error) {
	s.peerMu.RLock()
	peer := s.peers[nodeID]
	s.peerMu.RUnlock()
	if peer == nil {
		return nil, ErrNodeSessionUnavailable
	}
	return peer.begin(ctx, typ, payload)
}

func nodeResponseRequestID(frame nodeWireEnvelope) *string {
	return nullableUUID(frame.RequestID)
}
