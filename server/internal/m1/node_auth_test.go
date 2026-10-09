package m1

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func newTestNodeService(t *testing.T) *nodeService {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	service, err := newNodeService(client, Config{Origin: "https://nodes.example.test", Pepper: []byte(strings.Repeat("p", 32)),
		NodeSecretEncryptionKey: []byte(strings.Repeat("e", 32)), NodeJWTSigningKey: privateKey})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNodeSecretEncryptionUsesNodeBoundAAD(t *testing.T) {
	service := newTestNodeService(t)
	secret := strings.Repeat("ab", 32)
	ciphertext, err := service.encryptNodeSecret("00000000-0000-4000-8000-000000000001", secret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.decryptNodeSecret("00000000-0000-4000-8000-000000000001", ciphertext)
	if err != nil || got != secret {
		t.Fatalf("decrypt got %q, %v", got, err)
	}
	if _, err := service.decryptNodeSecret("00000000-0000-4000-8000-000000000002", ciphertext); err == nil {
		t.Fatal("ciphertext authenticated for a different node")
	}
	encoded := strings.TrimPrefix(ciphertext, "v1:")
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if _, err := service.decryptNodeSecret("00000000-0000-4000-8000-000000000001", "v1:"+base64.RawURLEncoding.EncodeToString(data)); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestNodeProofAndAccessTokenValidation(t *testing.T) {
	service := newTestNodeService(t)
	nodeID := "00000000-0000-4000-8000-000000000001"
	secret := strings.Repeat("ab", 32)
	nonce := "0123456789abcdef0123456789abcdef0123456789abcdef"
	proof, err := nodeAuthProof(secret, nodeID, nonce)
	if err != nil {
		t.Fatal(err)
	}
	secretBytes, _ := hex.DecodeString(secret)
	mac := hmac.New(sha256.New, secretBytes)
	_, _ = mac.Write([]byte("cordova-node-auth-v1\n" + nodeID + "\n" + nonce))
	if !hmac.Equal([]byte(proof), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		t.Fatal("proof does not match the C3 HMAC contract")
	}

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	token, expires, err := service.issueAccessToken(nodeID, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.verifyAccessToken(token, now.Add(time.Minute))
	if err != nil || claims.Subject != nodeID || claims.SecretVersion != 1 || !expires.Equal(time.Unix(claims.ExpiresAt, 0)) {
		t.Fatalf("valid access token rejected: claims=%+v err=%v", claims, err)
	}
	if _, err := service.verifyAccessToken(token, expires.Add(time.Second)); err == nil {
		t.Fatal("expired token was accepted")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	claims.Issuer = "https://attacker.example"
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(claimsJSON)
	parts[2] = base64.RawURLEncoding.EncodeToString(ed25519.Sign(service.signingKey, []byte(parts[0]+"."+parts[1])))
	if _, err := service.verifyAccessToken(strings.Join(parts, "."), now); err == nil {
		t.Fatal("token with an attacker issuer was accepted")
	}
}

func TestNodeJWTRejectsNonCanonicalPrivateKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateKey[ed25519.SeedSize] ^= 1
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()
	if _, err := newNodeService(client, Config{Origin: "https://nodes.example.test", Pepper: []byte(strings.Repeat("p", 32)),
		NodeSecretEncryptionKey: []byte(strings.Repeat("e", 32)), NodeJWTSigningKey: privateKey}); err == nil {
		t.Fatal("non-canonical Ed25519 private key was accepted")
	}

}
