package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

func TestNodeEnrollmentApprovalAndSingleUseAccessTokens(t *testing.T) {
	client := idempotencyTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newNodeService(client, Config{Origin: "https://nodes.example.test", Pepper: []byte(strings.Repeat("p", 32)),
		NodeSecretEncryptionKey: []byte(strings.Repeat("e", 32)), NodeJWTSigningKey: signingKey})
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	source := "node-auth-integration-" + sourceID.String()
	created, err := service.createEnrollment(ctx, source, "test-node", "1.0.0", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	cleanupKeys := []string{
		nodeEnrollmentPrefix + created.EnrollmentID,
		nodeEnrollmentDeviceKey + service.verifier("node-device-v1", created.DeviceCode),
		nodeEnrollmentUserKey + service.verifier("node-user-v1", created.UserCode),
		service.nodeRateKey("enroll", source),
		service.nodeRateKey("approve", ownerID.String()),
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanCancel()
		if len(cleanupKeys) > 0 {
			_ = client.Del(cleanCtx, cleanupKeys...).Err()
		}
	})

	if _, err := service.pollEnrollment(ctx, created.DeviceCode); !errors.Is(err, ErrNodePending) {
		t.Fatalf("poll before approval err=%v, want pending", err)
	}
	if err := service.approveEnrollment(ctx, ownerID.String(), created.UserCode); err != nil {
		t.Fatalf("approve enrollment: %v", err)
	}
	enrolled, err := service.pollEnrollment(ctx, created.DeviceCode)
	if err != nil || enrolled.ServerOrigin != service.origin {
		t.Fatalf("poll approved enrollment=%+v err=%v", enrolled, err)
	}
	cleanupKeys = append(cleanupKeys, nodeRecordKey(enrolled.NodeID))
	if len(enrolled.NodeSecret) != 64 {
		t.Fatalf("node secret length=%d, want 64 hex characters", len(enrolled.NodeSecret))
	}

	issue := func() string {
		t.Helper()
		nonce, _, err := service.createChallenge(ctx, source, enrolled.NodeID)
		if err != nil {
			t.Fatalf("create challenge: %v", err)
		}
		proof, err := nodeAuthProof(enrolled.NodeSecret, enrolled.NodeID, nonce)
		if err != nil {
			t.Fatal(err)
		}
		token, _, err := service.issueNodeToken(ctx, source, enrolled.NodeID, nonce, proof)
		if err != nil {
			t.Fatalf("issue access token: %v", err)
		}
		claims, err := service.verifyAccessToken(token, time.Now().UTC())
		if err != nil {
			t.Fatalf("verify access token: %v", err)
		}
		cleanupKeys = append(cleanupKeys, "cv:v1:node-jti:"+claims.TokenID,
			service.nodeRateKey("challenge", source+"\n"+enrolled.NodeID),
			service.nodeRateKey("token", source+"\n"+enrolled.NodeID))
		return token
	}

	firstToken := issue()
	claims, err := service.verifyAccessToken(firstToken, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Exercise consumption near expiry: the Redis replay key must retain at least
	// one second even when the HTTP request is processed late in the token life.
	identity, err := service.consumeAccessToken(ctx, firstToken, time.Unix(claims.ExpiresAt-1, 0))
	if err != nil || identity.NodeID != enrolled.NodeID || identity.OwnerID != ownerID.String() {
		t.Fatalf("consume access token identity=%+v err=%v", identity, err)
	}
	if _, err := service.consumeAccessToken(ctx, firstToken, time.Now().UTC()); !errors.Is(err, ErrNodeUnauthorized) {
		t.Fatalf("replayed access token err=%v, want unauthorized", err)
	}

	revocationToken := issue()
	record, err := service.getNode(ctx, enrolled.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	record.Revoked = true
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, nodeRecordKey(enrolled.NodeID), encoded, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.consumeAccessToken(ctx, revocationToken, time.Now().UTC()); !errors.Is(err, ErrNodeUnauthorized) {
		t.Fatalf("revoked node access token err=%v, want unauthorized", err)
	}
}

func TestNodeHTTPRoutesAreOptInAndPendingPollUses428(t *testing.T) {
	client := idempotencyTestRedis(t)
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	baseConfig := Config{Origin: "https://nodes.example.test", PrivacyNoticeVersion: "v1", Pepper: []byte(strings.Repeat("p", 32))}
	store, err := NewStore(client, baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := NewHandler(baseConfig, store, inference.Mock{})
	if err != nil {
		t.Fatal(err)
	}
	missing := httptest.NewRecorder()
	disabled.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/node/enrollments", strings.NewReader(`{}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("node route without opt-in keys returned %d", missing.Code)
	}

	config := baseConfig
	config.NodeSecretEncryptionKey = []byte(strings.Repeat("e", 32))
	config.NodeJWTSigningKey = signingKey
	service, err := newNodeService(client, config)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := NewHandler(config, store, inference.Mock{})
	if err != nil {
		t.Fatal(err)
	}
	remote := fmt.Sprintf("198.51.100.%d:1234", time.Now().UnixNano()%200+1)
	source, _, err := net.SplitHostPort(remote)
	if err != nil {
		t.Fatal(err)
	}
	var createdEnrollmentID, createdDeviceCode, createdUserCode string
	var ownerID, sessionID string
	var extraCleanup []string
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanCancel()
		keys := []string{service.nodeRateKey("enroll", source), service.nodeRateKey("poll", source),
			service.nodeRateKey("challenge", source+"\n00000000-0000-4000-8000-000000000001")}
		if ownerID != "" {
			keys = append(keys, service.nodeRateKey("approve", ownerID))
		}
		keys = append(keys, extraCleanup...)
		if createdEnrollmentID != "" {
			keys = append(keys, nodeEnrollmentPrefix+createdEnrollmentID,
				nodeEnrollmentDeviceKey+service.verifier("node-device-v1", createdDeviceCode),
				nodeEnrollmentUserKey+service.verifier("node-user-v1", createdUserCode))
		}
		_ = client.Del(cleanCtx, keys...).Err()
	})
	create := httptest.NewRequest(http.MethodPost, "/api/node/enrollments", strings.NewReader(
		`{"hwid_hash":"`+strings.Repeat("a", 64)+`","node_version":"1.0.0","device_name":"http-test"}`))
	create.RemoteAddr = remote
	create.Header.Set("Content-Type", "application/json")
	createdResponse := httptest.NewRecorder()
	enabled.ServeHTTP(createdResponse, create)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create enrollment status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created struct {
		EnrollmentID    string `json:"enrollment_id"`
		UserCode        string `json:"user_code"`
		DeviceCode      string `json:"device_code"`
		VerificationURI string `json:"verification_uri"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.EnrollmentID == "" || created.DeviceCode == "" || created.UserCode == "" {
		t.Fatal("enrollment response omitted required codes")
	}
	if created.VerificationURI != config.Origin+"/dashboard/nodes" {
		t.Fatalf("verification URI=%q, want authenticated node approval page", created.VerificationURI)
	}
	createdEnrollmentID, createdDeviceCode, createdUserCode = created.EnrollmentID, created.DeviceCode, created.UserCode

	poll := httptest.NewRequest(http.MethodPost, "/api/node/enrollments/token", strings.NewReader(`{"device_code":"`+created.DeviceCode+`"}`))
	poll.RemoteAddr = remote
	poll.Header.Set("Content-Type", "application/json")
	poll.Header.Set("Authorization", "Bearer "+created.DeviceCode)
	pollResponse := httptest.NewRecorder()
	enabled.ServeHTTP(pollResponse, poll)
	if pollResponse.Code != http.StatusPreconditionRequired {
		t.Fatalf("pending enrollment poll status=%d body=%s", pollResponse.Code, pollResponse.Body.String())
	}

	approve := httptest.NewRequest(http.MethodPost, "/api/node/enrollments/approve", strings.NewReader(`{"user_code":"`+created.UserCode+`"}`))
	approve.RemoteAddr = remote
	approve.Header.Set("Content-Type", "application/json")
	approve.Header.Set("Origin", config.Origin)
	approveResponse := httptest.NewRecorder()
	enabled.ServeHTTP(approveResponse, approve)
	if approveResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approval status=%d body=%s", approveResponse.Code, approveResponse.Body.String())
	}

	owner, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ownerID = owner.String()
	username := "owner-" + strings.ReplaceAll(ownerID[:8], "-", "")
	account, err := json.Marshal(accountRecord{ID: ownerID, Username: username, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), PrivacyNoticeVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), accountPrefix+ownerID, account, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), usernamePrefix+username, ownerID, 0).Err(); err != nil {
		t.Fatal(err)
	}
	ownerCookie, csrfToken, _, err := store.CreateSession(context.Background(), ownerID)
	if err != nil {
		t.Fatal(err)
	}
	sessionID = strings.TrimPrefix(ownerCookie, "cv1_")
	extraCleanup = append(extraCleanup, accountPrefix+ownerID, usernamePrefix+username, store.sessionKey(sessionID), "cv:v1:sessions:"+ownerID)

	badCSRF := httptest.NewRequest(http.MethodPost, "/api/node/enrollments/approve", strings.NewReader(`{"user_code":"`+created.UserCode+`"}`))
	badCSRF.RemoteAddr = remote
	badCSRF.Header.Set("Content-Type", "application/json")
	badCSRF.Header.Set("Origin", config.Origin)
	badCSRF.AddCookie(&http.Cookie{Name: sessionCookie, Value: ownerCookie})
	badCSRF.Header.Set("X-CSRF-Token", strings.Repeat("x", 32))
	badCSRFResponse := httptest.NewRecorder()
	enabled.ServeHTTP(badCSRFResponse, badCSRF)
	if badCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("approval with invalid CSRF status=%d", badCSRFResponse.Code)
	}

	approve = httptest.NewRequest(http.MethodPost, "/api/node/enrollments/approve", strings.NewReader(`{"user_code":"`+created.UserCode+`"}`))
	approve.RemoteAddr = remote
	approve.Header.Set("Content-Type", "application/json")
	approve.Header.Set("Origin", config.Origin)
	approve.Header.Set("X-CSRF-Token", csrfToken)
	approve.AddCookie(&http.Cookie{Name: sessionCookie, Value: ownerCookie})
	approveResponse = httptest.NewRecorder()
	enabled.ServeHTTP(approveResponse, approve)
	if approveResponse.Code != http.StatusNoContent {
		t.Fatalf("owner approval status=%d body=%s", approveResponse.Code, approveResponse.Body.String())
	}

	poll = httptest.NewRequest(http.MethodPost, "/api/node/enrollments/token", strings.NewReader(`{"device_code":"`+created.DeviceCode+`"}`))
	poll.RemoteAddr = remote
	poll.Header.Set("Content-Type", "application/json")
	poll.Header.Set("Authorization", "Bearer "+created.DeviceCode)
	pollResponse = httptest.NewRecorder()
	enabled.ServeHTTP(pollResponse, poll)
	if pollResponse.Code != http.StatusOK {
		t.Fatalf("approved enrollment poll status=%d body=%s", pollResponse.Code, pollResponse.Body.String())
	}
	var enrolled nodeEnrollmentToken
	if err := json.Unmarshal(pollResponse.Body.Bytes(), &enrolled); err != nil || len(enrolled.NodeSecret) != 64 {
		t.Fatalf("approved enrollment token=%+v err=%v", enrolled, err)
	}
	extraCleanup = append(extraCleanup, nodeRecordKey(enrolled.NodeID))

	challenge := httptest.NewRequest(http.MethodPost, "/api/node/auth/challenge", strings.NewReader(`{"node_id":"00000000-0000-4000-8000-000000000001"}`))
	challenge.RemoteAddr = remote
	challenge.Header.Set("Content-Type", "application/json")
	challengeResponse := httptest.NewRecorder()
	enabled.ServeHTTP(challengeResponse, challenge)
	if challengeResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unknown node challenge status=%d body=%s", challengeResponse.Code, challengeResponse.Body.String())
	}
}
