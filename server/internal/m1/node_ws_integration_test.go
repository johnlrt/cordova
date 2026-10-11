package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"

	"github.com/gorilla/websocket"
)

func TestNodeWebSocketHandshakeCatalogHeartbeatAndSessionFencing(t *testing.T) {
	client := idempotencyTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ownerID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	nodeUUID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	catalogData, catalogKey := signedNodeCatalogForTest(t)
	config := Config{Origin: "https://nodes.example.test", PrivacyNoticeVersion: "v1", Pepper: []byte(strings.Repeat("p", 32)),
		NodeSecretEncryptionKey: []byte(strings.Repeat("e", 32)), NodeJWTSigningKey: signingKey,
		NodeCatalogSnapshot: catalogData, NodeCatalogPublicKey: catalogKey}
	store, err := NewStore(client, config)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newNodeService(client, config)
	if err != nil {
		t.Fatal(err)
	}
	node := nodeRecord{ID: nodeUUID.String(), OwnerID: ownerID.String(), Name: "test-node", HWIDHash: strings.Repeat("a", 64),
		NodeVersion: "1.0.0", SecretCipher: "v1:test", SecretVersion: 1}
	nodeJSON, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, nodeRecordKey(node.ID), nodeJSON, 0).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanCancel()
		_ = client.Del(cleanCtx, nodeRecordKey(node.ID), nodeSessionKey(node.ID), nodeGenerationKey(node.ID), nodeHeartbeatKey(node.ID), nodeModelStateKey(node.ID, "cordova-chat-small-v1")).Err()
	})
	api, err := NewHandler(config, store, inference.Mock{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/node/ws"

	firstToken, firstExpiry, err := service.issueAccessToken(node.ID, node.SecretVersion, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	firstClaims, err := service.verifyAccessToken(firstToken, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	secondToken, secondExpiry, err := service.issueAccessToken(node.ID, node.SecretVersion, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	secondClaims, err := service.verifyAccessToken(secondToken, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanCancel()
		_ = client.Del(cleanCtx, "cv:v1:node-jti:"+firstClaims.TokenID, "cv:v1:node-jti:"+secondClaims.TokenID).Err()
	})
	if !firstExpiry.After(time.Now()) || !secondExpiry.After(time.Now()) {
		t.Fatal("issued test access token is already expired")
	}

	first, firstSession, firstCatalogVersion := dialTestNode(t, wsURL, firstToken, node.ID)
	defer first.Close()
	if firstCatalogVersion != nodeCatalogTestVersion {
		t.Fatalf("welcome catalog version=%q", firstCatalogVersion)
	}
	catalogRequest, _ := domain.NewID()
	catalogMessage, _ := domain.NewID()
	writeTestNodeFrame(t, first, 2, "catalog.get", catalogMessage.String(), catalogRequest.String(), firstSession, map[string]any{"known_version": nil})
	catalog := readTestNodeFrame(t, first)
	if catalog.Type != "catalog.snapshot" || catalog.Sequence != 2 || nullableUUID(catalog.RequestID) == nil || *nullableUUID(catalog.RequestID) != catalogRequest.String() {
		t.Fatalf("catalog response=%+v", catalog)
	}
	var snapshot struct {
		Version string            `json:"version"`
		Models  []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(catalog.Payload, &snapshot); err != nil || snapshot.Version != firstCatalogVersion || len(snapshot.Models) != 1 {
		t.Fatalf("catalog snapshot=%+v err=%v", snapshot, err)
	}
	var delivered NodeCatalogManifest
	if err := json.Unmarshal(snapshot.Models[0], &delivered); err != nil || delivered.Model.ID != "cordova-chat-small-v1" || delivered.Signature == "" {
		t.Fatalf("catalog manifest=%+v err=%v", delivered, err)
	}
	// A retransmission with the same message ID and payload gets a fresh-sequence acknowledgement.
	writeTestNodeFrame(t, first, 3, "catalog.get", catalogMessage.String(), catalogRequest.String(), firstSession, map[string]any{"known_version": nil})
	catalogReplay := readTestNodeFrame(t, first)
	if catalogReplay.Type != "catalog.snapshot" || catalogReplay.Sequence != 3 {
		t.Fatalf("duplicate catalog response=%+v", catalogReplay)
	}

	heartbeatMessage, _ := domain.NewID()
	heartbeat := testNodeHeartbeat()
	writeTestNodeFrame(t, first, 4, "heartbeat", heartbeatMessage.String(), "", firstSession, heartbeat)
	ack := readTestNodeFrame(t, first)
	if ack.Type != "heartbeat.ack" || ack.Sequence != 4 || nullableUUID(ack.RequestID) != nil {
		t.Fatalf("heartbeat ack=%+v", ack)
	}
	if current, err := client.Get(ctx, nodeSessionKey(node.ID)).Result(); err != nil || current != firstSession+":1" {
		t.Fatalf("first session key=%q err=%v", current, err)
	}
	if _, err := client.Get(ctx, nodeHeartbeatKey(node.ID)).Result(); err != nil {
		t.Fatalf("heartbeat metadata was not persisted: %v", err)
	}
	modelMessage, _ := domain.NewID()
	modelState := map[string]any{"model_id": "cordova-chat-small-v1", "state": "loaded", "artifact_sha256": strings.Repeat("b", 64),
		"active_slots": 0, "max_slots": 1, "reason_code": nil}
	writeTestNodeFrame(t, first, 5, "model.state", modelMessage.String(), "", firstSession, modelState)
	barrierRequest, _ := domain.NewID()
	barrierMessage, _ := domain.NewID()
	writeTestNodeFrame(t, first, 6, "heartbeat", barrierMessage.String(), barrierRequest.String(), firstSession, heartbeat)
	if barrier := readTestNodeFrame(t, first); barrier.Type != "heartbeat.ack" {
		t.Fatalf("model state persistence barrier=%+v", barrier)
	}
	storedState, err := client.Get(ctx, nodeModelStateKey(node.ID, "cordova-chat-small-v1")).Result()
	if err != nil || !strings.Contains(storedState, `"state":"loaded"`) {
		t.Fatalf("model state was not persisted: value=%q err=%v", storedState, err)
	}

	second, secondSession, secondCatalogVersion := dialTestNode(t, wsURL, secondToken, node.ID)
	defer second.Close()
	if secondCatalogVersion != firstCatalogVersion {
		t.Fatalf("reconnected welcome catalog version=%q want=%q", secondCatalogVersion, firstCatalogVersion)
	}
	if firstSession == secondSession {
		t.Fatal("reconnect reused the previous session ID")
	}
	staleRequest, _ := domain.NewID()
	staleMessage, _ := domain.NewID()
	writeTestNodeFrame(t, first, 7, "catalog.get", staleMessage.String(), staleRequest.String(), firstSession, map[string]any{"known_version": nil})
	stale := readTestNodeFrame(t, first)
	if stale.Type != "error" {
		t.Fatalf("stale session response type=%q", stale.Type)
	}
	var wireErr nodeWireError
	if decodeNodePayload(stale.Error, &wireErr, "code", "message", "retryable") != nil || wireErr.Code != "stale_session" {
		t.Fatalf("stale session error=%+v", wireErr)
	}
	if current, err := client.Get(ctx, nodeSessionKey(node.ID)).Result(); err != nil || current != secondSession+":2" {
		t.Fatalf("new session was removed by old connection cleanup: %q err=%v", current, err)
	}
}

func dialTestNode(t *testing.T, wsURL, token, nodeID string) (*websocket.Conn, string, string) {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, EnableCompression: false}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, response, err := dialer.Dial(wsURL, header)
	if err != nil {
		if response != nil {
			t.Fatalf("dial node websocket: HTTP %d", response.StatusCode)
		}
		t.Fatalf("dial node websocket: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	writeTestNodeFrame(t, conn, 1, "hello", newTestUUID(t), "", "", map[string]any{
		"node_id": nodeID, "node_version": "1.0.0", "protocol_versions": []int{1},
		"hardware": map[string]any{"platform": "macos", "arch": "aarch64", "backend": "metal", "gpu_name": "Apple GPU",
			"reported_total_vram_bytes": 0, "reported_free_vram_bytes": 0, "ram_bytes": 16 << 30},
		"hwid_hash": strings.Repeat("a", 64),
	})
	welcome := readTestNodeFrame(t, conn)
	if welcome.Type != "welcome" || welcome.Sequence != 1 {
		t.Fatalf("node welcome=%+v", welcome)
	}
	session := nullableUUID(welcome.SessionID)
	if session == nil || !validID(*session) {
		t.Fatal("welcome omitted a valid session ID")
	}
	var welcomePayload struct {
		CatalogVersion string `json:"catalog_version"`
	}
	if err := json.Unmarshal(welcome.Payload, &welcomePayload); err != nil || !validID(welcomePayload.CatalogVersion) {
		t.Fatalf("welcome catalog version=%q err=%v", welcomePayload.CatalogVersion, err)
	}
	return conn, *session, welcomePayload.CatalogVersion
}

func testNodeHeartbeat() map[string]any {
	return map[string]any{
		"active_leases": []string{},
		"hardware": map[string]any{"platform": "macos", "arch": "aarch64", "backend": "metal", "gpu_name": "Apple GPU",
			"reported_total_vram_bytes": 0, "reported_free_vram_bytes": 0, "ram_bytes": 16 << 30},
		"models": []any{},
	}
}

func writeTestNodeFrame(t *testing.T, conn *websocket.Conn, sequence uint32, kind, messageID, requestID, sessionID string, payload any) {
	t.Helper()
	var request, session any
	if requestID != "" {
		request = requestID
	}
	if sessionID != "" {
		session = sessionID
	}
	if err := conn.WriteJSON(map[string]any{"v": 1, "type": kind, "message_id": messageID, "request_id": request,
		"session_id": session, "seq": sequence, "sent_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": payload, "error": nil}); err != nil {
		t.Fatalf("write node frame: %v", err)
	}
}

func readTestNodeFrame(t *testing.T, conn *websocket.Conn) nodeWireEnvelope {
	t.Helper()
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		t.Fatalf("read node frame kind=%d err=%v", kind, err)
	}
	frame, err := decodeNodeWireEnvelope(data)
	if err != nil {
		t.Fatalf("decode node frame: %v", err)
	}
	return frame
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
