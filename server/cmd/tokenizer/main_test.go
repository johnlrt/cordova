package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"reflect"
	"testing"
)

func TestDecodePublicKeySupportsCatalogEncodings(t *testing.T) {
	want := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(want),
		base64.RawStdEncoding.EncodeToString(want),
		hex.EncodeToString(want),
	} {
		actual, err := decodePublicKey(encoded)
		if err != nil || !reflect.DeepEqual(actual, want) {
			t.Fatalf("decodePublicKey(%q) = %x, %v", encoded, actual, err)
		}
	}
	if _, err := decodePublicKey("not-a-catalog-key"); err == nil {
		t.Fatal("invalid key was accepted")
	}
}

func TestIsLoopbackAddressRequiresLiteralIP(t *testing.T) {
	loopback, err := isLoopbackAddress("127.0.0.1:9001")
	if err != nil || !loopback {
		t.Fatalf("loopback address = %v, %v", loopback, err)
	}
	loopback, err = isLoopbackAddress("0.0.0.0:9001")
	if err != nil || loopback {
		t.Fatalf("wildcard address = %v, %v", loopback, err)
	}
	if _, err := isLoopbackAddress("localhost:9001"); err == nil {
		t.Fatal("hostname listener was accepted")
	}
}
