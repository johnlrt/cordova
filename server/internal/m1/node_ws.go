package m1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"cordova/server/internal/domain"

	"github.com/gorilla/websocket"
)

const (
	nodeCatalogVersion     = "00000000-0000-4000-8000-000000000001"
	nodeSocketLifetime     = 24 * time.Hour
	nodeHeartbeatPeriod    = 15 * time.Second
	nodeHeartbeatLimit     = 45 * time.Second
	maxNodeModels          = 256
	maxNodeSessionMessages = 8192
)

type nodeWireEnvelope struct {
	Version   int             `json:"v"`
	Type      string          `json:"type"`
	MessageID string          `json:"message_id"`
	RequestID json.RawMessage `json:"request_id"`
	SessionID json.RawMessage `json:"session_id"`
	Sequence  uint32          `json:"seq"`
	SentAt    string          `json:"sent_at"`
	Payload   json.RawMessage `json:"payload"`
	Error     json.RawMessage `json:"error"`
}

type nodeHardware struct {
	Platform               string  `json:"platform"`
	Arch                   string  `json:"arch"`
	Backend                string  `json:"backend"`
	GPUName                *string `json:"gpu_name"`
	ReportedTotalVRAMBytes uint64  `json:"reported_total_vram_bytes"`
	ReportedFreeVRAMBytes  uint64  `json:"reported_free_vram_bytes"`
	RAMBytes               uint64  `json:"ram_bytes"`
}

type nodeModelReport struct {
	ModelID               string `json:"model_id"`
	State                 string `json:"state"`
	ActiveSlots           uint32 `json:"active_slots"`
	MaxSlots              uint32 `json:"max_slots"`
	VerifiedCapacityBytes uint64 `json:"verified_capacity_bytes"`
	UpdatedAt             string `json:"updated_at"`
}

type nodeHelloPayload struct {
	NodeID          string       `json:"node_id"`
	NodeVersion     string       `json:"node_version"`
	ProtocolVersion []int        `json:"protocol_versions"`
	Hardware        nodeHardware `json:"hardware"`
	HWIDHash        string       `json:"hwid_hash"`
}

type nodeHeartbeatPayload struct {
	ActiveLeases []string          `json:"active_leases"`
	Hardware     nodeHardware      `json:"hardware"`
	Models       []nodeModelReport `json:"models"`
}

type nodeModelRefPayload struct {
	ModelID        string `json:"model_id"`
	CatalogVersion string `json:"catalog_version"`
}

type nodeModelStatePayload struct {
	ModelID        string  `json:"model_id"`
	State          string  `json:"state"`
	ArtifactSHA256 *string `json:"artifact_sha256"`
	ActiveSlots    uint32  `json:"active_slots"`
	MaxSlots       uint32  `json:"max_slots"`
	ReasonCode     *string `json:"reason_code"`
}

type nodeCatalogGetPayload struct {
	KnownVersion json.RawMessage `json:"known_version"`
}

type nodeGoodbyePayload struct {
	Reason       string   `json:"reason"`
	ActiveLeases []string `json:"active_leases"`
}

type nodeSessionHeartbeat struct {
	SessionID string               `json:"session_id"`
	NodeID    string               `json:"node_id"`
	Received  string               `json:"received_at"`
	Payload   nodeHeartbeatPayload `json:"heartbeat"`
}

type nodeWireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type cachedNodeReply struct {
	Type      string
	RequestID *string
	Payload   any
}

func nodeSessionKey(nodeID string) string    { return "cv:v1:node-session:" + nodeID }
func nodeGenerationKey(nodeID string) string { return "cv:v1:node-generation:" + nodeID }
func nodeHeartbeatKey(nodeID string) string  { return "cv:v1:node-heartbeat:" + nodeID }
func nodeModelStateKey(nodeID, modelID string) string {
	return "cv:v1:node-model-state:" + nodeID + ":" + modelID
}

func (s *nodeService) establishSession(ctx context.Context, identity nodeIdentity, sessionID string, expires time.Time) (uint64, error) {
	value, err := s.redis.Eval(ctx, `
local node_type=redis.call('TYPE',KEYS[1]).ok
local generation_type=redis.call('TYPE',KEYS[2]).ok
local session_type=redis.call('TYPE',KEYS[3]).ok
if node_type~='string' or (generation_type~='none' and generation_type~='string') or (session_type~='none' and session_type~='string') then return redis.error_reply('node session key type mismatch') end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not ok or type(node)~='table' or node['id']~=ARGV[1] or node['owner_id']~=ARGV[2] or node['secret_version']~=tonumber(ARGV[3]) or node['revoked']~=false then return false end
local generation=redis.call('INCR',KEYS[2])
redis.call('SET',KEYS[3],ARGV[4]..':'..generation,'EX',ARGV[5])
redis.call('DEL',KEYS[4])
return generation`, []string{nodeRecordKey(identity.NodeID), nodeGenerationKey(identity.NodeID), nodeSessionKey(identity.NodeID), nodeHeartbeatKey(identity.NodeID)},
		identity.NodeID, identity.OwnerID, identity.SecretVersion, sessionID, int64(time.Until(expires).Seconds())).Int64()
	if err != nil {
		return 0, err
	}
	if value < 1 {
		return 0, ErrNodeUnauthorized
	}
	return uint64(value), nil
}

func (s *nodeService) nodeSessionCurrent(ctx context.Context, identity nodeIdentity, sessionID string) (bool, error) {
	value, err := s.redis.Eval(ctx, `
local session_type=redis.call('TYPE',KEYS[1]).ok
local node_type=redis.call('TYPE',KEYS[2]).ok
if session_type~='string' or node_type~='string' then return 0 end
if redis.call('GET',KEYS[1])~=ARGV[1]..':'..ARGV[2] then return 0 end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[2]))
if not ok or type(node)~='table' or node['id']~=ARGV[3] or node['owner_id']~=ARGV[4] or node['secret_version']~=tonumber(ARGV[5]) or node['revoked']~=false then return 0 end
return 1`, []string{nodeSessionKey(identity.NodeID), nodeRecordKey(identity.NodeID)}, sessionID, identity.Generation,
		identity.NodeID, identity.OwnerID, identity.SecretVersion).Int()
	return value == 1, err
}

func (s *nodeService) recordNodeHeartbeat(ctx context.Context, identity nodeIdentity, sessionID string, heartbeat nodeSessionHeartbeat) error {
	encoded, err := json.Marshal(heartbeat)
	if err != nil {
		return err
	}
	hardware, err := json.Marshal(heartbeat.Payload.Hardware)
	if err != nil {
		return err
	}
	result, err := s.redis.Eval(ctx, `
local session_type=redis.call('TYPE',KEYS[1]).ok
local node_type=redis.call('TYPE',KEYS[2]).ok
local heartbeat_type=redis.call('TYPE',KEYS[3]).ok
if session_type~='string' or node_type~='string' or (heartbeat_type~='none' and heartbeat_type~='string') then return redis.error_reply('node heartbeat key type mismatch') end
if redis.call('GET',KEYS[1])~=ARGV[1]..':'..ARGV[2] then return 0 end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[2]))
if not ok or type(node)~='table' or node['id']~=ARGV[3] or node['owner_id']~=ARGV[4] or node['secret_version']~=tonumber(ARGV[5]) or node['revoked']~=false then return 0 end
node['hardware']=cjson.decode(ARGV[8])
node['last_seen_at']=ARGV[9]
redis.call('SET',KEYS[2],cjson.encode(node))
redis.call('SET',KEYS[3],ARGV[6],'EX',ARGV[7])
return 1`, []string{nodeSessionKey(identity.NodeID), nodeRecordKey(identity.NodeID), nodeHeartbeatKey(identity.NodeID)},
		sessionID, identity.Generation, identity.NodeID, identity.OwnerID, identity.SecretVersion, string(encoded),
		int64(nodeHeartbeatLimit.Seconds()), string(hardware), heartbeat.Received).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrNodeConflict
	}
	return nil
}

func (s *nodeService) recordNodeModelState(ctx context.Context, identity nodeIdentity, sessionID string, state nodeModelStatePayload, received time.Time) error {
	encoded, err := json.Marshal(map[string]any{"session_id": sessionID, "node_id": identity.NodeID,
		"received_at": received.UTC().Format(time.RFC3339Nano), "model": state})
	if err != nil {
		return err
	}
	result, err := s.redis.Eval(ctx, `
local session_type=redis.call('TYPE',KEYS[1]).ok
local node_type=redis.call('TYPE',KEYS[2]).ok
local state_type=redis.call('TYPE',KEYS[3]).ok
if session_type~='string' or node_type~='string' or (state_type~='none' and state_type~='string') then return redis.error_reply('node model state key type mismatch') end
if redis.call('GET',KEYS[1])~=ARGV[1]..':'..ARGV[2] then return 0 end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[2]))
if not ok or type(node)~='table' or node['id']~=ARGV[3] or node['owner_id']~=ARGV[4] or node['secret_version']~=tonumber(ARGV[5]) or node['revoked']~=false then return 0 end
redis.call('SET',KEYS[3],ARGV[6],'EX',ARGV[7])
return 1`, []string{nodeSessionKey(identity.NodeID), nodeRecordKey(identity.NodeID), nodeModelStateKey(identity.NodeID, state.ModelID)},
		sessionID, identity.Generation, identity.NodeID, identity.OwnerID, identity.SecretVersion, string(encoded), int64(nodeHeartbeatLimit.Seconds())).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrNodeConflict
	}
	return nil
}

func (s *nodeService) clearNodeSession(ctx context.Context, identity nodeIdentity, sessionID string) error {
	return s.redis.Eval(ctx, `
local session_type=redis.call('TYPE',KEYS[1]).ok
if session_type~='none' and session_type~='string' then return redis.error_reply('node session key type mismatch') end
if redis.call('GET',KEYS[1])==ARGV[1]..':'..ARGV[2] then
  redis.call('DEL',KEYS[1],KEYS[2])
  return 1
end
return 0`, []string{nodeSessionKey(identity.NodeID), nodeHeartbeatKey(identity.NodeID)}, sessionID, identity.Generation).Err()
}

func (h *Handler) nodeWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if r.URL.RawQuery != "" || r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "csrf_failed", "Node sockets do not accept browser origins or query credentials.")
		return
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		writeNodeError(w, ErrNodeUnauthorized)
		return
	}
	identity, err := h.nodes.consumeAccessToken(r.Context(), strings.TrimPrefix(authorization, "Bearer "), time.Now().UTC())
	if err != nil {
		writeNodeError(w, err)
		return
	}
	upgrader := websocket.Upgrader{
		HandshakeTimeout:  5 * time.Second,
		EnableCompression: false,
		ReadBufferSize:    32 << 10,
		WriteBufferSize:   32 << 10,
		CheckOrigin:       func(request *http.Request) bool { return request.Header.Get("Origin") == "" },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	h.runNodeConnection(r.Context(), conn, identity)
}

func (h *Handler) runNodeConnection(parent context.Context, conn *websocket.Conn, identity nodeIdentity) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		nodeClose(conn, websocket.CloseProtocolError, "invalid hello")
		return
	}
	helloFrame, err := decodeNodeWireEnvelope(data)
	if err != nil || validateNodeEnvelope(helloFrame, 1, nil, true) != nil || helloFrame.Type != "hello" {
		sendNodeWireError(conn, 1, nil, nil, "hello", "invalid_message", "The node handshake is invalid.", false)
		nodeClose(conn, websocket.ClosePolicyViolation, "invalid hello")
		return
	}
	var hello nodeHelloPayload
	if decodeNodePayload(helloFrame.Payload, &hello, "node_id", "node_version", "protocol_versions", "hardware", "hwid_hash") != nil ||
		decodeNodePayload(nodeObjectField(helloFrame.Payload, "hardware"), &hello.Hardware, "platform", "arch", "backend", "gpu_name", "reported_total_vram_bytes", "reported_free_vram_bytes", "ram_bytes") != nil ||
		hello.NodeID != identity.NodeID || !validNodeText(hello.NodeVersion, 64) || len(hello.ProtocolVersion) != 1 || hello.ProtocolVersion[0] != 1 ||
		!validNodeHash(hello.HWIDHash) || !validNodeHardware(hello.Hardware) {
		sendNodeWireError(conn, 1, nullableUUID(helloFrame.RequestID), nil, "hello", "invalid_message", "The node handshake is invalid.", false)
		nodeClose(conn, websocket.ClosePolicyViolation, "invalid hello")
		return
	}

	sessionUUID, err := domain.NewID()
	if err != nil {
		nodeClose(conn, websocket.CloseInternalServerErr, "unavailable")
		return
	}
	sessionID := sessionUUID.String()
	now := time.Now().UTC()
	expires := now.Add(nodeSocketLifetime)
	generation, err := h.nodes.establishSession(parent, identity, sessionID, expires)
	if err != nil {
		if errors.Is(err, ErrNodeUnauthorized) {
			nodeClose(conn, websocket.ClosePolicyViolation, "unauthenticated")
		} else {
			nodeClose(conn, websocket.CloseInternalServerErr, "unavailable")
		}
		return
	}
	identity.SessionID, identity.Generation = sessionID, generation
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = h.nodes.clearNodeSession(cleanup, identity, sessionID)
	}()
	ctx, cancel := context.WithDeadline(parent, expires)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if !time.Now().Before(expires) {
				nodeClose(conn, 4001, "session expired")
			} else {
				nodeClose(conn, websocket.CloseGoingAway, "session ended")
			}
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	inputSeq, outputSeq := uint32(2), uint32(1)
	if err := writeNodeEnvelope(conn, &outputSeq, "welcome", nil, &sessionID, map[string]any{
		"session_id": sessionID, "heartbeat_interval_ms": int(nodeHeartbeatPeriod / time.Millisecond),
		"heartbeat_timeout_ms": int(nodeHeartbeatLimit / time.Millisecond), "catalog_version": h.catalog.Version,
		"max_frame_bytes": 1 << 20, "connection_expires_at": expires.Format(time.RFC3339Nano),
	}); err != nil {
		return
	}
	peer := newNodePeer(conn, identity, sessionID, outputSeq)
	h.nodes.attachPeer(peer)
	defer h.nodes.detachPeer(peer)
	current, currentErr := h.nodes.nodeSessionCurrent(ctx, identity, sessionID)
	if currentErr != nil {
		nodeClose(conn, websocket.CloseInternalServerErr, "unavailable")
		return
	}
	if !current {
		nodeClose(conn, 4001, "stale session")
		return
	}
	lastHeartbeat := now
	var lastPong atomic.Int64
	lastPong.Store(now.UnixNano())
	conn.SetPongHandler(func(string) error {
		lastPong.Store(time.Now().UnixNano())
		return conn.SetReadDeadline(time.Now().Add(nodeHeartbeatLimit))
	})
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(nodeHeartbeatPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingDone:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastPong.Load())) > nodeHeartbeatLimit {
					nodeClose(conn, websocket.CloseGoingAway, "heartbeat timeout")
					_ = conn.Close()
					return
				}
				_ = conn.WriteControl(websocket.PingMessage, []byte("cordova"), time.Now().Add(time.Second))
			}
		}
	}()
	defer close(pingDone)
	seen := make(map[string]string)
	replies := make(map[string]cachedNodeReply)
	for {
		if !time.Now().Before(expires) {
			nodeClose(conn, 4001, "session expired")
			return
		}
		if time.Since(lastHeartbeat) > nodeHeartbeatLimit {
			nodeClose(conn, websocket.CloseGoingAway, "heartbeat timeout")
			return
		}
		conn.SetReadDeadline(time.Now().Add(nodeHeartbeatLimit))
		kind, data, err = conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			peer.sendError(nil, "frame", "invalid_message", "Text frames are required.", false)
			nodeClose(conn, websocket.CloseProtocolError, "text frames required")
			return
		}
		frame, err := decodeNodeWireEnvelope(data)
		if err != nil || validateNodeEnvelope(frame, inputSeq, &sessionID, false) != nil {
			peer.sendError(nil, "frame", "invalid_message", "The node message is invalid.", false)
			nodeClose(conn, websocket.ClosePolicyViolation, "invalid message")
			return
		}
		inputSeq++
		if inputSeq == 0 {
			nodeClose(conn, websocket.ClosePolicyViolation, "sequence exhausted")
			return
		}
		digest := frame.Type + "\n" + string(frame.RequestID) + "\n" + string(frame.Payload) + "\n" + string(frame.Error)
		if previous, exists := seen[frame.MessageID]; exists {
			if previous != digest {
				peer.sendError(nullableUUID(frame.RequestID), frame.Type, "replayed_message", "This message identifier was reused.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "replayed message")
				return
			}
			reply, cached := replies[frame.MessageID]
			if !cached {
				peer.sendError(nullableUUID(frame.RequestID), frame.Type, "replayed_message", "This message was already processed.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "replayed message")
				return
			}
			if reply.Type == "" {
				continue
			}
			if peer.send(reply.Type, reply.RequestID, reply.Payload) != nil {
				return
			}
			continue
		}
		if len(seen) >= maxNodeSessionMessages {
			peer.sendError(nullableUUID(frame.RequestID), frame.Type, "rate_limited", "Too many distinct messages arrived in one session.", true)
			nodeClose(conn, websocket.ClosePolicyViolation, "message limit")
			return
		}
		seen[frame.MessageID] = digest

		current, currentErr := h.nodes.nodeSessionCurrent(ctx, identity, sessionID)
		if currentErr != nil {
			peer.sendError(nullableUUID(frame.RequestID), frame.Type, "internal_error", "The node session is unavailable.", true)
			nodeClose(conn, websocket.CloseInternalServerErr, "unavailable")
			return
		}
		if !current {
			peer.sendError(nullableUUID(frame.RequestID), frame.Type, "stale_session", "A newer node session is active.", false)
			nodeClose(conn, 4001, "stale session")
			return
		}
		switch frame.Type {
		case "heartbeat":
			var heartbeat nodeHeartbeatPayload
			if decodeNodePayload(frame.Payload, &heartbeat, "active_leases", "hardware", "models") != nil ||
				decodeNodePayload(nodeObjectField(frame.Payload, "hardware"), &heartbeat.Hardware, "platform", "arch", "backend", "gpu_name", "reported_total_vram_bytes", "reported_free_vram_bytes", "ram_bytes") != nil ||
				decodeNodeModels(nodeObjectField(frame.Payload, "models"), &heartbeat.Models) != nil ||
				!validNodeIDArray(nodeObjectField(frame.Payload, "active_leases")) || len(heartbeat.ActiveLeases) != 0 ||
				!validNodeHardware(heartbeat.Hardware) || !validNodeModels(heartbeat.Models) {
				peer.sendError(nullableUUID(frame.RequestID), "heartbeat", "stale_lease", "The heartbeat contains unsupported active leases or invalid state.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "invalid heartbeat")
				return
			}
			lastHeartbeat = time.Now().UTC()
			heartbeatRecord := nodeSessionHeartbeat{SessionID: sessionID, NodeID: identity.NodeID, Received: lastHeartbeat.Format(time.RFC3339Nano), Payload: heartbeat}
			if err := h.nodes.recordNodeHeartbeat(ctx, identity, sessionID, heartbeatRecord); err != nil {
				if errors.Is(err, ErrNodeConflict) {
					nodeClose(conn, 4001, "stale session")
				} else {
					nodeClose(conn, websocket.CloseInternalServerErr, "unavailable")
				}
				return
			}
			ack := map[string]any{
				"server_time": lastHeartbeat.Format(time.RFC3339Nano), "session_expires_at": expires.Format(time.RFC3339Nano),
			}
			if peer.send("heartbeat.ack", nullableUUID(frame.RequestID), ack) != nil {
				return
			}
			replies[frame.MessageID] = cachedNodeReply{Type: "heartbeat.ack", RequestID: nullableUUID(frame.RequestID), Payload: ack}
		case "catalog.get":
			var request nodeCatalogGetPayload
			if decodeNodePayload(frame.Payload, &request, "known_version") != nil || !validNullableID(request.KnownVersion) || nullableUUID(frame.RequestID) == nil {
				peer.sendError(nullableUUID(frame.RequestID), "catalog.get", "invalid_message", "The catalog request is invalid.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "invalid catalog request")
				return
			}
			if peer.send("catalog.snapshot", nullableUUID(frame.RequestID), h.catalog) != nil {
				return
			}
			replies[frame.MessageID] = cachedNodeReply{Type: "catalog.snapshot", RequestID: nullableUUID(frame.RequestID), Payload: h.catalog}
		case "model.state":
			var model nodeModelStatePayload
			if decodeNodePayload(frame.Payload, &model, "model_id", "state", "artifact_sha256", "active_slots", "max_slots", "reason_code") != nil ||
				!validNodeModelState(model) {
				peer.sendError(nullableUUID(frame.RequestID), "model.state", "invalid_message", "The model state is invalid.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "invalid model state")
				return
			}
			if requestID := nullableUUID(frame.RequestID); requestID != nil &&
				(validateNodeReply(frame) != nil || !peer.hasPending(*requestID)) {
				peer.sendError(requestID, "model.state", "stale_lease", "The model response is no longer pending.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "unsolicited model response")
				return
			}
			if err := h.nodes.recordNodeModelState(ctx, identity, sessionID, model, time.Now().UTC()); err != nil {
				if errors.Is(err, ErrNodeConflict) {
					nodeClose(conn, 4001, "stale session")
				} else {
					nodeClose(conn, websocket.CloseInternalServerErr, "unavailable")
				}
				return
			}
			if nullableUUID(frame.RequestID) != nil {
				if !peer.deliver(frame) {
					peer.sendError(nullableUUID(frame.RequestID), "model.state", "stale_lease", "The model response is no longer pending.", false)
					nodeClose(conn, websocket.ClosePolicyViolation, "unsolicited model response")
					return
				}
			}
			replies[frame.MessageID] = cachedNodeReply{}
		case "goodbye":
			var goodbye nodeGoodbyePayload
			if decodeNodePayload(frame.Payload, &goodbye, "reason", "active_leases") != nil ||
				!validNodeIDArray(nodeObjectField(frame.Payload, "active_leases")) ||
				(goodbye.Reason != "shutdown" && goodbye.Reason != "upgrade") || len(goodbye.ActiveLeases) != 0 {
				peer.sendError(nullableUUID(frame.RequestID), "goodbye", "stale_lease", "The node cannot disconnect with unsupported active leases.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "invalid goodbye")
				return
			}
			nodeClose(conn, websocket.CloseNormalClosure, "goodbye")
			return
		case "error":
			var payload struct {
				OperationType string  `json:"operation_type"`
				AttemptID     *string `json:"attempt_id"`
				LeaseID       *string `json:"lease_id"`
				CommandID     *string `json:"command_id"`
			}
			var wireError nodeWireError
			if decodeNodePayload(frame.Payload, &payload, "operation_type", "attempt_id", "lease_id", "command_id") != nil ||
				!validNodeText(payload.OperationType, 64) || !validOptionalNodeID(payload.AttemptID) ||
				!validOptionalNodeID(payload.LeaseID) || !validOptionalNodeID(payload.CommandID) ||
				decodeNodePayload(frame.Error, &wireError, "code", "message", "retryable") != nil ||
				!validNodeErrorCode(wireError.Code) || wireError.Message == "" || len(wireError.Message) > 256 || !utf8.ValidString(wireError.Message) {
				nodeClose(conn, websocket.ClosePolicyViolation, "invalid error frame")
				return
			}
			if !peer.deliver(frame) {
				peer.sendError(nullableUUID(frame.RequestID), "error", "stale_lease", "The node response is no longer pending.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "unsolicited node response")
				return
			}
			replies[frame.MessageID] = cachedNodeReply{}
		default:
			if validateNodeReply(frame) != nil || !peer.deliver(frame) {
				peer.sendError(nullableUUID(frame.RequestID), frame.Type, "invalid_message", "The node response is invalid or no longer pending.", false)
				nodeClose(conn, websocket.ClosePolicyViolation, "invalid or unsolicited node response")
				return
			}
			replies[frame.MessageID] = cachedNodeReply{}
		}
	}
}

func validOptionalNodeID(value *string) bool {
	return value == nil || validID(*value)
}

func nodeObjectField(data json.RawMessage, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	return fields[name]
}

func decodeNodeModels(raw json.RawMessage, target *[]nodeModelReport) error {
	var entries []json.RawMessage
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &entries) != nil || len(entries) > maxNodeModels {
		return errors.New("invalid node models")
	}
	models := make([]nodeModelReport, len(entries))
	for i, entry := range entries {
		if err := decodeNodePayload(entry, &models[i], "model_id", "state", "active_slots", "max_slots", "verified_capacity_bytes", "updated_at"); err != nil {
			return err
		}
	}
	*target = models
	return nil
}

func validNodeIDArray(raw json.RawMessage) bool {
	var values []string
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &values) != nil {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !validID(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func decodeNodeWireEnvelope(data []byte) (nodeWireEnvelope, error) {
	var frame nodeWireEnvelope
	if len(data) > 1<<20 || decodeStrictBytes(data, &frame) != nil || frame.RequestID == nil || frame.SessionID == nil || frame.Payload == nil || frame.Error == nil {
		return nodeWireEnvelope{}, errors.New("invalid node envelope")
	}
	return frame, nil
}

func validateNodeEnvelope(frame nodeWireEnvelope, expectedSeq uint32, sessionID *string, hello bool) error {
	if frame.Version != 1 || frame.Sequence != expectedSeq || frame.Sequence == 0 || !validID(frame.MessageID) || !validNodeText(frame.Type, 64) {
		return errors.New("invalid node envelope header")
	}
	if _, err := time.Parse(time.RFC3339Nano, frame.SentAt); err != nil {
		return errors.New("invalid node timestamp")
	}
	requestID := nullableUUID(frame.RequestID)
	if requestID != nil && !validID(*requestID) {
		return errors.New("invalid node request id")
	}
	frameSession := nullableUUID(frame.SessionID)
	if hello {
		if frame.Type != "hello" || requestID != nil || frameSession != nil || !isJSONNull(frame.Error) {
			return errors.New("invalid node hello envelope")
		}
	} else if sessionID == nil || frameSession == nil || *frameSession != *sessionID || frame.Type == "hello" {
		return errors.New("invalid node session binding")
	} else if frame.Type == "error" {
		if isJSONNull(frame.Error) {
			return errors.New("missing node error object")
		}
	} else if !isJSONNull(frame.Error) {
		return errors.New("unexpected node error object")
	}
	return nil
}

func decodeNodePayload(data json.RawMessage, target any, required ...string) error {
	if len(data) == 0 || isJSONNull(data) {
		return errors.New("missing node payload")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("invalid node payload")
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return errors.New("missing node payload field")
		}
	}
	return decodeStrictBytes(data, target)
}

func nullableUUID(raw json.RawMessage) *string {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func validNullableID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if isJSONNull(raw) {
		return true
	}
	value := nullableUUID(raw)
	return value != nil && validID(*value)
}

func isJSONNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func validNodeHardware(hardware nodeHardware) bool {
	if (hardware.Platform != "linux" && hardware.Platform != "windows" && hardware.Platform != "macos") ||
		(hardware.Arch != "x86_64" && hardware.Arch != "aarch64") ||
		(hardware.Backend != "cuda" && hardware.Backend != "metal" && hardware.Backend != "vulkan" && hardware.Backend != "cpu") ||
		hardware.RAMBytes == 0 || hardware.ReportedFreeVRAMBytes > hardware.ReportedTotalVRAMBytes {
		return false
	}
	return hardware.GPUName == nil || validHardwareName(*hardware.GPUName)
}

func validHardwareName(value string) bool {
	if len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validNodeModels(models []nodeModelReport) bool {
	if len(models) > maxNodeModels {
		return false
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if !validModelSlug(model.ModelID) || seen[model.ModelID] ||
			(model.State != "loaded" && model.State != "on_disk" && model.State != "absent") ||
			model.ActiveSlots > model.MaxSlots || (model.State == "loaded" && model.MaxSlots == 0) {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, model.UpdatedAt); err != nil {
			return false
		}
		seen[model.ModelID] = true
	}
	return true
}

func validModelSlug(value string) bool {
	if value == "" || len(value) > 96 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	previousDash := false
	for _, char := range value {
		if char == '-' {
			if previousDash {
				return false
			}
			previousDash = true
			continue
		}
		if char < 'a' || char > 'z' {
			if char < '0' || char > '9' {
				return false
			}
		}
		previousDash = false
	}
	return true
}

func validNodeModelState(model nodeModelStatePayload) bool {
	if !validModelSlug(model.ModelID) || (model.State != "loaded" && model.State != "on_disk" && model.State != "absent") ||
		model.ActiveSlots > model.MaxSlots || (model.State == "loaded" && model.MaxSlots == 0) ||
		(model.ArtifactSHA256 != nil && !validNodeHash(*model.ArtifactSHA256)) ||
		(model.ReasonCode != nil && !validNodeText(*model.ReasonCode, 64)) {
		return false
	}
	return true
}

func validNodeErrorCode(value string) bool {
	switch value {
	case "unsupported_version", "invalid_message", "unauthenticated", "replayed_message", "stale_session", "stale_lease", "model_not_found", "catalog_mismatch", "insufficient_memory", "no_capacity", "download_failed", "load_timeout", "inference_failed", "timeout", "cancelled", "output_limit", "rate_limited", "internal_error":
		return true
	default:
		return false
	}
}

func writeNodeEnvelope(conn *websocket.Conn, sequence *uint32, typ string, requestID, sessionID *string, payload any) error {
	if *sequence == 0 || requestID != nil && !validID(*requestID) || sessionID != nil && !validID(*sessionID) {
		return errors.New("invalid node response envelope")
	}
	messageID, err := domain.NewID()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(map[string]any{"v": 1, "type": typ, "message_id": messageID.String(), "request_id": requestID,
		"session_id": sessionID, "seq": *sequence, "sent_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": payload, "error": nil})
	if err != nil {
		return err
	}
	if len(encoded) > 1<<20 {
		return errors.New("node response exceeds frame limit")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, encoded); err != nil {
		return err
	}
	*sequence = *sequence + 1
	if *sequence == 0 {
		return errors.New("node server sequence exhausted")
	}
	return nil
}

func sendNodeWireError(conn *websocket.Conn, sequence uint32, sessionID, requestID *string, operation, code, message string, retryable bool) {
	if code == "" || len(message) == 0 || len(message) > 256 {
		return
	}
	messageID, err := domain.NewID()
	if err != nil {
		return
	}
	encoded, err := json.Marshal(map[string]any{"v": 1, "type": "error", "message_id": messageID.String(), "request_id": requestID,
		"session_id": sessionID, "seq": sequence, "sent_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{"operation_type": operation, "attempt_id": nil, "lease_id": nil, "command_id": nil},
		"error":   map[string]any{"code": code, "message": message, "retryable": retryable}})
	if err != nil || len(encoded) > 1<<20 {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, encoded)
}

func nodeClose(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}
