package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/argon2"
)

const (
	sessionCookie  = "__Host-cordova_session"
	accountPrefix  = "cv:v1:account:"
	usernamePrefix = "cv:v1:username:"
	keyIndexPrefix = "cv:v1:key-index:"
	keyPrefix      = "cv:v1:key:"
	apiKeyPrefix   = "cvk_"
	maxAuthBody    = 16 << 10
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrThrottled = errors.New("throttled")
	ErrInvalid   = errors.New("invalid input")
)

type Config struct {
	InferenceMode          string
	Origin                  string
	PrivacyNoticeVersion    string
	Pepper                  []byte
	NodeSecretEncryptionKey []byte
	NodeJWTSigningKey       ed25519.PrivateKey
	NodeCatalogSnapshot     []byte
	NodeCatalogPublicKey    ed25519.PublicKey
	SecureCookies           bool
	TrustProxy              bool
	UsagePendingKey         string
}

const DefaultPrivacyNoticeVersion = "privacy-v1"

type Store struct {
	redis           *redis.Client
	cfg             Config
	nodeMu          sync.Mutex
	nodes           *nodeService
	argonSlots      chan struct{}
	usagePendingKey string
}

type accountRecord struct {
	ID                   string `json:"id"`
	Username             string `json:"username"`
	PasswordHash         string `json:"password_hash"`
	CreatedAt            string `json:"created_at"`
	PrivacyNoticeVersion string `json:"privacy_notice_version"`
	TOTPEnabled          bool   `json:"totp_enabled"`
}

type Account struct {
	ID                   string `json:"id"`
	Username             string `json:"username"`
	CreatedAt            string `json:"created_at"`
	TOTPEnabled          bool   `json:"totp_enabled"`
	PrivacyNoticeVersion string `json:"privacy_notice_version"`
}

type sessionRecord struct {
	AccountID string `json:"account_id"`
	CSRFHash  string `json:"csrf_hash"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	IdleUntil int64  `json:"idle_until"`
}

type Principal struct {
	AccountID string
	Kind      string
	Scopes    map[string]bool
	KeyID     string
}

type keyRecord struct {
	ID          string   `json:"id"`
	AccountID   string   `json:"account_id"`
	Name        string   `json:"name"`
	Prefix      string   `json:"prefix"`
	Permissions []string `json:"permissions"`
	ModelIDs    []string `json:"model_ids"`
	DailyCap    string   `json:"daily_spend_cap_piconero"`
	CreatedAt   string   `json:"created_at"`
	LastUsedAt  *string  `json:"last_used_at"`
	RevokedAt   *string  `json:"revoked_at"`
	Verifier    string   `json:"verifier"`
}

type usageEvent struct {
	V          int             `json:"v"`
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt string          `json:"occurred_at"`
	ObservedAt string          `json:"observed_at"`
	NodeID     *string         `json:"node_id"`
	RequestID  *string         `json:"request_id"`
	AttemptID  *string         `json:"attempt_id"`
	Sequence   int64           `json:"sequence"`
	Payload    usageProjection `json:"payload"`
}

type usageProjection struct {
	AccountID    string `json:"account_id"`
	APIKeyID     string `json:"api_key_id"`
	ModelID      string `json:"model_id"`
	Status       string `json:"status"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Charged      string `json:"charged_piconero"`
	LatencyMS    int64  `json:"latency_ms"`
	Revision     int64  `json:"revision"`
	CreatedAt    string `json:"created_at"`
	FinishedAt   string `json:"finished_at"`
}

func NewStore(client *redis.Client, cfg Config) (*Store, error) {
	if client == nil || len(cfg.Pepper) < 32 || !validOrigin(cfg.Origin) || !strings.HasPrefix(strings.ToLower(cfg.Origin), "https://") || cfg.PrivacyNoticeVersion == "" || len(cfg.PrivacyNoticeVersion) > 64 {
		return nil, errors.New("M1 store requires Redis, origin, privacy version and a 32-byte pepper")
	}
	if cfg.UsagePendingKey == "" {
		cfg.UsagePendingKey = billing.DefaultUsagePendingKey
	}
	store := &Store{redis: client, cfg: cfg, argonSlots: make(chan struct{}, 4), usagePendingKey: cfg.UsagePendingKey}
	if len(cfg.NodeSecretEncryptionKey) > 0 || len(cfg.NodeJWTSigningKey) > 0 {
		nodes, err := newNodeService(client, cfg)
		if err != nil {
			return nil, err
		}
		store.nodes = nodes
	}
	return store, nil
}

// Claim implements the C2 client idempotency mapping. The client key and
// canonical request body are persisted only as peppered HMAC digests.
func (s *Store) Claim(ctx context.Context, principal api.Principal, clientKey, modelID, requestID string,
	canonicalBody []byte) (api.IdempotencyDecision, error) {
	if principal.Kind != "key" || principal.AccountID == "" || principal.KeyID == "" || clientKey == "" ||
		len(canonicalBody) == 0 || modelID != api.ModelID {
		return api.IdempotencyDecision{}, ErrInvalid
	}
	if _, err := domain.ParseID(principal.AccountID); err != nil {
		return api.IdempotencyDecision{}, ErrInvalid
	}
	if _, err := domain.ParseID(principal.KeyID); err != nil {
		return api.IdempotencyDecision{}, ErrInvalid
	}
	if _, err := domain.ParseID(requestID); err != nil {
		return api.IdempotencyDecision{}, ErrInvalid
	}
	keyHash := s.secretVerifier("idempotency-key-v1\n" + principal.AccountID + "\n" + principal.KeyID + "\n" + clientKey)
	bodyHash := s.secretVerifier("idempotency-body-v1\n" + string(canonicalBody))
	base := "cv:v1:idempotency:" + principal.AccountID + ":" + principal.KeyID + ":" + keyHash
	requestIndex := "cv:v1:idempotency-request:" + requestID
	result, err := s.redis.Eval(ctx, `
local record_type=redis.call('TYPE',KEYS[1]).ok
local body_type=redis.call('TYPE',KEYS[2]).ok
local key_type=redis.call('TYPE',KEYS[3]).ok
local pending_type=redis.call('TYPE',KEYS[4]).ok
local request_type=redis.call('TYPE',KEYS[5]).ok
if (record_type~='none' and record_type~='hash') or (body_type~='none' and body_type~='string') or key_type~='string' or (pending_type~='none' and pending_type~='set') or (request_type~='none' and request_type~='string') then return redis.error_reply('idempotency key type mismatch') end
local decoded,key=pcall(cjson.decode,redis.call('GET',KEYS[3]))
if not decoded or type(key)~='table' or key['id']~=ARGV[1] or key['account_id']~=ARGV[2] or key['revoked_at']~=cjson.null then return cjson.encode({unauthorized=true}) end
local can_chat=false
for _,scope in ipairs(key['permissions'] or {}) do if scope=='chat:write' then can_chat=true end end
local can_model=false
for _,model in ipairs(key['model_ids'] or {}) do if model==ARGV[3] then can_model=true end end
if not can_chat or not can_model then return cjson.encode({unauthorized=true}) end
if record_type=='hash' then
  local original=redis.call('HGET',KEYS[1],'request_id') or ''
  local status=redis.call('HGET',KEYS[1],'status') or 'in_progress'
  local digest=redis.call('GET',KEYS[2])
  return cjson.encode({created=false,request_id=original,status=status,compared=digest~=false,body_match=digest==ARGV[4]})
end
if body_type~='none' then return redis.error_reply('idempotency digest exists without a record') end
if request_type~='none' then return redis.error_reply('idempotency request identifier collision') end
redis.call('HSET',KEYS[1],'request_id',ARGV[5],'status','in_progress','created_at',ARGV[6])
redis.call('SET',KEYS[2],ARGV[4],'EX',2592000)
	redis.call('SET',KEYS[5],KEYS[1])
	redis.call('SADD',KEYS[4],ARGV[5])
	return cjson.encode({created=true,request_id=ARGV[5],status='in_progress',compared=true,body_match=true})`,
		[]string{base, base + ":body", keyPrefix + principal.KeyID, "cv:v1:idempotency-pending", requestIndex}, principal.KeyID, principal.AccountID, modelID,
		bodyHash, requestID, time.Now().UTC().Format(time.RFC3339Nano)).Text()
	if err != nil {
		return api.IdempotencyDecision{}, err
	}
	var response struct {
		Created      bool   `json:"created"`
		Unauthorized bool   `json:"unauthorized"`
		RequestID    string `json:"request_id"`
		Status       string `json:"status"`
		Compared     bool   `json:"compared"`
		BodyMatch    bool   `json:"body_match"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		return api.IdempotencyDecision{}, errors.New("idempotency record is malformed")
	}
	if response.Unauthorized {
		return api.IdempotencyDecision{}, api.ErrIdempotencyUnauthorized
	}
	if response.RequestID == "" || response.Status == "" {
		return api.IdempotencyDecision{}, errors.New("idempotency record is incomplete")
	}
	return api.IdempotencyDecision{Created: response.Created, RequestID: response.RequestID, Status: response.Status,
		Compared: response.Compared, BodyMatch: response.BodyMatch}, nil
}

// Complete marks an idempotency mapping terminal after the accounted request
// and content-free usage projection have been durably acknowledged.
func (s *Store) Complete(ctx context.Context, principal api.Principal, clientKey, requestID, status string) error {
	if principal.Kind != "key" || principal.AccountID == "" || principal.KeyID == "" || clientKey == "" {
		return ErrInvalid
	}
	if status != "settled" && status != "cancelled" && status != "refunded" && status != "failed" {
		return ErrInvalid
	}
	if _, err := domain.ParseID(requestID); err != nil {
		return ErrInvalid
	}
	keyHash := s.secretVerifier("idempotency-key-v1\n" + principal.AccountID + "\n" + principal.KeyID + "\n" + clientKey)
	base := "cv:v1:idempotency:" + principal.AccountID + ":" + principal.KeyID + ":" + keyHash
	return s.completeIdempotencyRecord(ctx, base, requestID, status)
}

func (s *Store) completeIdempotencyRecord(ctx context.Context, recordKey, requestID, status string) error {
	changed, err := s.redis.Eval(ctx, `
local record_type=redis.call('TYPE',KEYS[1]).ok
local pending_type=redis.call('TYPE',KEYS[2]).ok
local request_type=redis.call('TYPE',KEYS[3]).ok
if record_type~='hash' or (pending_type~='none' and pending_type~='set') or (request_type~='none' and request_type~='string') then return redis.error_reply('idempotency completion key type mismatch') end
if redis.call('HGET',KEYS[1],'request_id')~=ARGV[1] then return 0 end
if redis.call('GET',KEYS[3])~=KEYS[1] then return 0 end
local current=redis.call('HGET',KEYS[1],'status')
if current=='settled' or current=='cancelled' or current=='refunded' or current=='failed' then
  if current==ARGV[2] then return 1 end
  return 0
end
redis.call('HSET',KEYS[1],'status',ARGV[2],'completed_at',ARGV[3])
	redis.call('SREM',KEYS[2],ARGV[1])
	redis.call('DEL',KEYS[3])
	return 1`, []string{recordKey, "cv:v1:idempotency-pending", "cv:v1:idempotency-request:" + requestID}, requestID, status, time.Now().UTC().Format(time.RFC3339Nano)).Int()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

// ReconcileIdempotency completes mappings left in progress by a process crash.
// It runs after ledger recovery, before the gateway accepts requests.
func (s *Store) ReconcileIdempotency(ctx context.Context, ledger interface {
	Request(context.Context, string) (billing.RequestCheckpoint, uint64, error)
}) error {
	requestIDs, err := s.redis.SMembers(ctx, "cv:v1:idempotency-pending").Result()
	if err != nil {
		return err
	}
	for _, requestID := range requestIDs {
		if _, err := domain.ParseID(requestID); err != nil {
			return errors.New("idempotency recovery found an invalid request identifier")
		}
		requestIndex := "cv:v1:idempotency-request:" + requestID
		recordKey, getErr := s.redis.Get(ctx, requestIndex).Result()
		if errors.Is(getErr, redis.Nil) {
			return errors.New("idempotency recovery index is missing")
		}
		if getErr != nil {
			return getErr
		}
		if !strings.HasPrefix(recordKey, "cv:v1:idempotency:") {
			return errors.New("idempotency recovery index is invalid")
		}
		checkpoint, _, requestErr := ledger.Request(ctx, requestID)
		status := "failed"
		if requestErr == nil {
			switch checkpoint.Status {
			case billing.RequestSettled:
				status = "settled"
			case billing.RequestCancelled:
				status = "cancelled"
			case billing.RequestRefunded:
				status = "refunded"
			default:
				return errors.New("idempotency recovery found a nonterminal ledger request")
			}
		} else if !errors.Is(requestErr, billing.ErrRequestNotFound) && !errors.Is(requestErr, billing.ErrInvalidOperation) {
			return requestErr
		}
		if err := s.completeIdempotencyRecord(ctx, recordKey, requestID, status); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Register(ctx context.Context, username, password, notice, remote string) (Account, []string, error) {
	name, err := domain.NormalizeUsername(username)
	if err != nil || !validPassword(password) || notice != s.cfg.PrivacyNoticeVersion {
		return Account{}, nil, ErrInvalid
	}
	if err := s.throttle(ctx, name.String(), remote); err != nil {
		return Account{}, nil, err
	}
	if err := s.takeArgonSlot(ctx); err != nil {
		return Account{}, nil, err
	}
	passwordHash, err := hashPassword(password)
	<-s.argonSlots
	if err != nil {
		return Account{}, nil, err
	}
	id, err := domain.NewID()
	if err != nil {
		return Account{}, nil, err
	}
	account := accountRecord{ID: id.String(), Username: name.String(), PasswordHash: passwordHash,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), PrivacyNoticeVersion: notice}
	recovery := make([]string, 8)
	verifiers := make([]any, 0, len(recovery))
	for i := range recovery {
		var code [16]byte
		if _, err := rand.Read(code[:]); err != nil {
			return Account{}, nil, err
		}
		recovery[i] = "crv_" + hex.EncodeToString(code[:])
		verifiers = append(verifiers, s.secretVerifier(recovery[i]))
	}
	encoded, err := json.Marshal(account)
	if err != nil {
		return Account{}, nil, err
	}
	keys := []string{accountPrefix + account.ID, usernamePrefix + name.String(), "cv:v1:recovery:" + account.ID}
	args := []any{string(encoded), account.ID}
	for _, verifier := range verifiers {
		args = append(args, verifier)
	}
	result, err := s.redis.Eval(ctx, `
local account_type=redis.call('TYPE',KEYS[1]).ok
local username_type=redis.call('TYPE',KEYS[2]).ok
local recovery_type=redis.call('TYPE',KEYS[3]).ok
if (account_type~='none' and account_type~='string') or (username_type~='none' and username_type~='string') or (recovery_type~='none' and recovery_type~='set') then return redis.error_reply('account key type mismatch') end
if account_type~='none' or username_type~='none' or recovery_type~='none' then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[2])
for i=3,#ARGV do redis.call('SADD', KEYS[3], ARGV[i]) end
return 1`, keys, args...).Int()
	if err != nil {
		return Account{}, nil, err
	}
	if result != 1 {
		return Account{}, nil, ErrConflict
	}
	return publicAccount(account), recovery, nil
}

func (s *Store) Login(ctx context.Context, username, password, remote string) (accountRecord, error) {
	name, err := domain.NormalizeUsername(username)
	if err != nil {
		if throttleErr := s.throttle(ctx, strings.ToLower(strings.TrimSpace(username)), remote); throttleErr != nil {
			return accountRecord{}, throttleErr
		}
		if err := s.takeArgonSlot(ctx); err != nil {
			return accountRecord{}, err
		}
		consumePasswordTiming(password)
		<-s.argonSlots
		return accountRecord{}, ErrNotFound
	}
	if err := s.throttle(ctx, name.String(), remote); err != nil {
		return accountRecord{}, err
	}
	id, err := s.redis.Get(ctx, usernamePrefix+name.String()).Result()
	if err != nil {
		if slotErr := s.takeArgonSlot(ctx); slotErr != nil {
			return accountRecord{}, slotErr
		}
		consumePasswordTiming(password)
		<-s.argonSlots
		return accountRecord{}, ErrNotFound
	}
	data, err := s.redis.Get(ctx, accountPrefix+id).Bytes()
	if err != nil {
		if slotErr := s.takeArgonSlot(ctx); slotErr != nil {
			return accountRecord{}, slotErr
		}
		consumePasswordTiming(password)
		<-s.argonSlots
		return accountRecord{}, ErrNotFound
	}
	var account accountRecord
	if json.Unmarshal(data, &account) != nil {
		return accountRecord{}, errors.New("account record is invalid")
	}
	if err := s.takeArgonSlot(ctx); err != nil {
		return accountRecord{}, err
	}
	ok := verifyPassword(account.PasswordHash, password)
	<-s.argonSlots
	if !ok {
		return accountRecord{}, ErrNotFound
	}
	return account, nil
}

func (s *Store) Recover(ctx context.Context, username, recoveryCode, password, remote string) (accountRecord, error) {
	name, err := domain.NormalizeUsername(username)
	if err != nil || !strings.HasPrefix(recoveryCode, "crv_") || !validPassword(password) {
		return accountRecord{}, ErrNotFound
	}
	if err := s.throttle(ctx, name.String(), remote); err != nil {
		return accountRecord{}, err
	}
	id, err := s.redis.Get(ctx, usernamePrefix+name.String()).Result()
	if err != nil {
		return accountRecord{}, ErrNotFound
	}
	if err := s.takeArgonSlot(ctx); err != nil {
		return accountRecord{}, err
	}
	passwordHash, err := hashPassword(password)
	<-s.argonSlots
	if err != nil {
		return accountRecord{}, err
	}
	data, err := s.redis.Get(ctx, accountPrefix+id).Bytes()
	if err != nil {
		return accountRecord{}, ErrNotFound
	}
	var account accountRecord
	if json.Unmarshal(data, &account) != nil {
		return accountRecord{}, errors.New("account record is invalid")
	}
	account.PasswordHash = passwordHash
	account.TOTPEnabled = false
	encoded, err := json.Marshal(account)
	if err != nil {
		return accountRecord{}, err
	}
	codeHash := s.secretVerifier(recoveryCode)
	// Consume the code, change the verifier, lock payouts and revoke every session atomically.
	updated, err := s.redis.Eval(ctx, `
local account_type=redis.call('TYPE',KEYS[1]).ok
local recovery_type=redis.call('TYPE',KEYS[2]).ok
local lock_type=redis.call('TYPE',KEYS[3]).ok
local sessions_type=redis.call('TYPE',KEYS[4]).ok
if account_type~='string' or recovery_type~='set' or (lock_type~='none' and lock_type~='string') or (sessions_type~='none' and sessions_type~='zset') then return redis.error_reply('recovery key type mismatch') end
if redis.call('SREM',KEYS[2],ARGV[1]) ~= 1 then return 0 end
redis.call('SET',KEYS[1],ARGV[2])
redis.call('SET',KEYS[3],ARGV[3])
local sessions=redis.call('ZRANGE',KEYS[4],0,-1)
for _,key in ipairs(sessions) do redis.call('DEL',key) end
redis.call('DEL',KEYS[4])
return 1`, []string{accountPrefix + id, "cv:v1:recovery:" + id, "cv:v1:recovery-lock:" + id, "cv:v1:sessions:" + id}, codeHash, string(encoded), strconv.FormatInt(time.Now().UTC().Add(24*time.Hour).Unix(), 10)).Int()
	if err != nil {
		return accountRecord{}, err
	}
	if updated != 1 {
		return accountRecord{}, ErrNotFound
	}
	return account, nil
}

func (s *Store) CreateSession(ctx context.Context, accountID string) (string, string, time.Time, error) {
	if _, err := domain.ParseID(accountID); err != nil {
		return "", "", time.Time{}, ErrInvalid
	}
	sessionID, err := randomToken(32)
	if err != nil {
		return "", "", time.Time{}, err
	}
	cookieValue := "cv1_" + sessionID
	csrf := s.csrfForCookie(cookieValue)
	now := time.Now().UTC()
	expires := now.Add(7 * 24 * time.Hour)
	record := sessionRecord{AccountID: accountID, CSRFHash: s.secretVerifier(csrf), CreatedAt: now.Unix(), ExpiresAt: expires.Unix(), IdleUntil: now.Add(12 * time.Hour).Unix()}
	data, _ := json.Marshal(record)
	key := s.sessionKey(sessionID)
	index := "cv:v1:sessions:" + accountID
	created, err := s.redis.Eval(ctx, `
local session_type=redis.call('TYPE',KEYS[1]).ok
local index_type=redis.call('TYPE',KEYS[2]).ok
if (session_type~='none' and session_type~='string') or (index_type~='none' and index_type~='zset') then return redis.error_reply('session key type mismatch') end
if session_type~='none' then return redis.error_reply('session ID collision') end
local members=redis.call('ZRANGE',KEYS[2],0,-1)
for _,member in ipairs(members) do
  if redis.call('EXISTS',member)==0 then redis.call('ZREM',KEYS[2],member) end
end
if redis.call('ZCARD',KEYS[2])>=10 then return 0 end
redis.call('SET',KEYS[1],ARGV[1],'EX',ARGV[2],'NX')
redis.call('ZADD',KEYS[2],ARGV[3],KEYS[1])
return 1`, []string{key, index}, string(data), 12*60*60, expires.Unix()).Int()
	if err != nil {
		return "", "", time.Time{}, err
	}
	if created != 1 {
		return "", "", time.Time{}, ErrThrottled
	}
	return cookieValue, csrf, expires, nil
}

func (s *Store) Session(ctx context.Context, cookie string) (sessionRecord, Account, bool) {
	if !strings.HasPrefix(cookie, "cv1_") || len(cookie) != 47 {
		return sessionRecord{}, Account{}, false
	}
	key := s.sessionKey(strings.TrimPrefix(cookie, "cv1_"))
	data, err := s.redis.Get(ctx, key).Bytes()
	if err != nil {
		return sessionRecord{}, Account{}, false
	}
	var session sessionRecord
	if json.Unmarshal(data, &session) != nil {
		return sessionRecord{}, Account{}, false
	}
	now := time.Now().Unix()
	if now >= session.ExpiresAt || now >= session.IdleUntil {
		_ = s.redis.Del(ctx, key).Err()
		if session.AccountID != "" {
			_ = s.redis.ZRem(ctx, "cv:v1:sessions:"+session.AccountID, key).Err()
		}
		return sessionRecord{}, Account{}, false
	}
	accountData, err := s.redis.Get(ctx, accountPrefix+session.AccountID).Bytes()
	if err != nil {
		_ = s.redis.Del(ctx, key).Err()
		_ = s.redis.ZRem(ctx, "cv:v1:sessions:"+session.AccountID, key).Err()
		return sessionRecord{}, Account{}, false
	}
	var record accountRecord
	if json.Unmarshal(accountData, &record) != nil {
		return sessionRecord{}, Account{}, false
	}
	idle := time.Now().Add(12 * time.Hour).Unix()
	if idle > session.ExpiresAt {
		idle = session.ExpiresAt
	}
	session.IdleUntil = idle
	encoded, _ := json.Marshal(session)
	if s.redis.Set(ctx, key, encoded, time.Until(time.Unix(idle, 0))).Err() != nil {
		return sessionRecord{}, Account{}, false
	}
	return session, publicAccount(record), true
}

func (s *Store) ValidateCSRF(session sessionRecord, token string) bool {
	if token == "" {
		return false
	}
	provided := []byte(s.secretVerifier(token))
	want := []byte(session.CSRFHash)
	return hmac.Equal(provided, want)
}

func (s *Store) csrfForCookie(cookie string) string { return s.secretVerifier("csrf:" + cookie) }

func (s *Store) SessionActive(ctx context.Context, cookie string) bool {
	if !strings.HasPrefix(cookie, "cv1_") || len(cookie) != 47 {
		return false
	}
	data, err := s.redis.Get(ctx, s.sessionKey(strings.TrimPrefix(cookie, "cv1_"))).Bytes()
	if err != nil {
		return false
	}
	var session sessionRecord
	if json.Unmarshal(data, &session) != nil {
		return false
	}
	now := time.Now().Unix()
	return now < session.ExpiresAt && now < session.IdleUntil
}

type liveTicket struct {
	AccountID string `json:"account_id"`
	SessionID string `json:"session_id"`
}

func (s *Store) CreateLiveTicket(ctx context.Context, accountID, cookie string) (string, error) {
	if _, err := domain.ParseID(accountID); err != nil || !strings.HasPrefix(cookie, "cv1_") || len(cookie) != 47 {
		return "", ErrInvalid
	}
	ticket, err := randomToken(32)
	if err != nil {
		return "", err
	}
	sessionID := strings.TrimPrefix(cookie, "cv1_")
	session := liveTicket{AccountID: accountID, SessionID: s.sessionKey(sessionID)}
	encoded, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(ticket))
	key := "cv:v1:live-ticket:" + hex.EncodeToString(digest[:])
	if err := s.redis.SetNX(ctx, key, encoded, 30*time.Second).Err(); err != nil {
		return "", err
	}
	return ticket, nil
}

func (s *Store) ConsumeLiveTicket(ctx context.Context, accountID, cookie, ticket string) bool {
	if _, err := domain.ParseID(accountID); err != nil || len(ticket) < 32 || len(ticket) > 128 || !strings.HasPrefix(cookie, "cv1_") || len(cookie) != 47 {
		return false
	}
	digest := sha256.Sum256([]byte(ticket))
	key := "cv:v1:live-ticket:" + hex.EncodeToString(digest[:])
	data, err := s.redis.GetDel(ctx, key).Bytes()
	if err != nil {
		return false
	}
	var stored liveTicket
	if json.Unmarshal(data, &stored) != nil {
		return false
	}
	return hmac.Equal([]byte(stored.AccountID), []byte(accountID)) && hmac.Equal([]byte(stored.SessionID), []byte(s.sessionKey(strings.TrimPrefix(cookie, "cv1_"))))
}

type LiveEvent struct {
	EventID    string `json:"event_id"`
	Topic      string `json:"topic"`
	ResourceID string `json:"resource_id"`
	Revision   int64  `json:"revision"`
	Action     string `json:"action"`
}

func (s *Store) LiveEvents(ctx context.Context, accountID, afterID string) ([]LiveEvent, string, error) {
	stream := "cv:v1:live:" + accountID
	if afterID == "" {
		entries, err := s.redis.XRevRangeN(ctx, stream, "+", "-", 1).Result()
		if err != nil && err != redis.Nil {
			return nil, "", err
		}
		if len(entries) > 0 {
			afterID = entries[0].ID
		} else {
			afterID = "0-0"
		}
		return nil, afterID, nil
	}
	if !validStreamID(afterID) {
		return nil, "gap", nil
	}
	entries, err := s.redis.XRangeN(ctx, stream, "-", "+", 1).Result()
	if err != nil && err != redis.Nil {
		return nil, "", err
	}
	if len(entries) == 0 && afterID != "0-0" {
		return nil, "cursor_expired", nil
	}
	if len(entries) > 0 && afterID != "0-0" {
		cursor, err := s.redis.XRangeN(ctx, stream, afterID, afterID, 1).Result()
		if err != nil && err != redis.Nil {
			return nil, "", err
		}
		if len(cursor) == 0 {
			if compareStreamID(afterID, entries[0].ID) < 0 {
				return nil, "cursor_expired", nil
			}
			return nil, "gap", nil
		}
	}
	rows, err := s.redis.XRead(ctx, &redis.XReadArgs{Streams: []string{stream, afterID}, Count: 32, Block: time.Second}).Result()
	if err == redis.Nil {
		return nil, afterID, nil
	}
	if err != nil {
		return nil, "", err
	}
	events := make([]LiveEvent, 0, 32)
	for _, row := range rows {
		for _, entry := range row.Messages {
			resource, _ := entry.Values["resource_id"].(string)
			revision, _ := strconv.ParseInt(fmt.Sprint(entry.Values["revision"]), 10, 64)
			topic, _ := entry.Values["topic"].(string)
			events = append(events, LiveEvent{EventID: entry.ID, Topic: topic, ResourceID: resource, Revision: revision, Action: "invalidate"})
			afterID = entry.ID
		}
	}
	return events, afterID, nil
}

func validStreamID(value string) bool {
	if len(value) == 0 || len(value) > 40 || strings.Count(value, "-") != 1 {
		return false
	}
	parts := strings.SplitN(value, "-", 2)
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func compareStreamID(a, b string) int {
	partsA := strings.SplitN(a, "-", 2)
	partsB := strings.SplitN(b, "-", 2)
	for i := 0; i < 2; i++ {
		av, _ := strconv.ParseUint(partsA[i], 10, 64)
		bv, _ := strconv.ParseUint(partsB[i], 10, 64)
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func (s *Store) RevokeSession(ctx context.Context, cookie string) error {
	if !strings.HasPrefix(cookie, "cv1_") || len(cookie) != 47 {
		return nil
	}
	key := s.sessionKey(strings.TrimPrefix(cookie, "cv1_"))
	data, err := s.redis.Get(ctx, key).Bytes()
	if err != nil && err != redis.Nil {
		return err
	}
	if len(data) > 0 {
		var session sessionRecord
		if json.Unmarshal(data, &session) == nil && session.AccountID != "" {
			if err := s.redis.ZRem(ctx, "cv:v1:sessions:"+session.AccountID, key).Err(); err != nil {
				return err
			}
		}
	}
	return s.redis.Del(ctx, key).Err()
}

func (s *Store) CreateAPIKey(ctx context.Context, accountID, name string, scopes, models []string, dailyCap string) (keyRecord, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || len([]rune(name)) > 64 || containsControl(name) || len(scopes) == 0 || len(models) == 0 || len(models) > 8 {
		return keyRecord{}, "", ErrInvalid
	}
	if _, err := domain.ParsePiconero(dailyCap); err != nil {
		return keyRecord{}, "", ErrInvalid
	}
	allowedScopes := map[string]bool{"chat:write": true, "models:read": true, "usage:read": true}
	scopeSet := map[string]bool{}
	for _, scope := range scopes {
		if !allowedScopes[scope] || scopeSet[scope] {
			return keyRecord{}, "", ErrInvalid
		}
		scopeSet[scope] = true
	}
	modelSet := map[string]bool{}
	for _, model := range models {
		if model != api.ModelID || modelSet[model] {
			return keyRecord{}, "", ErrInvalid
		}
		modelSet[model] = true
	}
	id, err := domain.NewID()
	if err != nil {
		return keyRecord{}, "", err
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return keyRecord{}, "", err
	}
	secret := apiKeyPrefix + hex.EncodeToString(secretBytes)
	verifier := s.secretVerifier(secret)
	created := time.Now().UTC().Format(time.RFC3339Nano)
	sort.Strings(scopes)
	sort.Strings(models)
	key := keyRecord{ID: id.String(), AccountID: accountID, Name: name, Prefix: secret[:12], Permissions: scopes, ModelIDs: models, DailyCap: dailyCap, CreatedAt: created, Verifier: verifier}
	encoded, err := json.Marshal(key)
	if err != nil {
		return keyRecord{}, "", err
	}
	script := `
local key_type=redis.call('TYPE',KEYS[1]).ok
local index_type=redis.call('TYPE',KEYS[2]).ok
local account_type=redis.call('TYPE',KEYS[3]).ok
if (key_type~='none' and key_type~='string') or (index_type~='none' and index_type~='string') or (account_type~='none' and account_type~='set') then return redis.error_reply('API key storage type mismatch') end
if redis.call('SCARD',KEYS[3])>=20 then return 0 end
if redis.call('EXISTS', KEYS[1]) == 1 or redis.call('EXISTS', KEYS[2]) == 1 then return redis.error_reply('API key ID collision') end
redis.call('SET', KEYS[1], ARGV[1]); redis.call('SET', KEYS[2], ARGV[2]); redis.call('SADD', KEYS[3], ARGV[3]); return 1`
	createdCount, err := s.redis.Eval(ctx, script, []string{keyPrefix + key.ID, keyIndexPrefix + verifier, "cv:v1:account-keys:" + accountID}, string(encoded), key.ID, key.ID).Int()
	if err != nil {
		return keyRecord{}, "", err
	}
	if createdCount != 1 {
		return keyRecord{}, "", ErrThrottled
	}
	return key, secret, nil
}

func (s *Store) Authenticate(r *http.Request) (api.Principal, bool) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasPrefix(token, apiKeyPrefix) {
			key, ok := s.APIKey(r.Context(), token)
			if !ok {
				return api.Principal{}, false
			}
			scopes := map[string]bool{}
			for _, scope := range key.Permissions {
				scopes[scope] = true
			}
			models := make(map[string]bool, len(key.ModelIDs))
			for _, model := range key.ModelIDs {
				models[model] = true
			}
			return api.Principal{AccountID: key.AccountID, Kind: "key", Scopes: scopes, KeyID: key.ID,
				AllowedModels: models, DailySpendCapPiconero: key.DailyCap}, true
		}
		return api.Principal{}, false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return api.Principal{}, false
	}
	_, account, ok := s.Session(r.Context(), cookie.Value)
	if !ok {
		return api.Principal{}, false
	}
	return api.Principal{AccountID: account.ID, Kind: "session", Scopes: map[string]bool{"models:read": true}}, true
}

func (s *Store) APIKey(ctx context.Context, token string) (keyRecord, bool) {
	if len(token) != len(apiKeyPrefix)+64 {
		return keyRecord{}, false
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(token, apiKeyPrefix)); err != nil {
		return keyRecord{}, false
	}
	verifier := s.secretVerifier(token)
	id, err := s.redis.Get(ctx, keyIndexPrefix+verifier).Result()
	if err != nil {
		return keyRecord{}, false
	}
	data, err := s.redis.Get(ctx, keyPrefix+id).Bytes()
	if err != nil {
		return keyRecord{}, false
	}
	var key keyRecord
	if json.Unmarshal(data, &key) != nil || key.ID != id || !hmac.Equal([]byte(key.Verifier), []byte(verifier)) || key.RevokedAt != nil {
		return keyRecord{}, false
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	key.LastUsedAt = &now
	updated, err := json.Marshal(key)
	if err != nil {
		return keyRecord{}, false
	}
	changed, err := s.redis.Eval(ctx, `
local key_type=redis.call('TYPE',KEYS[1]).ok
if key_type~='string' then return 0 end
local decoded,key=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not decoded or type(key)~='table' or key['id']~=ARGV[1] or key['account_id']~=ARGV[2] or key['verifier']~=ARGV[3] or key['revoked_at']~=cjson.null then return 0 end
redis.call('SET',KEYS[1],ARGV[4])
return 1`, []string{keyPrefix + id}, id, key.AccountID, verifier, string(updated)).Int()
	if err != nil || changed != 1 {
		return keyRecord{}, false
	}
	return key, true
}

func (s *Store) ListKeys(ctx context.Context, accountID string) ([]keyRecord, error) {
	ids, err := s.redis.SMembers(ctx, "cv:v1:account-keys:"+accountID).Result()
	if err != nil {
		return nil, err
	}
	if len(ids) > 20 {
		return nil, errors.New("key inventory limit exceeded")
	}
	keys := make([]keyRecord, 0, len(ids))
	for _, id := range ids {
		data, err := s.redis.Get(ctx, keyPrefix+id).Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		var key keyRecord
		if json.Unmarshal(data, &key) != nil || key.AccountID != accountID {
			continue
		}
		key.Verifier = ""
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].CreatedAt > keys[j].CreatedAt })
	return keys, nil
}

func (s *Store) RevokeKey(ctx context.Context, accountID, keyID string) error {
	if _, err := domain.ParseID(keyID); err != nil {
		return ErrNotFound
	}
	data, err := s.redis.Get(ctx, keyPrefix+keyID).Bytes()
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var key keyRecord
	if json.Unmarshal(data, &key) != nil || key.ID != keyID || key.AccountID != accountID || key.Verifier == "" {
		return ErrNotFound
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	updated, err := s.redis.Eval(ctx, `
local keys_type=redis.call('TYPE',KEYS[1]).ok
local key_type=redis.call('TYPE',KEYS[2]).ok
local index_type=redis.call('TYPE',KEYS[3]).ok
if keys_type~='set' or key_type~='string' or (index_type~='none' and index_type~='string') then return redis.error_reply('API key storage type mismatch') end
if redis.call('SISMEMBER',KEYS[1],ARGV[1])~=1 then return 0 end
local decoded,key=pcall(cjson.decode,redis.call('GET',KEYS[2]))
if not decoded or type(key)~='table' or key['id']~=ARGV[1] or key['account_id']~=ARGV[2] or key['verifier']~=ARGV[3] then return 0 end
if key['revoked_at']==cjson.null then key['revoked_at']=ARGV[4]; redis.call('SET',KEYS[2],cjson.encode(key)) end
if type(key['verifier'])=='string' and key['verifier']~='' then redis.call('DEL',KEYS[3]) end
return 1`, []string{"cv:v1:account-keys:" + accountID, keyPrefix + keyID, keyIndexPrefix + key.Verifier}, keyID, accountID, key.Verifier, now).Int()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddUsage(ctx context.Context, accountID, keyID, requestID, modelID string, usage inference.Usage, latency time.Duration) error {
	return s.addUsage(ctx, accountID, keyID, requestID, modelID, "completed", "0", usage, latency)
}

func (s *Store) AddAccountedUsage(ctx context.Context, accountID, keyID, requestID, modelID, status, charged string,
	usage inference.Usage, latency time.Duration) error {
	if status != "settled" && status != "cancelled" && status != "refunded" {
		return ErrInvalid
	}
	amount, err := domain.ParsePiconero(charged)
	if err != nil || amount.String() != charged {
		return ErrInvalid
	}
	return s.addUsage(ctx, accountID, keyID, requestID, modelID, status, charged, usage, latency)
}

func (s *Store) addUsage(ctx context.Context, accountID, keyID, requestID, modelID, status, charged string,
	usage inference.Usage, latency time.Duration) error {
	if _, err := domain.ParseID(accountID); err != nil {
		return err
	}
	if _, err := domain.ParseID(keyID); err != nil {
		return err
	}
	if _, err := domain.ParseID(requestID); err != nil {
		return err
	}
	if modelID != api.ModelID || latency < 0 || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.PromptTokens > int(^uint(0)>>1)-usage.CompletionTokens || usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		return ErrInvalid
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	eventID, err := domain.NewID()
	if err != nil {
		return err
	}
	entry := usageEvent{V: 1, EventID: eventID.String(), EventType: "request.updated", OccurredAt: now, ObservedAt: now, RequestID: &requestID, Payload: usageProjection{AccountID: accountID, APIKeyID: keyID, ModelID: modelID, Status: status, InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, Charged: charged, LatencyMS: latency.Milliseconds(), Revision: 1, CreatedAt: now, FinishedAt: now}}
	return s.writeUsageEvent(ctx, requestID, keyID, accountID, modelID, entry)
}

func (s *Store) writeUsageEvent(ctx context.Context, requestID, keyID, accountID, modelID string, entry usageEvent) error {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	createdCount, err := s.redis.Eval(ctx, `
local key_type=redis.call('TYPE',KEYS[1]).ok
local sequence_type=redis.call('TYPE',KEYS[2]).ok
local outbox_type=redis.call('TYPE',KEYS[3]).ok
local pending_type=redis.call('TYPE',KEYS[4]).ok
if key_type~='string' then return 0 end
if (sequence_type~='none' and sequence_type~='string') or (outbox_type~='none' and outbox_type~='stream') or (pending_type~='none' and pending_type~='set') then return redis.error_reply('usage outbox key type mismatch') end
local key_data=redis.call('GET',KEYS[1])
local decoded,key=pcall(cjson.decode,key_data)
if not decoded or type(key)~='table' or key['id']~=ARGV[1] or key['account_id']~=ARGV[2] then return 0 end
local can_chat=false
for _,scope in ipairs(key['permissions'] or {}) do if scope=='chat:write' then can_chat=true end end
local can_use_model=false
for _,model in ipairs(key['model_ids'] or {}) do if model==ARGV[3] then can_use_model=true end end
if not can_chat or not can_use_model then return 0 end
local seq=redis.call('INCR',KEYS[2])
local event=cjson.decode(ARGV[4]); event['sequence']=seq
local body=cjson.encode(event)
redis.call('XADD',KEYS[3],'*','event_id',event['event_id'],'account_id',event['payload']['account_id'],'payload',body)
redis.call('SREM',KEYS[4],ARGV[5])
return 1`, []string{keyPrefix + keyID, "cv:v1:outbox-sequence:usage", "cv:v1:outbox:usage", s.usagePendingKey}, keyID, accountID, modelID, string(encoded), requestID).Int()
	if err != nil {
		return err
	}
	if createdCount != 1 {
		return ErrNotFound
	}
	return nil
}

type usageRecoveryLedger interface {
	PendingUsageRequestIDs(context.Context) ([]string, error)
	Request(context.Context, string) (billing.RequestCheckpoint, uint64, error)
	Operation(context.Context, string) (billing.Entry, error)
}

// ReconcileAccountedUsage republishes terminal ledger metadata that was left
// pending by a crash between the financial append and the account outbox write.
func (s *Store) ReconcileAccountedUsage(ctx context.Context, ledger usageRecoveryLedger) error {
	if ledger == nil {
		return errors.New("usage recovery requires an accounting ledger")
	}
	requestIDs, err := ledger.PendingUsageRequestIDs(ctx)
	if err != nil {
		return err
	}
	for _, requestID := range requestIDs {
		checkpoint, _, err := ledger.Request(ctx, requestID)
		if err != nil {
			return err
		}
		if checkpoint.Status != billing.RequestSettled && checkpoint.Status != billing.RequestCancelled && checkpoint.Status != billing.RequestRefunded {
			return errors.New("usage recovery found a nonterminal ledger request")
		}
		if checkpoint.InputTokens < 0 || checkpoint.InputTokens > int64(^uint(0)>>1) {
			return errors.New("usage recovery found invalid input token metadata")
		}
		outputTokens := int64(0)
		foundOutputAttempt := false
		for _, attempt := range checkpoint.Attempts {
			if attempt.Reservation.Status == billing.AttemptSettled || attempt.Reservation.Status == billing.AttemptCancelled {
				if foundOutputAttempt {
					return errors.New("usage recovery found multiple terminal output attempts")
				}
				foundOutputAttempt = true
				outputTokens = attempt.OutputTokens
			}
		}
		if (checkpoint.Status == billing.RequestSettled || checkpoint.Status == billing.RequestCancelled) && !foundOutputAttempt {
			return errors.New("usage recovery found no terminal output attempt")
		}
		if outputTokens < 0 || outputTokens > int64(^uint(0)>>1) {
			return errors.New("usage recovery found invalid output token metadata")
		}
		charged, err := domain.NewPiconero(0)
		if err != nil {
			return err
		}
		if checkpoint.Status == billing.RequestSettled || checkpoint.Status == billing.RequestCancelled {
			quote, quoteErr := billing.CalculateQuote(checkpoint.InputTokens, outputTokens, checkpoint.MaxOutputTokens,
				checkpoint.Price, checkpoint.Status == billing.RequestCancelled)
			if quoteErr != nil {
				return quoteErr
			}
			charged = quote.ActualCost
		}
		finishedAt, err := usageTerminalTime(ctx, ledger, requestID, checkpoint)
		if err != nil {
			return err
		}
		latency := finishedAt.Sub(checkpoint.AcceptedAt)
		if latency < 0 {
			latency = 0
		}
		eventID, err := domain.NewID()
		if err != nil {
			return err
		}
		observedAt := time.Now().UTC()
		requestIDCopy := requestID
		status := string(checkpoint.Status)
		entry := usageEvent{V: 1, EventID: eventID.String(), EventType: "request.updated", OccurredAt: finishedAt.UTC().Format(time.RFC3339Nano),
			ObservedAt: observedAt.Format(time.RFC3339Nano), RequestID: &requestIDCopy,
			Payload: usageProjection{AccountID: checkpoint.AccountID, APIKeyID: checkpoint.APIKeyID, ModelID: checkpoint.ModelID,
				Status: status, InputTokens: int(checkpoint.InputTokens), OutputTokens: int(outputTokens), Charged: charged.String(),
				LatencyMS: latency.Milliseconds(), Revision: 1, CreatedAt: checkpoint.AcceptedAt.UTC().Format(time.RFC3339Nano),
				FinishedAt: finishedAt.UTC().Format(time.RFC3339Nano)}}
		if err := s.writeUsageEvent(ctx, requestID, checkpoint.APIKeyID, checkpoint.AccountID, checkpoint.ModelID, entry); err != nil {
			return err
		}
	}
	return nil
}

func usageTerminalTime(ctx context.Context, ledger usageRecoveryLedger, requestID string,
	checkpoint billing.RequestCheckpoint) (time.Time, error) {
	operationID := ""
	switch checkpoint.Status {
	case billing.RequestSettled:
		operationID = "settle:" + requestID
	case billing.RequestCancelled:
		operationID = "cancel:" + requestID
	case billing.RequestRefunded:
		operationID = "abandon:" + requestID
	}
	if operationID != "" {
		entry, err := ledger.Operation(ctx, operationID)
		if err == nil {
			return entry.CreatedAt.UTC(), nil
		}
		if !errors.Is(err, billing.ErrRequestNotFound) {
			return time.Time{}, err
		}
	}
	var latest time.Time
	for attemptID, attempt := range checkpoint.Attempts {
		if attempt.Reservation.Status != billing.AttemptRefunded {
			continue
		}
		entry, err := ledger.Operation(ctx, "refund:"+requestID+":"+attemptID)
		if errors.Is(err, billing.ErrRequestNotFound) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if entry.CreatedAt.After(latest) {
			latest = entry.CreatedAt
		}
	}
	if latest.IsZero() {
		return time.Time{}, errors.New("usage recovery could not locate the terminal ledger operation")
	}
	return latest.UTC(), nil
}

type UsageRow struct {
	RequestID    string  `json:"request_id"`
	APIKeyID     string  `json:"api_key_id"`
	ModelID      string  `json:"model_id"`
	Status       string  `json:"status"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Charged      string  `json:"charged_piconero"`
	LatencyMS    int64   `json:"latency_ms"`
	CreatedAt    string  `json:"created_at"`
	FinishedAt   *string `json:"finished_at"`
}

func (s *Store) Usage(ctx context.Context, accountID, keyID string) ([]UsageRow, error) {
	stream := "cv:v1:account-usage:" + accountID
	if keyID != "" {
		stream = "cv:v1:key-usage:" + keyID
	}
	entries, err := s.redis.XRevRangeN(ctx, stream, "+", "-", 50).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	rows := make([]UsageRow, 0, len(entries))
	for _, entry := range entries {
		raw, ok := entry.Values["payload"].(string)
		if !ok {
			continue
		}
		var projection usageProjection
		if json.Unmarshal([]byte(raw), &projection) != nil || projection.AccountID != accountID || (keyID != "" && projection.APIKeyID != keyID) {
			continue
		}
		requestID, ok := entry.Values["request_id"].(string)
		if !ok {
			continue
		}
		finished := projection.FinishedAt
		rows = append(rows, UsageRow{RequestID: requestID, APIKeyID: projection.APIKeyID, ModelID: projection.ModelID, Status: projection.Status, InputTokens: projection.InputTokens, OutputTokens: projection.OutputTokens, Charged: projection.Charged, LatencyMS: projection.LatencyMS, CreatedAt: projection.CreatedAt, FinishedAt: &finished})
	}
	return rows, nil
}

func projectUsage(ctx context.Context, client *redis.Client, event usageEvent) error {
	amount, amountErr := domain.ParsePiconero(event.Payload.Charged)
	validStatus := event.Payload.Status == "completed" || event.Payload.Status == "settled" ||
		event.Payload.Status == "cancelled" || event.Payload.Status == "refunded"
	if event.V != 1 || event.EventType != "request.updated" || event.RequestID == nil || event.Sequence < 1 ||
		event.Payload.AccountID == "" || event.Payload.ModelID != api.ModelID || !validStatus || amountErr != nil || amount.String() != event.Payload.Charged ||
		event.Payload.InputTokens < 0 || event.Payload.OutputTokens < 0 ||
		event.Payload.LatencyMS < 0 || event.Payload.Revision < 1 {
		return ErrInvalid
	}
	if _, err := domain.ParseID(event.Payload.AccountID); err != nil {
		return ErrInvalid
	}
	if _, err := domain.ParseID(event.Payload.APIKeyID); err != nil {
		return ErrInvalid
	}
	if _, err := domain.ParseID(*event.RequestID); err != nil {
		return ErrInvalid
	}
	if _, err := domain.ParseID(event.EventID); err != nil {
		return ErrInvalid
	}
	if _, err := time.Parse(time.RFC3339Nano, event.OccurredAt); err != nil {
		return ErrInvalid
	}
	if _, err := time.Parse(time.RFC3339Nano, event.ObservedAt); err != nil {
		return ErrInvalid
	}
	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	eventBytes, err := json.Marshal(event)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(eventBytes)
	marker := "cv:v1:usage-event:" + event.EventID
	stream := "cv:v1:account-usage:" + event.Payload.AccountID
	streamID := strconv.FormatInt(event.Sequence, 10) + "-0"
	_, err = client.Eval(ctx, `
local marker_type=redis.call('TYPE',KEYS[1]).ok
local stream_type=redis.call('TYPE',KEYS[2]).ok
local live_type=redis.call('TYPE',KEYS[3]).ok
local key_stream_type=redis.call('TYPE',KEYS[4]).ok
if marker_type~='none' and marker_type~='string' then return redis.error_reply('usage marker key has wrong type') end
if stream_type~='none' and stream_type~='stream' then return redis.error_reply('usage projection key has wrong type') end
if live_type~='none' and live_type~='stream' then return redis.error_reply('live projection key has wrong type') end
if key_stream_type~='none' and key_stream_type~='stream' then return redis.error_reply('API key usage key has wrong type') end
local existing=redis.call('GET',KEYS[1])
if existing then
  if existing==ARGV[4] then return 0 end
  return redis.error_reply('usage event identity collision')
end
local prior=redis.call('XRANGE',KEYS[2],ARGV[1],ARGV[1],'COUNT',1)
if #prior>0 then
  local fields=prior[1][2]
  local event_id=''
  local payload=''
  for i=1,#fields,2 do if fields[i]=='event_id' then event_id=fields[i+1] elseif fields[i]=='payload' then payload=fields[i+1] end end
  if event_id~=ARGV[2] or payload~=ARGV[3] then return redis.error_reply('usage stream identity collision') end
  redis.call('SET',KEYS[1],ARGV[4],'EX',2592000)
  return 0
end
redis.call('XADD',KEYS[2],'MAXLEN','~',100,ARGV[1],'event_id',ARGV[2],'request_id',ARGV[5],'payload',ARGV[3])
redis.call('XADD',KEYS[4],'MAXLEN',100,ARGV[1],'event_id',ARGV[2],'request_id',ARGV[5],'payload',ARGV[3])
redis.call('XADD',KEYS[3],'MAXLEN',10000,'*','topic','usage','resource_id',ARGV[5],'revision',ARGV[6],'action','invalidate')
local now=redis.call('TIME')
local cutoff=tostring(tonumber(now[1])-86400)..'-0'
redis.call('XTRIM',KEYS[3],'MINID',cutoff)
redis.call('EXPIRE',KEYS[3],86400)
redis.call('SET',KEYS[1],ARGV[4],'EX',2592000)
return 1`, []string{marker, stream, "cv:v1:live:" + event.Payload.AccountID, "cv:v1:key-usage:" + event.Payload.APIKeyID}, streamID, event.EventID, string(encoded), hex.EncodeToString(digest[:]), *event.RequestID, strconv.FormatInt(event.Payload.Revision, 10)).Result()
	return err
}

func (s *Store) ProjectUsage(ctx context.Context, event usageEvent) error {
	return projectUsage(ctx, s.redis, event)
}

func (s *Store) sessionKey(id string) string {
	digest := sha256.Sum256([]byte(id))
	return "cv:v1:session:" + hex.EncodeToString(digest[:])
}
func (s *Store) secretVerifier(value string) string {
	mac := hmac.New(sha256.New, s.cfg.Pepper)
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) throttle(ctx context.Context, username, remote string) error {
	userHash := s.secretVerifier("u:" + username)
	sourceHash := s.secretVerifier("s:" + remote)
	allowed, err := s.redis.Eval(ctx, `
local user_type=redis.call('TYPE',KEYS[1]).ok
local source_type=redis.call('TYPE',KEYS[2]).ok
if (user_type~='none' and user_type~='string') or (source_type~='none' and source_type~='string') then return redis.error_reply('rate limit key type mismatch') end
local a=redis.call('INCR',KEYS[1]); if a==1 then redis.call('EXPIRE',KEYS[1],60) end
local b=redis.call('INCR',KEYS[2]); if b==1 then redis.call('EXPIRE',KEYS[2],60) end
if a>5 or b>5 then return 0 end return 1`, []string{"cv:v1:ratelimit:auth:user:" + userHash, "cv:v1:ratelimit:auth:source:" + sourceHash}).Int()
	if err != nil {
		return err
	}
	if allowed != 1 {
		return ErrThrottled
	}
	return nil
}

func (s *Store) takeArgonSlot(ctx context.Context) error {
	select {
	case s.argonSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrThrottled
	}
}

func publicAccount(record accountRecord) Account {
	return Account{ID: record.ID, Username: record.Username, CreatedAt: record.CreatedAt, TOTPEnabled: record.TOTPEnabled, PrivacyNoticeVersion: record.PrivacyNoticeVersion}
}
func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
func validPassword(value string) bool {
	n := len([]rune(value))
	return utf8.ValidString(value) && n >= 12 && n <= 128 && len(value) <= 512
}
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" || parts[2] != "m=65536,t=3,p=2" {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[3])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[4])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return hmac.Equal(got, want)
}
func consumePasswordTiming(password string) {
	var salt [16]byte
	_ = argon2.IDKey([]byte(password), salt[:], 3, 64*1024, 2, 32)
}
func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func remoteAddress(r *http.Request, trustedProxy bool) string {
	if trustedProxy {
		if value := strings.TrimSpace(r.Header.Get("X-Real-IP")); value != "" && net.ParseIP(value) != nil {
			return value
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return "unknown"
}
