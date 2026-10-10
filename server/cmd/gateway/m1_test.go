package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

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
