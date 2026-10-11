package m1

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"cordova/server/internal/domain"

	"github.com/redis/go-redis/v9"
)

const (
	nodeEnrollmentTTL = 10 * time.Minute
	nodeNonceTTL      = 60 * time.Second
	nodeAccessTTL     = 5 * time.Minute
	nodeClockSkew     = 30 * time.Second
)

var (
	ErrNodeUnauthorized = errors.New("node authentication failed")
	ErrNodePending      = errors.New("node enrollment is pending owner approval")
	ErrNodeExpired      = errors.New("node enrollment expired")
	ErrNodeConflict     = errors.New("node enrollment is no longer pending")
	ErrNodeThrottled    = errors.New("node authentication is rate limited")
)

type nodeService struct {
	redis        *redis.Client
	origin       string
	pepper       []byte
	encryption   []byte
	signingKey   ed25519.PrivateKey
	verification ed25519.PublicKey
	keyID        string
	peerMu       sync.RWMutex
	peers        map[string]*nodePeer
}

type nodeRecord struct {
	ID            string        `json:"id"`
	OwnerID       string        `json:"owner_id"`
	Name          string        `json:"name"`
	HWIDHash      string        `json:"hwid_hash"`
	NodeVersion   string        `json:"node_version"`
	SecretCipher  string        `json:"secret_ciphertext"`
	SecretVersion int64         `json:"secret_version"`
	Revoked       bool          `json:"revoked"`
	CreatedAt     string        `json:"created_at"`
	ApprovedAt    string        `json:"approved_at"`
	Hardware      *nodeHardware `json:"hardware,omitempty"`
	LastSeenAt    string        `json:"last_seen_at,omitempty"`
}

type nodeIdentity struct {
	NodeID        string
	OwnerID       string
	SecretVersion int64
	SessionID     string
	Generation    uint64
}

type nodeJWTHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type nodeJWTClaims struct {
	Issuer        string `json:"iss"`
	Audience      string `json:"aud"`
	Subject       string `json:"sub"`
	SecretVersion int64  `json:"secret_version"`
	TokenID       string `json:"jti"`
	IssuedAt      int64  `json:"iat"`
	ExpiresAt     int64  `json:"exp"`
}

func newNodeService(client *redis.Client, cfg Config) (*nodeService, error) {
	if client == nil || len(cfg.Pepper) < 32 || len(cfg.NodeSecretEncryptionKey) != 32 ||
		len(cfg.NodeJWTSigningKey) != ed25519.PrivateKeySize || !validOrigin(cfg.Origin) {
		return nil, errors.New("node API requires Redis, a 32-byte encryption key and an Ed25519 signing key")
	}
	privateKey := ed25519.NewKeyFromSeed(cfg.NodeJWTSigningKey[:ed25519.SeedSize])
	if !bytes.Equal(privateKey, cfg.NodeJWTSigningKey) {
		return nil, errors.New("node JWT signing key is not a canonical Ed25519 private key")
	}
	publicKey := append(ed25519.PublicKey(nil), privateKey.Public().(ed25519.PublicKey)...)
	keyDigest := sha256.Sum256(publicKey)
	return &nodeService{redis: client, origin: strings.TrimRight(cfg.Origin, "/"), pepper: append([]byte(nil), cfg.Pepper...),
		encryption: append([]byte(nil), cfg.NodeSecretEncryptionKey...), signingKey: privateKey, verification: publicKey,
		keyID: hex.EncodeToString(keyDigest[:8]), peers: make(map[string]*nodePeer)}, nil
}

func (s *Store) ensureNodeService(cfg Config) (*nodeService, error) {
	if s == nil {
		return nil, errors.New("M1 store is required for the node service")
	}
	s.nodeMu.Lock()
	defer s.nodeMu.Unlock()
	if s.nodes == nil {
		nodes, err := newNodeService(s.redis, cfg)
		if err != nil {
			return nil, err
		}
		s.nodes = nodes
		return nodes, nil
	}
	if s.nodes.origin != strings.TrimRight(cfg.Origin, "/") || !hmac.Equal(s.nodes.pepper, cfg.Pepper) ||
		!hmac.Equal(s.nodes.encryption, cfg.NodeSecretEncryptionKey) || !hmac.Equal(s.nodes.signingKey, cfg.NodeJWTSigningKey) {
		return nil, errors.New("node service configuration differs from the M1 store")
	}
	return s.nodes, nil
}

func (s *nodeService) verifier(purpose, value string) string {
	mac := hmac.New(sha256.New, s.pepper)
	_, _ = mac.Write([]byte(purpose + "\n" + value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *nodeService) encryptNodeSecret(nodeID, secret string) (string, error) {
	block, err := aes.NewCipher(s.encryption)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := aead.Seal(nonce, nonce, []byte(secret), []byte("cordova-node-secret-v1\n"+nodeID))
	return "v1:" + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

func (s *nodeService) decryptNodeSecret(nodeID, encoded string) (string, error) {
	if !strings.HasPrefix(encoded, "v1:") {
		return "", ErrNodeUnauthorized
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, "v1:"))
	if err != nil {
		return "", ErrNodeUnauthorized
	}
	block, err := aes.NewCipher(s.encryption)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return "", ErrNodeUnauthorized
	}
	nonce, body := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, []byte("cordova-node-secret-v1\n"+nodeID))
	if err != nil || len(plain) != 64 {
		return "", ErrNodeUnauthorized
	}
	if _, err := hex.DecodeString(string(plain)); err != nil {
		return "", ErrNodeUnauthorized
	}
	return string(plain), nil
}

func nodeAuthProof(secret, nodeID, nonce string) (string, error) {
	decoded, err := hex.DecodeString(secret)
	if err != nil || len(decoded) != 32 || !validID(nodeID) || len(nonce) < 32 {
		return "", ErrNodeUnauthorized
	}
	mac := hmac.New(sha256.New, decoded)
	_, _ = mac.Write([]byte("cordova-node-auth-v1\n" + nodeID + "\n" + nonce))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *nodeService) issueAccessToken(nodeID string, secretVersion int64, now time.Time) (string, time.Time, error) {
	if !validID(nodeID) || secretVersion < 1 {
		return "", time.Time{}, ErrNodeUnauthorized
	}
	tokenID, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	now = now.UTC().Truncate(time.Second)
	expires := now.Add(nodeAccessTTL)
	header, err := json.Marshal(nodeJWTHeader{Algorithm: "EdDSA", Type: "JWT", KeyID: s.keyID})
	if err != nil {
		return "", time.Time{}, err
	}
	claims, err := json.Marshal(nodeJWTClaims{Issuer: s.origin, Audience: "cordova-node-ws", Subject: nodeID,
		SecretVersion: secretVersion, TokenID: tokenID, IssuedAt: now.Unix(), ExpiresAt: expires.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	signature := ed25519.Sign(s.signingKey, []byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

func (s *nodeService) verifyAccessToken(token string, now time.Time) (nodeJWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 4096 {
		return nodeJWTClaims{}, ErrNodeUnauthorized
	}
	headerBytes, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	claimsBytes, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	signature, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(signature) != ed25519.SignatureSize {
		return nodeJWTClaims{}, ErrNodeUnauthorized
	}
	var header nodeJWTHeader
	var claims nodeJWTClaims
	if decodeStrictBytes(headerBytes, &header) != nil || decodeStrictBytes(claimsBytes, &claims) != nil ||
		header.Algorithm != "EdDSA" || header.Type != "JWT" || header.KeyID != s.keyID ||
		!ed25519.Verify(s.verification, []byte(parts[0]+"."+parts[1]), signature) {
		return nodeJWTClaims{}, ErrNodeUnauthorized
	}
	nowUnix := now.UTC().Unix()
	if claims.Issuer != s.origin || claims.Audience != "cordova-node-ws" || !validID(claims.Subject) ||
		claims.SecretVersion < 1 || claims.TokenID == "" || claims.IssuedAt > nowUnix+int64(nodeClockSkew.Seconds()) ||
		claims.ExpiresAt <= nowUnix || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64((nodeAccessTTL+nodeClockSkew).Seconds()) {
		return nodeJWTClaims{}, ErrNodeUnauthorized
	}
	tokenID, err := base64.RawURLEncoding.DecodeString(claims.TokenID)
	if err != nil || len(tokenID) != 32 {
		return nodeJWTClaims{}, ErrNodeUnauthorized
	}
	return claims, nil
}

func validNodeHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validID(value string) bool {
	_, err := domain.ParseID(value)
	return err == nil
}

func nodeRecordKey(nodeID string) string { return "cv:v1:node:" + nodeID }

func (s *nodeService) nodeRateKey(scope, source string) string {
	return "cv:v1:node-rate:" + s.verifier("node-rate-v1\n"+scope, source)
}

func (s *nodeService) allow(ctx context.Context, key string, limit int64) error {
	result, err := s.redis.Eval(ctx, `
local key_type=redis.call('TYPE',KEYS[1]).ok
if key_type~='none' and key_type~='string' then return redis.error_reply('node rate key type mismatch') end
local count=redis.call('INCR',KEYS[1])
if count==1 then redis.call('EXPIRE',KEYS[1],60) end
if count>tonumber(ARGV[1]) then return 0 end
return 1`, []string{key}, limit).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrNodeThrottled
	}
	return nil
}

func (s *nodeService) consumeAccessToken(ctx context.Context, token string, now time.Time) (nodeIdentity, error) {
	claims, err := s.verifyAccessToken(token, now)
	if err != nil {
		return nodeIdentity{}, err
	}
	ttlSeconds := claims.ExpiresAt - now.UTC().Unix()
	if ttlSeconds < 1 {
		return nodeIdentity{}, ErrNodeUnauthorized
	}
	result, err := s.redis.Eval(ctx, `
local node_type=redis.call('TYPE',KEYS[1]).ok
local replay_type=redis.call('TYPE',KEYS[2]).ok
if node_type~='string' or (replay_type~='none' and replay_type~='string') then return redis.error_reply('node token key type mismatch') end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not ok or type(node)~='table' or node['id']~=ARGV[1] or node['secret_version']~=tonumber(ARGV[2]) or node['revoked']~=false then return cjson.encode({unauthorized=true}) end
if redis.call('EXISTS',KEYS[2])==1 then return cjson.encode({unauthorized=true}) end
redis.call('SET',KEYS[2],'1','EX',ARGV[3])
return cjson.encode({owner_id=node['owner_id']})`,
		[]string{nodeRecordKey(claims.Subject), "cv:v1:node-jti:" + claims.TokenID}, claims.Subject, claims.SecretVersion, ttlSeconds).Text()
	if err != nil {
		return nodeIdentity{}, err
	}
	var resultData struct {
		OwnerID      string `json:"owner_id"`
		Unauthorized bool   `json:"unauthorized"`
	}
	if json.Unmarshal([]byte(result), &resultData) != nil || resultData.OwnerID == "" || resultData.Unauthorized {
		return nodeIdentity{}, ErrNodeUnauthorized
	}
	return nodeIdentity{NodeID: claims.Subject, OwnerID: resultData.OwnerID, SecretVersion: claims.SecretVersion}, nil
}

func nodeAuthError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrNodePending):
		return 428, "enrollment_pending", "The node is waiting for owner approval."
	case errors.Is(err, ErrNodeExpired):
		return 410, "not_found", "The enrollment code has expired."
	case errors.Is(err, ErrNodeConflict):
		return 409, "conflict", "The enrollment was already approved or is no longer available."
	case errors.Is(err, ErrNodeThrottled):
		return 429, "rate_limited", "Node authentication is temporarily rate limited."
	case errors.Is(err, ErrNodeUnauthorized):
		return 401, "unauthenticated", "Node authentication failed."
	default:
		return 503, "storage_unavailable", "Node authentication is temporarily unavailable."
	}
}

func writeNodeError(w http.ResponseWriter, err error) {
	status, code, message := nodeAuthError(err)
	writeError(w, status, code, message)
}
