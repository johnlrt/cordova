package m1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNodePeerExchangeMultiplexesStrictInferenceReplies(t *testing.T) {
	const sessionID = "00000000-0000-4000-8000-000000000001"
	const nodeID = "00000000-0000-4000-8000-000000000002"
	result := make(chan []string, 1)
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedSession := sessionID
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		peer := newNodePeer(conn, nodeIdentity{NodeID: nodeID, SessionID: expectedSession}, expectedSession, 1)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		exchange, err := peer.begin(ctx, "inference.start", map[string]any{
			"model_id": "cordova-chat-small-v1", "catalog_version": "00000000-0000-4000-8000-000000000099",
			"attempt_id": nodeID, "lease_id": sessionID,
		})
		if err != nil {
			serverErr <- err
			return
		}
		defer exchange.Close()

		observed := make([]string, 0, 3)
		for range 3 {
			_, data, readErr := conn.ReadMessage()
			if readErr != nil {
				serverErr <- readErr
				return
			}
			frame, decodeErr := decodeNodeWireEnvelope(data)
			if decodeErr != nil || validateNodeEnvelope(frame, uint32(len(observed)+1), &expectedSession, false) != nil ||
				validateNodeReply(frame) != nil || !peer.deliver(frame) {
				serverErr <- errors.New("node reply was rejected")
				return
			}
			observed = append(observed, frame.Type)
			if _, nextErr := exchange.Next(ctx); nextErr != nil {
				serverErr <- nextErr
				return
			}
		}
		result <- observed
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial test peer: HTTP %d", response.StatusCode)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := decodeNodeWireEnvelope(data)
	if err != nil || outbound.Type != "inference.start" || outbound.Sequence != 1 || nullableUUID(outbound.RequestID) == nil ||
		*nullableUUID(outbound.RequestID) == "" || nullableUUID(outbound.SessionID) == nil || *nullableUUID(outbound.SessionID) != sessionID {
		t.Fatalf("outbound request = %+v, err=%v", outbound, err)
	}
	requestID := *nullableUUID(outbound.RequestID)
	frames := []struct {
		typ     string
		payload any
	}{
		{"inference.accepted", map[string]any{"attempt_id": nodeID, "lease_id": sessionID}},
		{"inference.delta", map[string]any{"attempt_id": nodeID, "lease_id": sessionID, "chunk_index": 0, "text": "hello"}},
		{"inference.complete", map[string]any{"attempt_id": nodeID, "lease_id": sessionID, "last_chunk_index": 0, "finish_reason": "stop", "claimed_input_tokens": 3, "claimed_output_tokens": 1}},
	}
	for index, frame := range frames {
		writeTestNodeFrame(t, conn, uint32(index+1), frame.typ, newTestUUID(t), requestID, sessionID, frame.payload)
	}
	select {
	case err := <-serverErr:
		t.Fatalf("server exchange failed: %v", err)
	case got := <-result:
		if len(got) != 3 || got[0] != "inference.accepted" || got[1] != "inference.delta" || got[2] != "inference.complete" {
			t.Fatalf("reply order = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the inference reply stream")
	}
}

func TestNodePeerRejectsMalformedOrUnsolicitedReplies(t *testing.T) {
	requestID := "00000000-0000-4000-8000-000000000003"
	p := newNodePeer(nil, nodeIdentity{}, "00000000-0000-4000-8000-000000000001", 1)
	exchange, err := newNodeExchange(p, requestID, "inference.start", map[string]any{
		"model_id": "cordova-chat-small-v1", "catalog_version": "00000000-0000-4000-8000-000000000099",
		"attempt_id": "00000000-0000-4000-8000-000000000002", "lease_id": "00000000-0000-4000-8000-000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	p.pending[requestID] = exchange

	frame := nodeWireEnvelope{Type: "inference.delta", RequestID: json.RawMessage(`"` + requestID + `"`),
		Payload: json.RawMessage(`{"attempt_id":"00000000-0000-4000-8000-000000000002","lease_id":"00000000-0000-4000-8000-000000000001","chunk_index":0,"text":"hello","unexpected":true}`)}
	if validateNodeReply(frame) == nil {
		t.Fatal("unknown node response field was accepted")
	}
	if p.deliver(frame) {
		t.Fatal("malformed reply was delivered to a pending request")
	}
	accepted := nodeWireEnvelope{Type: "inference.accepted", RequestID: json.RawMessage(`"` + requestID + `"`),
		Payload: json.RawMessage(`{"attempt_id":"00000000-0000-4000-8000-000000000002","lease_id":"00000000-0000-4000-8000-000000000001"}`)}
	if !p.deliver(accepted) {
		t.Fatal("valid acceptance was not delivered")
	}
	<-exchange.events
	frame.Payload = json.RawMessage(`{"attempt_id":"00000000-0000-4000-8000-000000000002","lease_id":"00000000-0000-4000-8000-000000000001","chunk_index":0,"text":"hello"}`)
	if !p.deliver(frame) {
		t.Fatal("valid reply was not delivered")
	}
	if p.deliver(frame) {
		t.Fatal("duplicate chunk index was accepted")
	}
	p.close()
	if p.deliver(frame) {
		t.Fatal("closed session accepted a reply")
	}
}
