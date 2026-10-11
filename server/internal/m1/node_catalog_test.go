package m1

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const nodeCatalogTestVersion = "00000000-0000-4000-8000-000000000002"

func TestParseNodeCatalogVerifiesSignedManifest(t *testing.T) {
	data, publicKey := signedNodeCatalogForTest(t)
	snapshot, err := parseNodeCatalog(data, publicKey)
	if err != nil {
		t.Fatalf("parse signed catalog: %v", err)
	}
	if snapshot.Version != nodeCatalogTestVersion || len(snapshot.Models) != 1 || snapshot.Models[0].Model.ID != "cordova-chat-small-v1" {
		t.Fatalf("unexpected catalog snapshot: %+v", snapshot)
	}

	wrongKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("w", ed25519.SeedSize)))
	if _, err := parseNodeCatalog(data, wrongKey.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("catalog signed by a different key was accepted")
	}

	var tampered NodeCatalogSnapshot
	if err := json.Unmarshal(data, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Models[0].ArtifactURL = "https://attacker.example/model.gguf"
	tamperedData, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseNodeCatalog(tamperedData, publicKey); err == nil {
		t.Fatal("modified manifest was accepted")
	}
}

func TestNodeCatalogRustGoldenFixture(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/fixtures/ws/node-catalog-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := base64.StdEncoding.DecodeString("p/bfr484uJuozmSbWU+R5NAf3Ff5yUk99DteUKmYc2c=")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := parseNodeCatalog(data, ed25519.PublicKey(publicKey))
	if err != nil {
		t.Fatalf("verify shared Go/Rust catalog vector: %v", err)
	}
	if snapshot.Version != nodeCatalogTestVersion || len(snapshot.Models) != 1 {
		t.Fatalf("unexpected shared catalog: %+v", snapshot)
	}
}

func TestSignNodeCatalogJSONProducesVerifiedSnapshot(t *testing.T) {
	data, publicKey := signedNodeCatalogForTest(t)
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("c", ed25519.SeedSize)))
	var template NodeCatalogSnapshot
	if err := json.Unmarshal(data, &template); err != nil {
		t.Fatal(err)
	}
	template.Models[0].Signature = "operator-supplied-value-is-replaced"
	templateData, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignNodeCatalogJSON(templateData, privateKey)
	if err != nil {
		t.Fatalf("sign catalog: %v", err)
	}
	snapshot, err := parseNodeCatalog(signed, publicKey)
	if err != nil {
		t.Fatalf("verify signed catalog: %v", err)
	}
	if snapshot.Version != nodeCatalogTestVersion || len(snapshot.Models) != 1 || snapshot.Models[0].Signature == "" {
		t.Fatalf("unexpected signed catalog: %+v", snapshot)
	}
	if !bytes.HasSuffix(signed, []byte("\n")) {
		t.Fatal("signed catalog output is not newline-terminated")
	}
}

func TestSignNodeCatalogJSONRejectsMalformedOrAmbiguousInput(t *testing.T) {
	data, _ := signedNodeCatalogForTest(t)
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("c", ed25519.SeedSize)))
	cases := [][]byte{
		[]byte(`{"version":"00000000-0000-4000-8000-000000000002","version":"00000000-0000-4000-8000-000000000002","models":[]}`),
		[]byte(strings.Replace(string(data), `"version":"`+nodeCatalogTestVersion+`"`, `"version":"`+nodeCatalogTestVersion+`","unexpected":true`, 1)),
	}
	for i, input := range cases {
		if _, err := SignNodeCatalogJSON(input, privateKey); err == nil {
			t.Fatalf("malformed catalog template %d was signed", i)
		}
	}
	if _, err := SignNodeCatalogJSON(data, ed25519.PrivateKey([]byte("short"))); err == nil {
		t.Fatal("invalid signing key was accepted")
	}
}

func TestSignNodeCatalogSnapshotRejectsDuplicateModelIDs(t *testing.T) {
	data, _ := signedNodeCatalogForTest(t)
	var snapshot NodeCatalogSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Models = append(snapshot.Models, snapshot.Models[0])
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("c", ed25519.SeedSize)))
	if _, err := SignNodeCatalogSnapshot(snapshot, privateKey); err == nil {
		t.Fatal("duplicate model IDs were signed")
	}
}

func TestParseNodeCatalogRejectsMalformedCatalogs(t *testing.T) {
	data, publicKey := signedNodeCatalogForTest(t)
	var snapshot NodeCatalogSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}

	duplicate := snapshot
	duplicate.Models = append(append([]NodeCatalogManifest{}, snapshot.Models...), snapshot.Models[0])
	duplicateData, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseNodeCatalog(duplicateData, publicKey); err == nil {
		t.Fatal("duplicate model ID was accepted")
	}

	missing := strings.Replace(string(data), `"created":1,`, "", 1)
	if _, err := parseNodeCatalog([]byte(missing), publicKey); err == nil {
		t.Fatal("manifest missing a required field was accepted")
	}

	unknown := strings.Replace(string(data), `"version":"`+nodeCatalogVersion+`"`, `"version":"`+nodeCatalogVersion+`","unexpected":true`, 1)
	if _, err := parseNodeCatalog([]byte(unknown), publicKey); err == nil {
		t.Fatal("catalog with an unknown field was accepted")
	}
	duplicateKey := strings.Replace(string(data), `"version":"`+nodeCatalogVersion+`"`, `"version":"`+nodeCatalogVersion+`","version":"`+nodeCatalogVersion+`"`, 1)
	if _, err := parseNodeCatalog([]byte(duplicateKey), publicKey); err == nil {
		t.Fatal("catalog with a duplicate JSON key was accepted")
	}

	if _, err := parseNodeCatalog(data, nil); err == nil {
		t.Fatal("configured catalog without a trust key was accepted")
	}
}

func TestParseNodeCatalogRejectsOversizedEncodedFrame(t *testing.T) {
	_, publicKey := signedNodeCatalogForTest(t)
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("c", ed25519.SeedSize)))
	manifest := NodeCatalogManifest{
		Model: NodeCatalogModel{
			ID: "cordova-chat-small-v1", Object: "model", Created: 1, OwnedBy: "cordova",
			ContextTokens: 4096, MaxOutputTokens: 2048,
			InputRatePiconeroPerMillion: "0", OutputRatePiconeroPerMillion: "0",
			OwnerShareBPS: 8000, PriceVersion: nodeCatalogVersion,
		},
		ArtifactURL:    "https://models.example.test/model.gguf?" + strings.Repeat("&", 180_000),
		ArtifactSHA256: strings.Repeat("a", 64), ArtifactSizeBytes: 1024,
		TokenizerSHA256: strings.Repeat("b", 64), TemplateSHA256: strings.Repeat("c", 64),
		LlamaCommit: strings.Repeat("d", 40), AddBOS: true,
		MinMemoryBytes: 1 << 30, MemoryPerSlotBytes: 1 << 30,
		LicenseSPDX: "Apache-2.0", Version: nodeCatalogVersion,
	}
	canonical, err := canonicalUnsignedManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, canonical))
	snapshot := NodeCatalogSnapshot{Version: nodeCatalogTestVersion, Models: []NodeCatalogManifest{manifest}}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(snapshot); err != nil {
		t.Fatal(err)
	}
	if data.Len() >= MaxNodeCatalogBytes {
		t.Fatalf("test catalog input unexpectedly large: %d", data.Len())
	}
	if _, err := parseNodeCatalog(data.Bytes(), publicKey); err == nil {
		t.Fatal("catalog whose JSON escaping exceeds the node frame limit was accepted")
	}
}

func signedNodeCatalogForTest(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	seed := []byte(strings.Repeat("c", ed25519.SeedSize))
	privateKey := ed25519.NewKeyFromSeed(seed)
	manifest := NodeCatalogManifest{
		Model: NodeCatalogModel{
			ID: "cordova-chat-small-v1", Object: "model", Created: 1, OwnedBy: "cordova",
			ContextTokens: 4096, MaxOutputTokens: 2048,
			InputRatePiconeroPerMillion: "0", OutputRatePiconeroPerMillion: "0",
			OwnerShareBPS: 8000, PriceVersion: nodeCatalogVersion,
		},
		ArtifactURL:    "https://models.example.test/cordova-chat-small-v1.gguf?download=1&source=catalog",
		ArtifactSHA256: strings.Repeat("a", 64), ArtifactSizeBytes: 1024,
		TokenizerSHA256: strings.Repeat("b", 64), TemplateSHA256: strings.Repeat("c", 64),
		LlamaCommit: strings.Repeat("d", 40), AddBOS: true,
		MinMemoryBytes: 1 << 30, MemoryPerSlotBytes: 1 << 30,
		LicenseSPDX: "Apache-2.0", Version: nodeCatalogVersion,
	}
	canonical, err := canonicalUnsignedManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, canonical))
	snapshot := NodeCatalogSnapshot{Version: nodeCatalogTestVersion, Models: []NodeCatalogManifest{manifest}}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return data, privateKey.Public().(ed25519.PublicKey)
}
