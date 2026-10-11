package m1

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"cordova/server/internal/domain"

	"github.com/redis/go-redis/v9"
)

const (
	nodeEnrollmentPrefix    = "cv:v1:node-enrollment:"
	nodeEnrollmentDeviceKey = "cv:v1:node-enrollment-device:"
	nodeEnrollmentUserKey   = "cv:v1:node-enrollment-user:"
	nodeChallengePrefix     = "cv:v1:node-challenge:"
)

type nodeEnrollmentRecord struct {
	ID             string `json:"id"`
	NodeID         string `json:"node_id"`
	NodeName       string `json:"node_name"`
	NodeVersion    string `json:"node_version"`
	HWIDHash       string `json:"hwid_hash"`
	DeviceVerifier string `json:"device_verifier"`
	UserVerifier   string `json:"user_verifier"`
	SecretCipher   string `json:"secret_ciphertext"`
	Status         string `json:"status"`
	OwnerID        string `json:"owner_id"`
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at"`
	ExpiresAtUnix  int64  `json:"expires_at_unix"`
	ApprovedAt     string `json:"approved_at"`
	TokenReadUntil string `json:"token_read_until"`
}

type nodeEnrollmentCreated struct {
	EnrollmentID        string
	UserCode            string
	DeviceCode          string
	VerificationURI     string
	ExpiresAt           time.Time
	PollIntervalSeconds int
}

type nodeEnrollmentToken struct {
	NodeID       string `json:"node_id"`
	NodeSecret   string `json:"node_secret"`
	ServerOrigin string `json:"server_origin"`
}

func (s *nodeService) createEnrollment(ctx context.Context, source, name, version, hwidHash string) (nodeEnrollmentCreated, error) {
	if err := s.allow(ctx, s.nodeRateKey("enroll", source), 10); err != nil {
		return nodeEnrollmentCreated{}, err
	}
	if !validNodeHash(hwidHash) || !validNodeText(name, 64) || !validNodeText(version, 64) {
		return nodeEnrollmentCreated{}, ErrInvalid
	}
	enrollmentID, err := domain.NewID()
	if err != nil {
		return nodeEnrollmentCreated{}, err
	}
	nodeID, err := domain.NewID()
	if err != nil {
		return nodeEnrollmentCreated{}, err
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nodeEnrollmentCreated{}, err
	}
	nodeSecret := hex.EncodeToString(secretBytes)
	deviceBytes := make([]byte, 32)
	if _, err := rand.Read(deviceBytes); err != nil {
		return nodeEnrollmentCreated{}, err
	}
	deviceCode := hex.EncodeToString(deviceBytes)
	userBytes := make([]byte, 8)
	if _, err := rand.Read(userBytes); err != nil {
		return nodeEnrollmentCreated{}, err
	}
	userCode := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(userBytes)
	userCode = userCode[:5] + "-" + userCode[5:10] + "-" + userCode[10:]
	ciphertext, err := s.encryptNodeSecret(nodeID.String(), nodeSecret)
	if err != nil {
		return nodeEnrollmentCreated{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(nodeEnrollmentTTL)
	record := nodeEnrollmentRecord{ID: enrollmentID.String(), NodeID: nodeID.String(), NodeName: name,
		NodeVersion: version, HWIDHash: hwidHash, DeviceVerifier: s.verifier("node-device-v1", deviceCode),
		UserVerifier: s.verifier("node-user-v1", userCode), SecretCipher: ciphertext, Status: "pending",
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano), ExpiresAtUnix: expires.Unix()}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nodeEnrollmentCreated{}, err
	}
	result, err := s.redis.Eval(ctx, `
local record_type=redis.call('TYPE',KEYS[1]).ok
local device_type=redis.call('TYPE',KEYS[2]).ok
local user_type=redis.call('TYPE',KEYS[3]).ok
if (record_type~='none' and record_type~='string') or (device_type~='none' and device_type~='string') or (user_type~='none' and user_type~='string') then return redis.error_reply('node enrollment key type mismatch') end
if redis.call('EXISTS',KEYS[1])==1 or redis.call('EXISTS',KEYS[2])==1 or redis.call('EXISTS',KEYS[3])==1 then return 0 end
redis.call('SET',KEYS[1],ARGV[1],'EX',ARGV[4])
redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[4])
redis.call('SET',KEYS[3],ARGV[3],'EX',ARGV[4])
return 1`, []string{nodeEnrollmentPrefix + record.ID,
		nodeEnrollmentDeviceKey + record.DeviceVerifier, nodeEnrollmentUserKey + record.UserVerifier},
		string(encoded), record.ID, record.ID, int64(nodeEnrollmentTTL.Seconds())).Int()
	if err != nil {
		return nodeEnrollmentCreated{}, err
	}
	if result != 1 {
		return nodeEnrollmentCreated{}, errors.New("node enrollment identifier collision")
	}
	return nodeEnrollmentCreated{EnrollmentID: record.ID, UserCode: userCode, DeviceCode: deviceCode,
		VerificationURI: s.origin + "/dashboard/nodes", ExpiresAt: expires, PollIntervalSeconds: 5}, nil
}

func (s *nodeService) approveEnrollment(ctx context.Context, accountID, userCode string) error {
	_, err := s.approveEnrollmentWithName(ctx, accountID, userCode, "")
	return err
}

func (s *nodeService) approveEnrollmentWithName(ctx context.Context, accountID, userCode, name string) (nodeRecord, error) {
	if !validID(accountID) {
		return nodeRecord{}, ErrNodeUnauthorized
	}
	if err := s.allow(ctx, s.nodeRateKey("approve", accountID), 10); err != nil {
		return nodeRecord{}, err
	}
	userCode = strings.ToUpper(strings.TrimSpace(userCode))
	if !validNodeText(userCode, 32) || strings.Count(userCode, "-") != 2 || (name != "" && !validNodeText(name, 64)) {
		return nodeRecord{}, ErrInvalid
	}
	userVerifier := s.verifier("node-user-v1", userCode)
	indexKey := nodeEnrollmentUserKey + userVerifier
	enrollmentID, err := s.redis.Get(ctx, indexKey).Result()
	if err == redis.Nil {
		return nodeRecord{}, ErrNodeExpired
	}
	if err != nil {
		return nodeRecord{}, err
	}
	data, err := s.redis.Get(ctx, nodeEnrollmentPrefix+enrollmentID).Bytes()
	if err == redis.Nil {
		return nodeRecord{}, ErrNodeExpired
	}
	if err != nil {
		return nodeRecord{}, err
	}
	var record nodeEnrollmentRecord
	if decodeStrictBytes(data, &record) != nil || record.ID != enrollmentID || record.UserVerifier != userVerifier ||
		!validID(record.NodeID) || record.Status != "pending" {
		return nodeRecord{}, ErrNodeConflict
	}
	expires, err := time.Parse(time.RFC3339Nano, record.ExpiresAt)
	if err != nil || !expires.After(time.Now()) {
		return nodeRecord{}, ErrNodeExpired
	}
	if name != "" {
		record.NodeName = name
	}
	now := time.Now().UTC()
	dailyApprovalKey := s.nodeRateKey("approve-daily", accountID+"\n"+now.Format("2006-01-02"))
	record.Status, record.OwnerID = "approved", accountID
	record.ApprovedAt = now.Format(time.RFC3339Nano)
	record.TokenReadUntil = now.Add(nodeEnrollmentTTL).Format(time.RFC3339Nano)
	updated, err := json.Marshal(record)
	if err != nil {
		return nodeRecord{}, err
	}
	node := nodeRecord{ID: record.NodeID, OwnerID: accountID, Name: record.NodeName, HWIDHash: record.HWIDHash,
		NodeVersion: record.NodeVersion, SecretCipher: record.SecretCipher, SecretVersion: 1, CreatedAt: record.CreatedAt,
		ApprovedAt: record.ApprovedAt}
	member, err := nodeIndexMember(node)
	if err != nil {
		return nodeRecord{}, err
	}
	nodeData, err := json.Marshal(node)
	if err != nil {
		return nodeRecord{}, err
	}
	result, err := s.redis.Eval(ctx, `
local enrollment_type=redis.call('TYPE',KEYS[1]).ok
local node_type=redis.call('TYPE',KEYS[2]).ok
local device_type=redis.call('TYPE',KEYS[3]).ok
local user_type=redis.call('TYPE',KEYS[4]).ok
local index_type=redis.call('TYPE',KEYS[5]).ok
local daily_type=redis.call('TYPE',KEYS[6]).ok
local revision_type=redis.call('TYPE',KEYS[7]).ok
local live_type=redis.call('TYPE',KEYS[8]).ok
if enrollment_type~='string' or (node_type~='none' and node_type~='string') or (device_type~='none' and device_type~='string') or user_type~='string' or
   (index_type~='none' and index_type~='zset') or (daily_type~='none' and daily_type~='string') or
   (revision_type~='none' and revision_type~='string') or (live_type~='none' and live_type~='stream') then return redis.error_reply('node approval key type mismatch') end
local ok,current=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not ok or type(current)~='table' or current['id']~=ARGV[1] or current['node_id']~=ARGV[2] or current['user_verifier']~=ARGV[3] then return -1 end
if current['status']~='pending' then return 0 end
if tonumber(current['expires_at_unix'])<=tonumber(ARGV[6]) then return -1 end
if redis.call('EXISTS',KEYS[2])==1 then return redis.error_reply('node identifier already exists') end
if tonumber(redis.call('GET',KEYS[6]) or '0')>=100 then return -2 end
if redis.call('INCR',KEYS[6])==1 then redis.call('EXPIRE',KEYS[6],ARGV[10]) end
redis.call('SET',KEYS[2],ARGV[4])
redis.call('SET',KEYS[1],ARGV[5],'EX',ARGV[7])
redis.call('SET',KEYS[3],ARGV[8],'EX',ARGV[7])
redis.call('DEL',KEYS[4])
redis.call('ZADD',KEYS[5],0,ARGV[9])
local revision=redis.call('INCR',KEYS[7])
redis.call('XADD',KEYS[8],'MAXLEN',10000,'*','topic','nodes','resource_id',ARGV[2],'revision',tostring(revision),'action','invalidate')
local now=redis.call('TIME')
local cutoff=string.format('%.0f',tonumber(now[1])*1000-86400000)..'-0'
redis.call('XTRIM',KEYS[8],'MINID',cutoff)
redis.call('EXPIRE',KEYS[8],86400)
return 1`, []string{nodeEnrollmentPrefix + enrollmentID, nodeRecordKey(record.NodeID),
		nodeEnrollmentDeviceKey + record.DeviceVerifier, indexKey, nodeOwnerIndexKey(accountID), dailyApprovalKey,
		nodeRevisionKey(record.NodeID), accountLiveKey(accountID)}, enrollmentID, record.NodeID, userVerifier,
		string(nodeData), string(updated), now.Unix(), int64(nodeEnrollmentTTL.Seconds()), enrollmentID, member, int64(48*time.Hour/time.Second)).Int()
	if err != nil {
		return nodeRecord{}, err
	}
	switch result {
	case 1:
		return node, nil
	case -1:
		return nodeRecord{}, ErrNodeExpired
	case -2:
		return nodeRecord{}, ErrNodeThrottled
	default:
		return nodeRecord{}, ErrNodeConflict
	}
}

func (s *nodeService) pollEnrollment(ctx context.Context, deviceCode string) (nodeEnrollmentToken, error) {
	if len(deviceCode) != 64 {
		return nodeEnrollmentToken{}, ErrNodeUnauthorized
	}
	if _, err := hex.DecodeString(deviceCode); err != nil {
		return nodeEnrollmentToken{}, ErrNodeUnauthorized
	}
	verifier := s.verifier("node-device-v1", deviceCode)
	enrollmentID, err := s.redis.Get(ctx, nodeEnrollmentDeviceKey+verifier).Result()
	if err == redis.Nil {
		return nodeEnrollmentToken{}, ErrNodeExpired
	}
	if err != nil {
		return nodeEnrollmentToken{}, err
	}
	data, err := s.redis.Get(ctx, nodeEnrollmentPrefix+enrollmentID).Bytes()
	if err == redis.Nil {
		return nodeEnrollmentToken{}, ErrNodeExpired
	}
	if err != nil {
		return nodeEnrollmentToken{}, err
	}
	var record nodeEnrollmentRecord
	if decodeStrictBytes(data, &record) != nil || record.ID != enrollmentID || record.DeviceVerifier != verifier {
		return nodeEnrollmentToken{}, ErrNodeUnauthorized
	}
	if record.Status == "pending" {
		expires, err := time.Parse(time.RFC3339Nano, record.ExpiresAt)
		if err != nil || !expires.After(time.Now()) {
			return nodeEnrollmentToken{}, ErrNodeExpired
		}
		return nodeEnrollmentToken{}, ErrNodePending
	}
	if record.Status != "approved" || !validID(record.NodeID) || !validID(record.OwnerID) {
		return nodeEnrollmentToken{}, ErrNodeUnauthorized
	}
	readUntil, err := time.Parse(time.RFC3339Nano, record.TokenReadUntil)
	if err != nil || !readUntil.After(time.Now()) {
		return nodeEnrollmentToken{}, ErrNodeExpired
	}
	secret, err := s.decryptNodeSecret(record.NodeID, record.SecretCipher)
	if err != nil {
		return nodeEnrollmentToken{}, err
	}
	return nodeEnrollmentToken{NodeID: record.NodeID, NodeSecret: secret, ServerOrigin: s.origin}, nil
}

func (s *nodeService) createChallenge(ctx context.Context, source, nodeID string) (string, time.Time, error) {
	if !validID(nodeID) {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	if err := s.allow(ctx, s.nodeRateKey("challenge", source+"\n"+nodeID), 5); err != nil {
		return "", time.Time{}, err
	}
	record, err := s.getNode(ctx, nodeID)
	if err != nil || record.Revoked || record.SecretVersion < 1 {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	nonce, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	key := nodeChallengePrefix + s.verifier("node-nonce-v1", nonce)
	created, err := s.redis.SetNX(ctx, key, nodeID, nodeNonceTTL).Result()
	if err != nil {
		return "", time.Time{}, err
	}
	if !created {
		return "", time.Time{}, errors.New("node challenge collision")
	}
	return nonce, time.Now().UTC().Add(nodeNonceTTL), nil
}

func (s *nodeService) issueNodeToken(ctx context.Context, source, nodeID, nonce, proof string) (string, time.Time, error) {
	if !validID(nodeID) || len(nonce) < 32 || len(proof) != 64 {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	if err := s.allow(ctx, s.nodeRateKey("token", source+"\n"+nodeID), 10); err != nil {
		return "", time.Time{}, err
	}
	nonceKey := nodeChallengePrefix + s.verifier("node-nonce-v1", nonce)
	consumed, err := s.redis.Eval(ctx, `
local key_type=redis.call('TYPE',KEYS[1]).ok
if key_type~='none' and key_type~='string' then return redis.error_reply('node challenge key type mismatch') end
local value=redis.call('GET',KEYS[1])
if not value then return false end
redis.call('DEL',KEYS[1])
return value`, []string{nonceKey}).Result()
	if err != nil || consumed != nodeID {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	record, err := s.getNode(ctx, nodeID)
	if err != nil || record.Revoked || record.SecretVersion < 1 {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	secret, err := s.decryptNodeSecret(nodeID, record.SecretCipher)
	if err != nil {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	expected, err := nodeAuthProof(secret, nodeID, nonce)
	if err != nil || !hmac.Equal([]byte(expected), []byte(proof)) {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	return s.issueAccessToken(nodeID, record.SecretVersion, time.Now().UTC())
}

func (s *nodeService) getNode(ctx context.Context, nodeID string) (nodeRecord, error) {
	data, err := s.redis.Get(ctx, nodeRecordKey(nodeID)).Bytes()
	if err == redis.Nil {
		return nodeRecord{}, ErrNodeUnauthorized
	}
	if err != nil {
		return nodeRecord{}, err
	}
	var record nodeRecord
	if decodeStrictBytes(data, &record) != nil || record.ID != nodeID || !validID(record.OwnerID) {
		return nodeRecord{}, ErrNodeUnauthorized
	}
	return record, nil
}

func validNodeText(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}

func decodeNodeBody(r *http.Request, target any) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBody+1))
	if err != nil || len(body) == 0 || len(body) > maxAuthBody {
		return ErrInvalid
	}
	if err := decodeStrictBytes(body, target); err != nil {
		return ErrInvalid
	}
	return nil
}

func (h *Handler) createNodeEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		HWIDHash    string `json:"hwid_hash"`
		NodeVersion string `json:"node_version"`
		DeviceName  string `json:"device_name"`
	}
	if decodeNodeBody(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node enrollment request is invalid.")
		return
	}
	created, err := h.nodes.createEnrollment(r.Context(), remoteAddress(r, h.cfg.TrustProxy),
		strings.TrimSpace(input.DeviceName), strings.TrimSpace(input.NodeVersion), input.HWIDHash)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request", "The node enrollment request is invalid.")
		} else {
			writeNodeError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"enrollment_id": created.EnrollmentID, "user_code": created.UserCode,
		"device_code": created.DeviceCode, "verification_uri": created.VerificationURI,
		"expires_at": created.ExpiresAt.Format(time.RFC3339Nano), "poll_interval_seconds": created.PollIntervalSeconds})
}

func (h *Handler) pollNodeEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		DeviceCode string `json:"device_code"`
	}
	if decodeNodeBody(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node enrollment token request is invalid.")
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !hmac.Equal([]byte(input.DeviceCode), []byte(bearer)) {
		writeNodeError(w, ErrNodeUnauthorized)
		return
	}
	if err := h.nodes.allow(r.Context(), h.nodes.nodeRateKey("poll", remoteAddress(r, h.cfg.TrustProxy)), 30); err != nil {
		writeNodeError(w, err)
		return
	}
	token, err := h.nodes.pollEnrollment(r.Context(), input.DeviceCode)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, token)
}

func (h *Handler) approveNodeEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	principal, authenticated := h.authenticate(r)
	if !authenticated || principal.Kind != "session" || principal.AccountID == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "An owner session is required.")
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "An owner session is required.")
		return
	}
	session, account, ok := h.store.Session(r.Context(), cookie.Value)
	if !ok || account.ID != principal.AccountID {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "An owner session is required.")
		return
	}
	if !h.csrf(r, session) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Reload the page and try again.")
		return
	}
	var input struct {
		UserCode string `json:"user_code"`
	}
	if decodeNodeBody(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node approval request is invalid.")
		return
	}
	if err := h.nodes.approveEnrollment(r.Context(), account.ID, input.UserCode); err != nil {
		if errors.Is(err, ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request", "The node approval request is invalid.")
		} else {
			writeNodeError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) nodeAuthChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		NodeID string `json:"node_id"`
	}
	if decodeNodeBody(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node authentication request is invalid.")
		return
	}
	nonce, expires, err := h.nodes.createChallenge(r.Context(), remoteAddress(r, h.cfg.TrustProxy), input.NodeID)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"nonce": nonce, "expires_at": expires.Format(time.RFC3339Nano)})
}

func (h *Handler) nodeAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		NodeID string `json:"node_id"`
		Nonce  string `json:"nonce"`
		Proof  string `json:"proof"`
	}
	if decodeNodeBody(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node authentication request is invalid.")
		return
	}
	token, expires, err := h.nodes.issueNodeToken(r.Context(), remoteAddress(r, h.cfg.TrustProxy), input.NodeID, input.Nonce, input.Proof)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "expires_at": expires.Format(time.RFC3339Nano)})
}
