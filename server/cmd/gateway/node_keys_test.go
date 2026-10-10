package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func clearNodeKeyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CORDOVA_NODE_SECRET_ENCRYPTION_KEY", "CORDOVA_NODE_SECRET_ENCRYPTION_KEY_FILE",
		"CORDOVA_NODE_JWT_SIGNING_KEY", "CORDOVA_NODE_JWT_SIGNING_KEY_FILE",
		"CORDOVA_MODEL_CATALOG_FILE", "CORDOVA_MODEL_CATALOG_PUBLIC_KEY", "CORDOVA_MODEL_CATALOG_PUBLIC_KEY_FILE",
	} {
		t.Setenv(name, "")
	}
}

func TestNodeAPIKeysAcceptsHexEncryptionKeyAndEd25519Seed(t *testing.T) {
	clearNodeKeyEnvironment(t)
	encryption := make([]byte, 32)
	seed := make([]byte, ed25519.SeedSize)
	for i := range encryption {
		encryption[i] = byte(i + 1)
		seed[i] = byte(255 - i)
	}
	t.Setenv("CORDOVA_NODE_SECRET_ENCRYPTION_KEY", hex.EncodeToString(encryption))
	t.Setenv("CORDOVA_NODE_JWT_SIGNING_KEY", base64.RawStdEncoding.EncodeToString(seed))
	gotEncryption, gotSigning, err := nodeAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if string(gotEncryption) != string(encryption) {
		t.Fatal("decoded encryption key differs")
	}
	if string(gotSigning) != string(ed25519.NewKeyFromSeed(seed)) {
		t.Fatal("decoded Ed25519 seed was not expanded canonically")
	}
}

func TestNodeAPIKeysRequireBothKeysAndAllowDisabledMode(t *testing.T) {
	clearNodeKeyEnvironment(t)
	if encryption, signing, err := nodeAPIKeys(); err != nil || encryption != nil || signing != nil {
		t.Fatalf("disabled node API returned encryption=%x signing=%x err=%v", encryption, signing, err)
	}
	t.Setenv("CORDOVA_NODE_SECRET_ENCRYPTION_KEY", hex.EncodeToString(make([]byte, 32)))
	if _, _, err := nodeAPIKeys(); err == nil {
		t.Fatal("node API enabled with only one key")
	}
}

func TestNodeCatalogConfigRequiresAndLoadsMatchingSettings(t *testing.T) {
	clearNodeKeyEnvironment(t)
	if data, key, err := nodeCatalogConfig(); err != nil || data != nil || key != nil {
		t.Fatalf("disabled catalog returned data=%q key=%x err=%v", data, key, err)
	}
	t.Setenv("CORDOVA_MODEL_CATALOG_PUBLIC_KEY", base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)))
	if _, _, err := nodeCatalogConfig(); err == nil {
		t.Fatal("catalog public key without catalog file was accepted")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":"00000000-0000-4000-8000-000000000001","models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	t.Setenv("CORDOVA_MODEL_CATALOG_FILE", path)
	t.Setenv("CORDOVA_MODEL_CATALOG_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	data, key, err := nodeCatalogConfig()
	if err != nil || string(data) != `{"version":"00000000-0000-4000-8000-000000000001","models":[]}` || string(key) != string(publicKey) {
		t.Fatalf("loaded catalog data=%q key=%x err=%v", data, key, err)
	}
}
