package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
	"github.com/redis/go-redis/v9"
)

func TestNodeInventoryOwnershipPaginationRenameAndRevoke(t *testing.T) {
	client := idempotencyTestRedis(t)
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Origin: "https://nodes.example.test", PrivacyNoticeVersion: "v1", Pepper: []byte(strings.Repeat("p", 32)),
		NodeSecretEncryptionKey: []byte(strings.Repeat("e", 32)), NodeJWTSigningKey: signingKey}
	store, err := NewStore(client, config)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newNodeService(client, config)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(config, store, inference.Mock{})
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	createOwner := func(id string) (string, string) {
		t.Helper()
		username := "node-owner-" + strings.ReplaceAll(id[:8], "-", "")
		account, err := json.Marshal(accountRecord{ID: id, Username: username, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), PrivacyNoticeVersion: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Set(context.Background(), accountPrefix+id, account, 0).Err(); err != nil {
			t.Fatal(err)
		}
		if err := client.Set(context.Background(), usernamePrefix+username, id, 0).Err(); err != nil {
			t.Fatal(err)
		}
		cookie, csrf, _, err := store.CreateSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return cookie, csrf
	}
	ownerCookie, ownerCSRF := createOwner(ownerID.String())
	otherCookie, _ := createOwner(otherID.String())
	remote := fmt.Sprintf("198.51.100.%d:1234", time.Now().UnixNano()%200+1)
	source, _, err := net.SplitHostPort(remote)
	if err != nil {
		t.Fatal(err)
	}
	var records []nodeRecord
	var enrollments []nodeEnrollmentCreated
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		keys := []string{accountPrefix + ownerID.String(), usernamePrefix + "node-owner-" + strings.ReplaceAll(ownerID.String()[:8], "-", ""),
			accountPrefix + otherID.String(), usernamePrefix + "node-owner-" + strings.ReplaceAll(otherID.String()[:8], "-", ""),
			store.sessionKey(strings.TrimPrefix(ownerCookie, "cv1_")), store.sessionKey(strings.TrimPrefix(otherCookie, "cv1_")),
			"cv:v1:sessions:" + ownerID.String(), "cv:v1:sessions:" + otherID.String(),
			nodeOwnerIndexKey(ownerID.String()), nodeOwnerIndexReadyKey(ownerID.String()),
			nodeOwnerIndexKey(otherID.String()), nodeOwnerIndexReadyKey(otherID.String()),
			service.nodeRateKey("enroll", source), service.nodeRateKey("approve", ownerID.String())}
		for _, enrollment := range enrollments {
			keys = append(keys, nodeEnrollmentPrefix+enrollment.EnrollmentID,
				nodeEnrollmentDeviceKey+service.verifier("node-device-v1", enrollment.DeviceCode),
				nodeEnrollmentUserKey+service.verifier("node-user-v1", enrollment.UserCode))
		}
		for _, record := range records {
			member, _ := nodeIndexMember(record)
			keys = append(keys, nodeRecordKey(record.ID), nodeSessionKey(record.ID), nodeHeartbeatKey(record.ID),
				nodeGenerationKey(record.ID), nodeRateKeyForCleanup(service, ownerID.String(), record.ID))
			_ = member
		}
		_ = client.Del(ctx, keys...).Err()
	})

	request := func(method, path, body, cookie, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = remote
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		if method != http.MethodGet {
			req.Header.Set("Origin", config.Origin)
			if csrf != "" {
				req.Header.Set("X-CSRF-Token", csrf)
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	newEnrollment := func(name string) nodeEnrollmentCreated {
		t.Helper()
		enrollment, err := service.createEnrollment(context.Background(), source, name, "1.0.0", strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		enrollments = append(enrollments, enrollment)
		return enrollment
	}
	link := func(enrollment nodeEnrollmentCreated, name string) nodeView {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"user_code": enrollment.UserCode, "name": name})
		response := request(http.MethodPost, "/api/nodes/link", string(body), ownerCookie, ownerCSRF)
		if response.Code != http.StatusCreated {
			t.Fatalf("link status=%d body=%s", response.Code, response.Body.String())
		}
		var view nodeView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		var record nodeRecord
		if err := json.Unmarshal(mustNodeBytes(t, client, view.ID), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		return view
	}

	if response := request(http.MethodGet, "/api/nodes", "", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status=%d", response.Code)
	}
	if response := request(http.MethodGet, "/api/nodes?limit=0", "", ownerCookie, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status=%d", response.Code)
	}
	empty := request(http.MethodGet, "/api/nodes", "", ownerCookie, "")
	if empty.Code != http.StatusOK || empty.Body.String() != "{\"data\":[],\"next_cursor\":null}\n" {
		t.Fatalf("empty inventory status=%d body=%s", empty.Code, empty.Body.String())
	}
	pending := newEnrollment("device-one")
	missingName, _ := json.Marshal(map[string]string{"user_code": pending.UserCode})
	if response := request(http.MethodPost, "/api/nodes/link", string(missingName), ownerCookie, ownerCSRF); response.Code != http.StatusBadRequest {
		t.Fatalf("missing node name status=%d body=%s", response.Code, response.Body.String())
	}
	first := link(pending, "workstation one")
	if first.Name != "workstation one" || first.Status != "enrolling" || first.Hardware != nil || first.Rank != nil || first.EarningsPendingPiconero != nil {
		t.Fatalf("unmeasured node snapshot=%+v", first)
	}
	second := link(newEnrollment("device-two"), "workstation two")
	if first.ID == second.ID {
		t.Fatal("two enrollment codes produced the same node")
	}

	pageOne := request(http.MethodGet, "/api/nodes?limit=1", "", ownerCookie, "")
	if pageOne.Code != http.StatusOK {
		t.Fatalf("first page status=%d body=%s", pageOne.Code, pageOne.Body.String())
	}
	var firstPage nodeInventoryPage
	if err := json.Unmarshal(pageOne.Body.Bytes(), &firstPage); err != nil || len(firstPage.Data) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("first page=%+v err=%v", firstPage, err)
	}
	pageTwo := request(http.MethodGet, "/api/nodes?limit=1&cursor="+*firstPage.NextCursor, "", ownerCookie, "")
	var secondPage nodeInventoryPage
	if pageTwo.Code != http.StatusOK || json.Unmarshal(pageTwo.Body.Bytes(), &secondPage) != nil || len(secondPage.Data) != 1 || secondPage.NextCursor != nil {
		t.Fatalf("second page status=%d body=%s page=%+v", pageTwo.Code, pageTwo.Body.String(), secondPage)
	}
	if response := request(http.MethodGet, "/api/nodes?limit=1&cursor="+*firstPage.NextCursor+"x", "", ownerCookie, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor status=%d", response.Code)
	}

	otherDetail := request(http.MethodGet, "/api/nodes/"+first.ID, "", otherCookie, "")
	if otherDetail.Code != http.StatusNotFound {
		t.Fatalf("cross-account detail status=%d body=%s", otherDetail.Code, otherDetail.Body.String())
	}
	wrongCSRF := request(http.MethodPatch, "/api/nodes/"+first.ID, `{"name":"stolen"}`, ownerCookie, strings.Repeat("x", 32))
	if wrongCSRF.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF rename status=%d", wrongCSRF.Code)
	}
	renamed := request(http.MethodPatch, "/api/nodes/"+first.ID, `{"name":"renamed-node"}`, ownerCookie, ownerCSRF)
	if renamed.Code != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	var renamedNode nodeView
	if err := json.Unmarshal(renamed.Body.Bytes(), &renamedNode); err != nil || renamedNode.Name != "renamed-node" {
		t.Fatalf("renamed node=%+v err=%v", renamedNode, err)
	}

	if err := client.Set(context.Background(), nodeSessionKey(second.ID), "synthetic-session:1", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), nodeHeartbeatKey(second.ID), "synthetic-heartbeat", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	revoke := request(http.MethodPost, "/api/nodes/"+second.ID+"/revoke", `{}`, ownerCookie, ownerCSRF)
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	var revokedRecord nodeRecord
	if err := json.Unmarshal(mustNodeBytes(t, client, second.ID), &revokedRecord); err != nil || !revokedRecord.Revoked || revokedRecord.SecretVersion != 2 {
		t.Fatalf("revoked record=%+v err=%v", revokedRecord, err)
	}
	if count, err := client.Exists(context.Background(), nodeSessionKey(second.ID), nodeHeartbeatKey(second.ID)).Result(); err != nil || count != 0 {
		t.Fatalf("revoked session/heartbeat still exist: count=%d err=%v", count, err)
	}
	if response := request(http.MethodGet, "/api/nodes/"+second.ID, "", ownerCookie, ""); response.Code != http.StatusNotFound {
		t.Fatalf("revoked node detail status=%d", response.Code)
	}
	remaining := request(http.MethodGet, "/api/nodes", "", ownerCookie, "")
	var remainingPage nodeInventoryPage
	if remaining.Code != http.StatusOK || json.Unmarshal(remaining.Body.Bytes(), &remainingPage) != nil || len(remainingPage.Data) != 1 || remainingPage.Data[0].ID != first.ID {
		t.Fatalf("inventory after revoke status=%d body=%s page=%+v", remaining.Code, remaining.Body.String(), remainingPage)
	}
}

func mustNodeBytes(t *testing.T, client redis.Cmdable, nodeID string) []byte {
	t.Helper()
	value, err := client.Get(context.Background(), nodeRecordKey(nodeID)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nodeRateKeyForCleanup(service *nodeService, accountID, nodeID string) string {
	return service.nodeRateKey("approve-daily", accountID+"\n"+time.Now().UTC().Format("2006-01-02"))
}
