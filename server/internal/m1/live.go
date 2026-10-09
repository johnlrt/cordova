package m1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"cordova/server/internal/domain"

	"github.com/gorilla/websocket"
)

type liveEnvelope struct {
	Version   int             `json:"v"`
	Type      string          `json:"type"`
	MessageID string          `json:"message_id"`
	RequestID *string         `json:"request_id"`
	SessionID *string         `json:"session_id"`
	Sequence  uint32          `json:"seq"`
	SentAt    string          `json:"sent_at"`
	Payload   json.RawMessage `json:"payload"`
	Error     json.RawMessage `json:"error"`
}

type liveSubscribe struct {
	Topics      []string        `json:"topics"`
	LastEventID json.RawMessage `json:"last_event_id"`
}

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !h.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Open live updates from the Cordova site.")
		return
	}
	_, account, ok := h.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	upgrader := websocket.Upgrader{
		HandshakeTimeout:  5 * time.Second,
		EnableCompression: false,
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		CheckOrigin:       func(request *http.Request) bool { return request.Header.Get("Origin") == h.cfg.Origin },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		closeLive(conn, websocket.CloseProtocolError, "invalid authentication")
		return
	}
	var auth liveEnvelope
	if !decodeLiveEnvelope(data, &auth) || auth.Version != 1 || auth.Type != "live.auth" || auth.Sequence != 1 || auth.SessionID != nil || auth.Error == nil || string(auth.Error) != "null" || !validUUID(auth.MessageID) || auth.RequestID == nil || !validUUID(*auth.RequestID) {
		closeLive(conn, websocket.ClosePolicyViolation, "invalid authentication")
		return
	}
	var authPayload struct {
		Ticket string `json:"ticket"`
	}
	if decodeStrictBytes(auth.Payload, &authPayload) != nil || len(authPayload.Ticket) < 32 || !h.store.ConsumeLiveTicket(r.Context(), account.ID, cookie.Value, authPayload.Ticket) {
		closeLive(conn, websocket.ClosePolicyViolation, "invalid authentication")
		return
	}
	if !h.store.SessionActive(r.Context(), cookie.Value) {
		closeLive(conn, 4001, "session expired")
		return
	}
	sessionID, err := domain.NewID()
	if err != nil {
		closeLive(conn, websocket.CloseInternalServerErr, "unavailable")
		return
	}
	sessionIDText := sessionID.String()
	sequence := uint32(1)
	if writeLive(conn, &sequence, "live.ready", auth.RequestID, &sessionIDText, map[string]any{"session_id": sessionIDText, "account_id": account.ID, "heartbeat_interval_ms": 15000}) != nil {
		return
	}
	h.runLiveConnection(r.Context(), conn, cookie.Value, account.ID, sessionIDText)
}

func (h *Handler) runLiveConnection(parent context.Context, conn *websocket.Conn, cookie, accountID, sessionID string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type inbound struct {
		kind int
		data []byte
		err  error
	}
	incoming := make(chan inbound, 1)
	go func() {
		for {
			conn.SetReadDeadline(time.Now().Add(45 * time.Second))
			kind, data, err := conn.ReadMessage()
			select {
			case incoming <- inbound{kind: kind, data: data, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	sequence := uint32(2)
	expectedInbound := uint32(2)
	var topics map[string]bool
	cursor := ""
	lastHeartbeat := time.Now()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	sessionCheck := time.NewTicker(time.Second)
	defer sessionCheck.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sessionCheck.C:
			if !h.store.SessionActive(ctx, cookie) {
				closeLive(conn, 4001, "session expired")
				return
			}
		case <-heartbeat.C:
			if time.Since(lastHeartbeat) > 45*time.Second || writeLive(conn, &sequence, "live.ping", nil, &sessionID, map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano)}) != nil {
				closeLive(conn, websocket.CloseGoingAway, "heartbeat timeout")
				return
			}
		case message := <-incoming:
			if message.err != nil {
				return
			}
			if message.kind != websocket.TextMessage {
				closeLive(conn, websocket.CloseProtocolError, "text frames required")
				return
			}
			var frame liveEnvelope
			if !decodeLiveEnvelope(message.data, &frame) || frame.Version != 1 || !validUUID(frame.MessageID) || frame.Sequence != expectedInbound || frame.SessionID == nil || !hmacEqual(*frame.SessionID, sessionID) || frame.Error == nil || string(frame.Error) != "null" {
				closeLive(conn, websocket.ClosePolicyViolation, "invalid message")
				return
			}
			expectedInbound++
			lastHeartbeat = time.Now()
			switch frame.Type {
			case "live.subscribe":
				if frame.RequestID == nil || !validUUID(*frame.RequestID) {
					closeLive(conn, websocket.ClosePolicyViolation, "invalid subscription")
					return
				}
				var subscription liveSubscribe
				if decodeStrictBytes(frame.Payload, &subscription) != nil || len(subscription.Topics) == 0 || len(subscription.Topics) > 3 || len(subscription.LastEventID) == 0 {
					closeLive(conn, websocket.ClosePolicyViolation, "invalid subscription")
					return
				}
				topics = map[string]bool{}
				for _, topic := range subscription.Topics {
					if (topic != "nodes" && topic != "usage" && topic != "billing") || topics[topic] {
						closeLive(conn, websocket.ClosePolicyViolation, "invalid subscription")
						return
					}
					topics[topic] = true
				}
				cursor = ""
				if string(subscription.LastEventID) != "null" {
					if json.Unmarshal(subscription.LastEventID, &cursor) != nil || len(cursor) > 256 {
						closeLive(conn, websocket.ClosePolicyViolation, "invalid subscription")
						return
					}
				}
			case "live.pong":
				if _, err := decodeLiveTime(frame.Payload); err != nil {
					closeLive(conn, websocket.ClosePolicyViolation, "invalid heartbeat")
					return
				}
			case "live.ping":
				if frame.RequestID == nil || !validUUID(*frame.RequestID) {
					closeLive(conn, websocket.ClosePolicyViolation, "invalid heartbeat")
					return
				}
				if _, err := decodeLiveTime(frame.Payload); err != nil {
					closeLive(conn, websocket.ClosePolicyViolation, "invalid heartbeat")
					return
				}
				if writeLive(conn, &sequence, "live.pong", frame.RequestID, &sessionID, map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano)}) != nil {
					return
				}
			default:
				closeLive(conn, websocket.ClosePolicyViolation, "unsupported message")
				return
			}
		case <-poll.C:
			if topics == nil || !topics["usage"] {
				continue
			}
			events, next, err := h.store.LiveEvents(ctx, accountID, cursor)
			if err != nil {
				closeLive(conn, websocket.CloseInternalServerErr, "unavailable")
				return
			}
			if next == "cursor_expired" || next == "gap" {
				if writeLive(conn, &sequence, "live.resync", nil, &sessionID, map[string]any{"reason": next}) != nil {
					return
				}
				_, cursor, err = h.store.LiveEvents(ctx, accountID, "")
				if err != nil {
					return
				}
				continue
			}
			cursor = next
			for _, event := range events {
				if event.Topic != "usage" || event.Action != "invalidate" || !validUUID(event.ResourceID) || event.Revision < 1 || !validStreamID(event.EventID) {
					closeLive(conn, websocket.CloseInternalServerErr, "unavailable")
					return
				}
				if !topics[event.Topic] {
					continue
				}
				if writeLive(conn, &sequence, "live.event", nil, &sessionID, event) != nil {
					return
				}
			}
		}
	}
}

func decodeLiveTime(payload []byte) (time.Time, error) {
	var value struct {
		Time string `json:"time"`
	}
	if decodeStrictBytes(payload, &value) != nil {
		return time.Time{}, errors.New("invalid time payload")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.Time)
	return parsed, err
}

func decodeLiveEnvelope(data []byte, target *liveEnvelope) bool {
	if decodeStrictBytes(data, target) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	for _, name := range []string{"v", "type", "message_id", "request_id", "session_id", "seq", "sent_at", "payload", "error"} {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, target.SentAt); err != nil {
		return false
	}
	return len(target.Payload) > 0
}

func writeLive(conn *websocket.Conn, sequence *uint32, typ string, requestID, sessionID *string, payload any) error {
	if *sequence == ^uint32(0) {
		return errors.New("websocket sequence exhausted")
	}
	messageID, err := domain.NewID()
	if err != nil {
		return err
	}
	frame := map[string]any{
		"v": 1, "type": typ, "message_id": messageID.String(), "request_id": requestID,
		"session_id": sessionID, "seq": *sequence, "sent_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": payload, "error": nil,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, data)
	*sequence++
	return err
}

func closeLive(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

func validUUID(value string) bool {
	_, err := domain.ParseID(value)
	return err == nil && strings.ToLower(value) == value
}
