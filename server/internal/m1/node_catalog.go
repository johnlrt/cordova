package m1

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
)

const (
	maxNodeCatalogModels = 256
	MaxNodeCatalogBytes  = (1 << 20) - 4096
)

// NodeCatalogSnapshot contains signed model manifests delivered to authenticated nodes.
// Its wire shape intentionally matches node/src/protocol.rs and node/src/catalog.rs.
type NodeCatalogSnapshot struct {
	Version string                `json:"version"`
	Models  []NodeCatalogManifest `json:"models"`
}

type NodeCatalogManifest struct {
	Model              NodeCatalogModel `json:"model"`
	ArtifactURL        string           `json:"artifact_url"`
	ArtifactSHA256     string           `json:"artifact_sha256"`
	ArtifactSizeBytes  uint64           `json:"artifact_size_bytes"`
	TokenizerSHA256    string           `json:"tokenizer_sha256"`
	TemplateSHA256     string           `json:"template_sha256"`
	LlamaCommit        string           `json:"llama_commit"`
	AddBOS             bool             `json:"add_bos"`
	MinMemoryBytes     uint64           `json:"min_memory_bytes"`
	MemoryPerSlotBytes uint64           `json:"memory_per_slot_bytes"`
	LicenseSPDX        string           `json:"license_spdx"`
	Version            string           `json:"version"`
	Signature          string           `json:"signature"`
}

type NodeCatalogModel struct {
	ID                           string `json:"id"`
	Object                       string `json:"object"`
	Created                      uint64 `json:"created"`
	OwnedBy                      string `json:"owned_by"`
	ContextTokens                uint32 `json:"context_tokens"`
	MaxOutputTokens              uint32 `json:"max_output_tokens"`
	InputRatePiconeroPerMillion  string `json:"input_rate_piconero_per_million"`
	OutputRatePiconeroPerMillion string `json:"output_rate_piconero_per_million"`
	OwnerShareBPS                uint16 `json:"owner_share_bps"`
	PriceVersion                 string `json:"price_version"`
}

func parseNodeCatalog(data []byte, publicKey ed25519.PublicKey) (NodeCatalogSnapshot, error) {
	if len(data) == 0 {
		return NodeCatalogSnapshot{Version: nodeCatalogVersion, Models: []NodeCatalogManifest{}}, nil
	}
	if len(data) > MaxNodeCatalogBytes {
		return NodeCatalogSnapshot{}, errors.New("node catalog exceeds the safe WebSocket frame limit")
	}
	if err := duplicateJSON(data); err != nil {
		return NodeCatalogSnapshot{}, errors.New("node catalog contains invalid or duplicate-key JSON")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return NodeCatalogSnapshot{}, errors.New("a 32-byte catalog verification key is required")
	}
	if err := requireNodeCatalogFields(data); err != nil {
		return NodeCatalogSnapshot{}, err
	}
	var snapshot NodeCatalogSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return NodeCatalogSnapshot{}, fmt.Errorf("decode node catalog: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return NodeCatalogSnapshot{}, errors.New("node catalog must contain exactly one JSON value")
	}
	if !validID(snapshot.Version) || snapshot.Models == nil || len(snapshot.Models) > maxNodeCatalogModels {
		return NodeCatalogSnapshot{}, errors.New("node catalog version or model count is invalid")
	}
	seen := make(map[string]struct{}, len(snapshot.Models))
	for i := range snapshot.Models {
		manifest := &snapshot.Models[i]
		if err := verifyNodeCatalogManifest(*manifest, publicKey); err != nil {
			return NodeCatalogSnapshot{}, fmt.Errorf("node catalog model %d: %w", i, err)
		}
		if _, ok := seen[manifest.Model.ID]; ok {
			return NodeCatalogSnapshot{}, errors.New("node catalog contains duplicate model IDs")
		}
		seen[manifest.Model.ID] = struct{}{}
	}
	payload, err := json.Marshal(snapshot)
	if err != nil || len(payload) > MaxNodeCatalogBytes {
		return NodeCatalogSnapshot{}, errors.New("encoded node catalog exceeds the safe WebSocket frame limit")
	}
	return snapshot, nil
}

func requireNodeCatalogFields(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return errors.New("node catalog must be an object")
	}
	if err := requireJSONFields(root, "version", "models"); err != nil {
		return err
	}
	var models []json.RawMessage
	if err := json.Unmarshal(root["models"], &models); err != nil {
		return errors.New("node catalog models must be an array")
	}
	for _, rawModel := range models {
		var manifest map[string]json.RawMessage
		if err := json.Unmarshal(rawModel, &manifest); err != nil || manifest == nil {
			return errors.New("node catalog manifest must be an object")
		}
		if err := requireJSONFields(manifest, "model", "artifact_url", "artifact_sha256", "artifact_size_bytes", "tokenizer_sha256", "template_sha256", "llama_commit", "add_bos", "min_memory_bytes", "memory_per_slot_bytes", "license_spdx", "version", "signature"); err != nil {
			return err
		}
		var model map[string]json.RawMessage
		if err := json.Unmarshal(manifest["model"], &model); err != nil || model == nil {
			return errors.New("node catalog model metadata must be an object")
		}
		if err := requireJSONFields(model, "id", "object", "created", "owned_by", "context_tokens", "max_output_tokens", "input_rate_piconero_per_million", "output_rate_piconero_per_million", "owner_share_bps", "price_version"); err != nil {
			return err
		}
	}
	return nil
}

func requireJSONFields(value map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if _, exists := value[field]; !exists {
			return fmt.Errorf("node catalog is missing required field %q", field)
		}
	}
	return nil
}

func verifyNodeCatalogManifest(manifest NodeCatalogManifest, publicKey ed25519.PublicKey) error {
	model := manifest.Model
	if !validModelSlug(model.ID) || model.Object != "model" || model.OwnedBy != "cordova" ||
		model.ContextTokens == 0 || model.MaxOutputTokens == 0 || model.MaxOutputTokens > model.ContextTokens ||
		model.OwnerShareBPS > 10_000 || !validCatalogMoney(model.InputRatePiconeroPerMillion) ||
		!validCatalogMoney(model.OutputRatePiconeroPerMillion) || !validID(model.PriceVersion) {
		return errors.New("model metadata is invalid")
	}
	if !validNodeHash(manifest.ArtifactSHA256) || !validNodeHash(manifest.TokenizerSHA256) ||
		!validNodeHash(manifest.TemplateSHA256) || manifest.ArtifactSizeBytes == 0 || manifest.ArtifactSizeBytes > 1<<50 ||
		manifest.MinMemoryBytes == 0 || manifest.MemoryPerSlotBytes == 0 {
		return errors.New("artifact hashes or memory bounds are invalid")
	}
	parsedURL, err := url.Parse(manifest.ArtifactURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Hostname() == "" || parsedURL.Path == "" ||
		parsedURL.User != nil || parsedURL.Fragment != "" || parsedURL.Port() != "" && parsedURL.Port() != "443" {
		return errors.New("artifact URL must be HTTPS without credentials or fragments")
	}
	if len(manifest.LlamaCommit) != 40 || !validLowerHex(manifest.LlamaCommit) ||
		manifest.LicenseSPDX == "" || len(manifest.LicenseSPDX) > 128 || !validID(manifest.Version) {
		return errors.New("license or llama.cpp revision is invalid")
	}
	signature, err := base64.StdEncoding.DecodeString(manifest.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("manifest signature is invalid")
	}
	canonical, err := canonicalUnsignedManifest(manifest)
	if err != nil || !ed25519.Verify(publicKey, canonical, signature) {
		return errors.New("manifest signature does not match the configured catalog key")
	}
	return nil
}

func canonicalUnsignedManifest(manifest NodeCatalogManifest) ([]byte, error) {
	manifest.Signature = ""
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("manifest must be a JSON object")
	}
	delete(object, "signature")
	var canonical bytes.Buffer
	if err := writeCanonicalJSON(&canonical, object); err != nil {
		return nil, err
	}
	return canonical.Bytes(), nil
}

func writeCanonicalJSON(dst *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		dst.WriteByte('{')
		for i, key := range keys {
			if i != 0 {
				dst.WriteByte(',')
			}
			if err := writeCanonicalString(dst, key); err != nil {
				return err
			}
			dst.WriteByte(':')
			if err := writeCanonicalJSON(dst, value[key]); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	case []any:
		dst.WriteByte('[')
		for i, item := range value {
			if i != 0 {
				dst.WriteByte(',')
			}
			if err := writeCanonicalJSON(dst, item); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	case string:
		return writeCanonicalString(dst, value)
	case json.Number:
		dst.WriteString(value.String())
	case bool:
		if value {
			dst.WriteString("true")
		} else {
			dst.WriteString("false")
		}
	case nil:
		dst.WriteString("null")
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func writeCanonicalString(dst *bytes.Buffer, value string) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2028`), []byte("\xe2\x80\xa8"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2029`), []byte("\xe2\x80\xa9"))
	dst.Write(encoded)
	return nil
}

func validCatalogMoney(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || value[0] == '0' || len(value) > 19 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validLowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return value != ""
}
