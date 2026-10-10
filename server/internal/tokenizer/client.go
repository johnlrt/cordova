// Package tokenizer implements the gateway client for the private C9 service.
// It never substitutes approximate counts when the service is unavailable.
package tokenizer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"cordova/server/internal/inference"
)

const (
	maxBodyBytes           = 1 << 20
	maxPrepareMessages     = 128
	maxMessageTextBytes    = 256 << 10
	maxCountTextBytes      = 1 << 20
	maxPreparedInputTokens = 8192
	maxContextTokens       = 32768
	operationDeadline      = 2 * time.Second
)

var (
	ErrUnavailable     = errors.New("trusted tokenizer is unavailable")
	ErrInvalidRequest  = errors.New("trusted tokenizer rejected the request")
	ErrModelNotFound   = errors.New("model is not available to the trusted tokenizer")
	ErrCatalogMismatch = errors.New("trusted tokenizer catalog does not match")
)

type Model struct {
	ID                string
	CatalogVersion    string
	ContextTokens     uint32
	AddBOS            bool
	ArtifactURL       string
	ArtifactSHA256    string
	ArtifactSizeBytes uint64
	LlamaCommit       string
	TokenizerSHA256   string
	TemplateSHA256    string
}

type Client struct {
	baseURL *url.URL
	token   string
	models  map[string]Model
	http    *http.Client
}

type prepareRequest struct {
	ModelID        string              `json:"model_id"`
	CatalogVersion string              `json:"catalog_version"`
	Messages       []inference.Message `json:"messages"`
}

type prepareResponse struct {
	InputTokenIDs   []uint32 `json:"input_token_ids"`
	InputTokens     uint32   `json:"input_tokens"`
	ContextTokens   uint32   `json:"context_tokens"`
	TokenizerSHA256 string   `json:"tokenizer_sha256"`
	TemplateSHA256  string   `json:"template_sha256"`
}

type countRequest struct {
	ModelID        string `json:"model_id"`
	CatalogVersion string `json:"catalog_version"`
	Text           string `json:"text"`
}

type countResponse struct {
	Tokens uint32 `json:"tokens"`
}

type readyResponse struct {
	Status         string   `json:"status"`
	CatalogVersion *string  `json:"catalog_version"`
	Models         []string `json:"models"`
}

type helperErrorResponse struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Param   json.RawMessage `json:"param"`
		Code    string          `json:"code"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// NewClient creates a no-proxy client for the private tokenizer. Plain HTTP is
// accepted only for loopback development endpoints; remote endpoints require TLS.
func NewClient(endpoint, token string, models []Model, roots *x509.CertPool) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("tokenizer endpoint must be an absolute HTTP(S) URL without credentials or query parameters")
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return nil, errors.New("plain HTTP is allowed only for loopback tokenizer development endpoints")
	}
	if len(token) < 32 || !validBearerToken(token) {
		return nil, errors.New("tokenizer bearer token must contain at least 32 printable bytes")
	}
	if parsed.Path == "/" {
		parsed.Path = ""
	}
	if strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}
	modelMap := make(map[string]Model, len(models))
	catalogVersion := ""
	for _, model := range models {
		if !validModelID(model.ID) || !validUUID(model.CatalogVersion) || model.ContextTokens == 0 || model.ContextTokens > maxContextTokens ||
			!validHash(model.TokenizerSHA256) || !validHash(model.TemplateSHA256) {
			return nil, errors.New("tokenizer model identity is invalid")
		}
		if _, exists := modelMap[model.ID]; exists {
			return nil, errors.New("tokenizer model identity is duplicated")
		}
		if catalogVersion != "" && model.CatalogVersion != catalogVersion {
			return nil, errors.New("tokenizer models must use one catalog snapshot")
		}
		catalogVersion = model.CatalogVersion
		modelMap[model.ID] = model
	}
	if len(modelMap) == 0 {
		return nil, errors.New("tokenizer client requires at least one admitted model")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
		ResponseHeaderTimeout: operationDeadline,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   operationDeadline,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{baseURL: parsed, token: token, models: modelMap, http: client}, nil
}

func (c *Client) PrepareInput(ctx context.Context, modelID string, messages []inference.Message) (inference.PreparedInput, error) {
	if ctx == nil || !validMessages(messages) {
		return inference.PreparedInput{}, ErrInvalidRequest
	}
	model, exists := c.models[modelID]
	if !exists {
		return inference.PreparedInput{}, ErrModelNotFound
	}
	request := prepareRequest{ModelID: modelID, CatalogVersion: model.CatalogVersion, Messages: messages}
	var response prepareResponse
	if err := c.post(ctx, "/internal/tokenizer/prepare", request, &response,
		"input_token_ids", "input_tokens", "context_tokens", "tokenizer_sha256", "template_sha256"); err != nil {
		return inference.PreparedInput{}, err
	}
	if len(response.InputTokenIDs) == 0 || len(response.InputTokenIDs) > maxPreparedInputTokens ||
		response.InputTokens == 0 || int(response.InputTokens) != len(response.InputTokenIDs) ||
		response.ContextTokens != model.ContextTokens ||
		response.TokenizerSHA256 != model.TokenizerSHA256 || response.TemplateSHA256 != model.TemplateSHA256 {
		return inference.PreparedInput{}, ErrCatalogMismatch
	}
	if response.InputTokens > response.ContextTokens {
		return inference.PreparedInput{}, inference.ErrContextExceeded
	}
	for _, tokenID := range response.InputTokenIDs {
		if tokenID > 2_147_483_647 {
			return inference.PreparedInput{}, ErrUnavailable
		}
	}
	return inference.PreparedInput{
		TokenIDs: append([]uint32(nil), response.InputTokenIDs...), TokenCount: int64(response.InputTokens),
		ContextTokens: response.ContextTokens, CatalogVersion: model.CatalogVersion,
	}, nil
}

// CountInput is retained for the coordinator's legacy tokenizer interface.
// Coordinators detect PrepareInput and route its exact token IDs when available.
func (c *Client) CountInput(ctx context.Context, modelID string, messages []inference.Message) (int64, error) {
	prepared, err := c.PrepareInput(ctx, modelID, messages)
	return prepared.TokenCount, err
}

func (c *Client) CountOutput(ctx context.Context, modelID, text string) (int64, error) {
	if ctx == nil || !utf8.ValidString(text) || len(text) > maxCountTextBytes {
		return 0, ErrInvalidRequest
	}
	model, exists := c.models[modelID]
	if !exists {
		return 0, ErrModelNotFound
	}
	request := countRequest{ModelID: modelID, CatalogVersion: model.CatalogVersion, Text: text}
	var response countResponse
	if err := c.post(ctx, "/internal/tokenizer/count", request, &response, "tokens"); err != nil {
		return 0, err
	}
	if response.Tokens > 2_147_483_647 {
		return 0, ErrUnavailable
	}
	return int64(response.Tokens), nil
}

// CheckReady verifies that the helper has loaded exactly the catalog snapshot
// admitted by the gateway before it accepts accounted requests.
func (c *Client) CheckReady(ctx context.Context) error {
	if ctx == nil {
		return ErrUnavailable
	}
	requestContext, cancel := context.WithTimeout(ctx, operationDeadline)
	defer cancel()
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/readyz"
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	var ready readyResponse
	if err := decodeResponse(response, &ready, "status", "catalog_version", "models"); err != nil {
		return err
	}
	if ready.Status != "ready" {
		return ErrUnavailable
	}
	if ready.CatalogVersion == nil || *ready.CatalogVersion != c.catalogVersion() || len(ready.Models) != len(c.models) {
		return ErrCatalogMismatch
	}
	seen := make(map[string]struct{}, len(ready.Models))
	for _, modelID := range ready.Models {
		if _, exists := c.models[modelID]; !exists {
			return ErrCatalogMismatch
		}
		if _, duplicate := seen[modelID]; duplicate {
			return ErrCatalogMismatch
		}
		seen[modelID] = struct{}{}
	}
	return nil
}

func (c *Client) catalogVersion() string {
	for _, model := range c.models {
		return model.CatalogVersion
	}
	return ""
}

func (c *Client) post(ctx context.Context, route string, body any, target any, requiredFields ...string) error {
	data, err := json.Marshal(body)
	if err != nil || len(data) > maxBodyBytes {
		return ErrInvalidRequest
	}
	requestContext, cancel := context.WithTimeout(ctx, operationDeadline)
	defer cancel()
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + route
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return ErrInvalidRequest
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusBadRequest:
			return c.decodeHelperError(response)
		case http.StatusNotFound:
			return ErrModelNotFound
		case http.StatusConflict:
			return ErrCatalogMismatch
		default:
			return ErrUnavailable
		}
	}
	return decodeResponse(response, target, requiredFields...)
}

func (c *Client) decodeHelperError(response *http.Response) error {
	var payload helperErrorResponse
	if err := decodeResponse(response, &payload, "error", "request_id"); err != nil {
		return ErrUnavailable
	}
	if !validUUID(payload.RequestID) || payload.Error.Message == "" || len(payload.Error.Message) > 256 ||
		len(payload.Error.Param) == 0 || string(payload.Error.Param) != "null" && payload.Error.Param[0] != '"' ||
		payload.Error.Type != "invalid_request_error" && payload.Error.Type != "server_error" && payload.Error.Type != "authentication_error" {
		return ErrUnavailable
	}
	switch payload.Error.Code {
	case "context_exceeded":
		return inference.ErrContextExceeded
	case "invalid_request":
		return ErrInvalidRequest
	default:
		return ErrUnavailable
	}
}

func decodeResponse(response *http.Response, target any, requiredFields ...string) error {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxBodyBytes || !json.Valid(data) || duplicateJSON(data) != nil {
		return ErrUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return ErrUnavailable
	}
	for _, field := range requiredFields {
		if _, exists := fields[field]; !exists {
			return ErrUnavailable
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func validMessages(messages []inference.Message) bool {
	if len(messages) == 0 || len(messages) > maxPrepareMessages {
		return false
	}
	for _, message := range messages {
		if (message.Role != "system" && message.Role != "user" && message.Role != "assistant") ||
			!utf8.ValidString(message.Content) || len(message.Content) > maxMessageTextBytes {
			return false
		}
	}
	return true
}

func validBearerToken(value string) bool {
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func validModelID(value string) bool {
	if value == "" || len(value) > 96 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	previousDash := false
	for _, char := range value {
		if char == '-' {
			if previousDash {
				return false
			}
			previousDash = true
			continue
		}
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
		previousDash = false
	}
	return true
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || strings.IndexByte("89ab", value[19]) < 0 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
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

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func duplicateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("duplicate JSON object key")
			}
			seen[name] = struct{}{}
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
}
