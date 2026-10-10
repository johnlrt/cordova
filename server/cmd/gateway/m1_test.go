package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cordova/server/internal/mocknode"
	trustedtokenizer "cordova/server/internal/tokenizer"
)

func TestConfiguredTokenizerRequiresCompleteSignedModelIdentity(t *testing.T) {
	legacy, enabled, err := configuredTokenizer("", "", false, nil, nil, nil)
	if err != nil || enabled {
		t.Fatalf("unconfigured development tokenizer: enabled=%t err=%v", enabled, err)
	}
	if _, ok := legacy.(mocknode.SyntheticTokenizer); !ok {
		t.Fatalf("unconfigured development tokenizer has type %T", legacy)
	}

	if _, _, err := configuredTokenizer("http://127.0.0.1:9000", "", false, nil, nil, nil); err == nil {
		t.Fatal("partial tokenizer configuration was accepted")
	}

	publicKeyBytes, err := base64.StdEncoding.DecodeString("p/bfr484uJuozmSbWU+R5NAf3Ff5yUk99DteUKmYc2c=")
	if err != nil {
		t.Fatal(err)
	}
	catalogData, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "ws", "node-catalog-snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokenizer, enabled, err := configuredTokenizer("http://127.0.0.1:9000", "s12345678901234567890123456789012", true,
		catalogData, ed25519.PublicKey(publicKeyBytes), nil)
	if err != nil || !enabled {
		t.Fatalf("valid signed model catalog rejected: enabled=%t err=%v", enabled, err)
	}
	if _, ok := tokenizer.(*trustedtokenizer.Client); !ok {
		t.Fatalf("configured tokenizer has type %T", tokenizer)
	}

	if _, _, err := configuredTokenizer("http://127.0.0.1:9000", "s12345678901234567890123456789012", true,
		[]byte(`{"version":"00000000-0000-4000-8000-000000000002","models":[]}`), ed25519.PublicKey(publicKeyBytes), nil); err == nil {
		t.Fatal("empty signed model catalog enabled trusted tokenization")
	}
}

func TestTokenizerTLSRootsLoadsOnlyValidConfiguredCertificates(t *testing.T) {
	t.Setenv("CORDOVA_TOKENIZER_CA_FILE", "")
	if roots, err := tokenizerTLSRoots(); err != nil || roots != nil {
		t.Fatalf("default system roots should not require a custom pool: roots=%v err=%v", roots, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tokenizer-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORDOVA_TOKENIZER_CA_FILE", path)
	roots, err := tokenizerTLSRoots()
	if err != nil || len(roots.Subjects()) == 0 {
		t.Fatalf("valid custom CA was rejected: roots=%v err=%v", roots, err)
	}

	if err := os.WriteFile(path, []byte("not a PEM certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tokenizerTLSRoots(); err == nil {
		t.Fatal("invalid custom CA bundle was accepted")
	}
}
