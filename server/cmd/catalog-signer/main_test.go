package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"cordova/server/internal/m1"
)

func TestDecodePrivateKeyPreservesRawPrivateKey(t *testing.T) {
	seed := bytes.Repeat([]byte{0x31}, ed25519.SeedSize)
	want := ed25519.NewKeyFromSeed(seed)
	got, err := decodePrivateKey(want)
	if err != nil {
		t.Fatalf("decode raw private key: %v", err)
	}
	defer wipe(got)
	if !bytes.Equal(got, want) {
		t.Fatal("raw private key was interpreted as another key encoding")
	}
}

func TestCatalogSignerGeneratesProtectedKeyAndSignedOutput(t *testing.T) {
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "catalog-seed.hex")
	if err := run([]string{"--generate-key-file", keyPath}); err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	privateKey, err := readPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("read generated signing key: %v", err)
	}
	defer wipe(privateKey)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("generated key permissions=%o, want 600", info.Mode().Perm())
		}
	}

	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "ws", "node-catalog-snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(directory, "unsigned-template.json")
	outputPath := filepath.Join(directory, "signed-catalog.json")
	if err := os.WriteFile(inputPath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--input", inputPath, "--output", outputPath, "--private-key-file", keyPath}); err != nil {
		t.Fatalf("sign catalog: %v", err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot m1.NodeCatalogSnapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 1 || snapshot.Models[0].Signature == "" {
		t.Fatalf("output has no signed model manifest: %+v", snapshot)
	}
	if err := run([]string{"--generate-key-file", keyPath}); err == nil {
		t.Fatal("key generation overwrote an existing key file")
	}
}
