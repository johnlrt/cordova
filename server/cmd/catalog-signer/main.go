package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"cordova/server/internal/m1"
)

const maxSigningKeyFileBytes = 4096

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-signer:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("catalog-signer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "unsigned catalog JSON template")
	outputPath := flags.String("output", "", "new signed catalog output path")
	keyPath := flags.String("private-key-file", "", "offline Ed25519 private key file")
	generateKeyPath := flags.String("generate-key-file", "", "create a new offline Ed25519 seed file")
	if err := flags.Parse(args); err != nil {
		return errors.New("usage: catalog-signer --input FILE --output NEW_FILE --private-key-file KEY_FILE | --generate-key-file NEW_KEY_FILE")
	}
	if strings.TrimSpace(*generateKeyPath) != "" {
		if flags.NArg() != 0 || strings.TrimSpace(*inputPath) != "" || strings.TrimSpace(*outputPath) != "" || strings.TrimSpace(*keyPath) != "" {
			return errors.New("key generation cannot be combined with catalog signing flags")
		}
		return generateSigningKey(*generateKeyPath)
	}
	if flags.NArg() != 0 || strings.TrimSpace(*inputPath) == "" || strings.TrimSpace(*outputPath) == "" || strings.TrimSpace(*keyPath) == "" {
		return errors.New("usage: catalog-signer --input FILE --output NEW_FILE --private-key-file KEY_FILE | --generate-key-file NEW_KEY_FILE")
	}
	inputAbsolute, err := filepath.Abs(*inputPath)
	if err != nil {
		return errors.New("catalog input path is invalid")
	}
	outputAbsolute, err := filepath.Abs(*outputPath)
	if err != nil || samePath(inputAbsolute, outputAbsolute) {
		return errors.New("catalog output must be a different path from the input")
	}
	inputFile, err := os.Open(inputAbsolute)
	if err != nil {
		return errors.New("catalog input could not be opened")
	}
	input, readErr := io.ReadAll(io.LimitReader(inputFile, m1.MaxNodeCatalogBytes+1))
	closeErr := inputFile.Close()
	if readErr != nil || closeErr != nil || len(input) == 0 || len(input) > m1.MaxNodeCatalogBytes {
		return errors.New("catalog input is empty, oversized or could not be read")
	}
	privateKey, err := readPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	defer wipe(privateKey)
	signed, err := m1.SignNodeCatalogJSON(input, privateKey)
	if err != nil {
		return fmt.Errorf("catalog signing failed: %w", err)
	}
	if err := writeNewFile(outputAbsolute, signed); err != nil {
		return err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	fmt.Printf("Signed %d model manifests for catalog %s.\n", countModels(signed), catalogVersion(signed))
	fmt.Printf("Catalog public key (base64): %s\n", base64.StdEncoding.EncodeToString(publicKey))
	fmt.Printf("Wrote verified catalog to %s\n", outputAbsolute)
	return nil
}

func generateSigningKey(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errors.New("private key output path is invalid")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("operating system random source could not create a signing key")
	}
	defer wipe(privateKey)
	seed := privateKey.Seed()
	defer wipe(seed)
	keyFile := []byte(hex.EncodeToString(seed) + "\n")
	defer wipe(keyFile)
	if err := writeNewFile(absolute, keyFile); err != nil {
		return err
	}
	fmt.Printf("Created offline Ed25519 seed at %s\n", absolute)
	fmt.Printf("Catalog public key (base64): %s\n", base64.StdEncoding.EncodeToString(publicKey))
	return nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private key file could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("private key path must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private key file permissions must be 0600 or stricter")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSigningKeyFileBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxSigningKeyFileBytes {
		wipe(data)
		return nil, errors.New("private key file is empty or oversized")
	}
	defer wipe(data)
	key, err := decodePrivateKey(data)
	if err != nil {
		return nil, errors.New("private key must encode an Ed25519 seed or private key")
	}
	return key, nil
}

func decodePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if len(data) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(data), nil
	}
	if len(data) == ed25519.PrivateKeySize {
		if key, ok := privateKeyFromBytes(data); ok {
			return key, nil
		}
	}
	trimmed := strings.TrimSpace(string(data))
	if decoded, err := hex.DecodeString(trimmed); err == nil {
		if key, ok := privateKeyFromBytes(decoded); ok {
			wipe(decoded)
			return key, nil
		}
		wipe(decoded)
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(trimmed)
		if err != nil {
			continue
		}
		key, ok := privateKeyFromBytes(decoded)
		wipe(decoded)
		if ok {
			return key, nil
		}
	}
	if len(data) == ed25519.PrivateKeySize {
		if key, ok := privateKeyFromBytes(data); ok {
			return key, nil
		}
	}
	if len(trimmed) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed([]byte(trimmed)), nil
	}
	return nil, errors.New("invalid Ed25519 key")
}

func privateKeyFromBytes(value []byte) (ed25519.PrivateKey, bool) {
	switch len(value) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(value), true
	case ed25519.PrivateKeySize:
		derived := ed25519.NewKeyFromSeed(value[:ed25519.SeedSize])
		if bytes.Equal(derived, value) {
			return derived, true
		}
		wipe(derived)
	}
	return nil, false
}

func writeNewFile(path string, data []byte) error {
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("catalog output already exists")
		}
		return errors.New("catalog output could not be created")
	}
	removeIncompleteOutput := true
	defer func() {
		_ = output.Close()
		if removeIncompleteOutput {
			_ = os.Remove(path)
		}
	}()
	if _, err := output.Write(data); err != nil {
		return errors.New("signed catalog could not be written")
	}
	if err := output.Sync(); err != nil {
		return errors.New("signed catalog could not be synced")
	}
	if err := output.Close(); err != nil {
		return errors.New("signed catalog could not be closed")
	}
	removeIncompleteOutput = false
	return nil
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func countModels(data []byte) int {
	var snapshot m1.NodeCatalogSnapshot
	if json.Unmarshal(data, &snapshot) != nil {
		return 0
	}
	return len(snapshot.Models)
}

func catalogVersion(data []byte) string {
	var snapshot m1.NodeCatalogSnapshot
	if json.Unmarshal(data, &snapshot) != nil {
		return ""
	}
	return snapshot.Version
}

func wipe(data []byte) {
	for i := range data {
		data[i] = 0
	}
}
