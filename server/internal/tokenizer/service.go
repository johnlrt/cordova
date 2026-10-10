package tokenizer

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

const (
	maxQueuedOperations = 100
	maxQueueWait        = 500 * time.Millisecond
)

var (
	ErrRuntimeUnavailable = errors.New("tokenizer runtime is unavailable")
	ErrQueueFull          = errors.New("tokenizer queue is full")
	fallbackRequestIDs    atomic.Uint64
)

// Runtime is the native, model-pinned implementation behind the C9 HTTP API.
// It must not log or retain request data after each call.
type Runtime interface {
	CheckReady(context.Context, []Model) ([]Model, error)
	PrepareInput(context.Context, Model, []inference.Message) ([]uint32, error)
	CountOutput(context.Context, Model, string) (uint32, error)
}

type modelLimiter struct {
	slots chan struct{}
}

type Service struct {
	catalogVersion string
	token          []byte
	models         map[string]Model
	limiters       map[string]*modelLimiter
	queued         chan struct{}
	runtime        Runtime
}

type prepareServiceRequest struct {
	ModelID        string           `json:"model_id"`
	CatalogVersion string           `json:"catalog_version"`
	Messages       []serviceMessage `json:"messages"`
}

type serviceMessage struct {
	Role    *string `json:"role"`
	Content *string `json:"content"`
}

type countServiceRequest struct {
	ModelID        string `json:"model_id"`
	CatalogVersion string `json:"catalog_version"`
	Text           string `json:"text"`
}

// NewService wires validated catalog identities to a native tokenizer runtime.
// A service without a ready native runtime can serve health checks but reports
// not_ready and fails all tokenization requests closed.
func NewService(catalogVersion, helperToken string, models []Model, runtime Runtime) (*Service, error) {
	if !validUUID(catalogVersion) || len(helperToken) < 32 || !validBearerToken(helperToken) || len(models) == 0 || len(models) > 256 {
		return nil, errors.New("tokenizer service configuration is invalid")
	}
	configured := make(map[string]Model, len(models))
	limiters := make(map[string]*modelLimiter, len(models))
	for _, model := range models {
		if !validModelID(model.ID) || model.CatalogVersion != catalogVersion || model.ContextTokens == 0 ||
			model.ContextTokens > maxContextTokens || !validHash(model.TokenizerSHA256) || !validHash(model.TemplateSHA256) {
			return nil, errors.New("tokenizer service model identity is invalid")
		}
		if _, exists := configured[model.ID]; exists {
			return nil, errors.New("tokenizer service model identity is duplicated")
		}
		configured[model.ID] = model
		limiters[model.ID] = &modelLimiter{slots: make(chan struct{}, 2)}
	}
	return &Service{
		catalogVersion: catalogVersion,
		token:          []byte(helperToken),
		models:         configured,
		limiters:       limiters,
		queued:         make(chan struct{}, maxQueuedOperations),
		runtime:        runtime,
	}, nil
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/tokenizer/prepare", s.prepare)
	mux.HandleFunc("/internal/tokenizer/count", s.count)
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/", s.notFound)
	return mux
}

func (s *Service) prepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.authorized(r) {
		writeServiceError(w, http.StatusUnauthorized, "authentication_error", "unauthenticated", "authentication required", 0)
		return
	}
	var input prepareServiceRequest
	if !decodeServiceRequest(w, r, &input, "model_id", "catalog_version", "messages") {
		return
	}
	model, ok := s.model(w, input.ModelID, input.CatalogVersion)
	if !ok {
		return
	}
	messages, valid := decodeServiceMessages(input.Messages)
	if !valid {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationDeadline)
	defer cancel()
	release, err := s.acquire(ctx, model.ID)
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	defer release()
	if s.runtime == nil {
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
		return
	}
	ids, err := s.runtime.PrepareInput(ctx, model, messages)
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	if len(ids) == 0 || len(ids) > maxPreparedInputTokens {
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
		return
	}
	if uint64(len(ids)) > uint64(model.ContextTokens) {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "context_exceeded", "model context limit exceeded", 0)
		return
	}
	for _, id := range ids {
		if id > 2_147_483_647 {
			writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
			return
		}
	}
	writeServiceJSON(w, http.StatusOK, prepareResponse{
		InputTokenIDs: ids, InputTokens: uint32(len(ids)), ContextTokens: model.ContextTokens,
		TokenizerSHA256: model.TokenizerSHA256, TemplateSHA256: model.TemplateSHA256,
	})
}

func (s *Service) count(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.authorized(r) {
		writeServiceError(w, http.StatusUnauthorized, "authentication_error", "unauthenticated", "authentication required", 0)
		return
	}
	var input countServiceRequest
	if !decodeServiceRequest(w, r, &input, "model_id", "catalog_version", "text") {
		return
	}
	model, ok := s.model(w, input.ModelID, input.CatalogVersion)
	if !ok {
		return
	}
	if !utf8.ValidString(input.Text) || len(input.Text) > maxCountTextBytes {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationDeadline)
	defer cancel()
	release, err := s.acquire(ctx, model.ID)
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	defer release()
	if s.runtime == nil {
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
		return
	}
	tokens, err := s.runtime.CountOutput(ctx, model, input.Text)
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	if tokens > 2_147_483_647 {
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
		return
	}
	writeServiceJSON(w, http.StatusOK, countResponse{Tokens: tokens})
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeServiceJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.authorized(r) {
		writeServiceError(w, http.StatusUnauthorized, "authentication_error", "unauthenticated", "authentication required", 0)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operationDeadline)
	defer cancel()
	if s.runtime == nil {
		writeServiceJSON(w, http.StatusOK, map[string]any{"status": "not_ready", "catalog_version": nil, "models": []string{}})
		return
	}
	want := make([]Model, 0, len(s.models))
	for _, model := range s.models {
		want = append(want, model)
	}
	sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
	ready, err := s.runtime.CheckReady(ctx, want)
	if err != nil || !sameModelSet(want, ready) {
		writeServiceJSON(w, http.StatusOK, map[string]any{"status": "not_ready", "catalog_version": nil, "models": []string{}})
		return
	}
	ids := make([]string, 0, len(ready))
	for _, model := range ready {
		ids = append(ids, model.ID)
	}
	sort.Strings(ids)
	writeServiceJSON(w, http.StatusOK, map[string]any{"status": "ready", "catalog_version": s.catalogVersion, "models": ids})
}

func (s *Service) notFound(w http.ResponseWriter, r *http.Request) {
	writeServiceError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found", 0)
}

func (s *Service) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := []byte(strings.TrimPrefix(value, prefix))
	return len(provided) == len(s.token) && subtle.ConstantTimeCompare(provided, s.token) == 1
}

func (s *Service) model(w http.ResponseWriter, id, catalogVersion string) (Model, bool) {
	if !validModelID(id) || !validUUID(catalogVersion) {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return Model{}, false
	}
	model, exists := s.models[id]
	if !exists {
		writeServiceError(w, http.StatusNotFound, "invalid_request_error", "not_found", "model not found", 0)
		return Model{}, false
	}
	if catalogVersion != s.catalogVersion {
		writeServiceError(w, http.StatusConflict, "invalid_request_error", "conflict", "catalog version mismatch", 0)
		return Model{}, false
	}
	return model, true
}

func decodeServiceMessages(input []serviceMessage) ([]inference.Message, bool) {
	if len(input) == 0 || len(input) > maxPrepareMessages {
		return nil, false
	}
	messages := make([]inference.Message, 0, len(input))
	for _, item := range input {
		if item.Role == nil || item.Content == nil {
			return nil, false
		}
		messages = append(messages, inference.Message{Role: *item.Role, Content: *item.Content})
	}
	if !validMessages(messages) {
		return nil, false
	}
	return messages, true
}

func (s *Service) acquire(ctx context.Context, modelID string) (func(), error) {
	limiter := s.limiters[modelID]
	if limiter == nil {
		return nil, ErrModelNotFound
	}
	select {
	case limiter.slots <- struct{}{}:
		return func() { <-limiter.slots }, nil
	default:
	}
	select {
	case s.queued <- struct{}{}:
		defer func() { <-s.queued }()
	default:
		return nil, ErrQueueFull
	}
	timer := time.NewTimer(maxQueueWait)
	defer timer.Stop()
	select {
	case limiter.slots <- struct{}{}:
		return func() { <-limiter.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrQueueFull
	}
}

func (s *Service) writeOperationError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	switch {
	case errors.Is(err, inference.ErrContextExceeded):
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "context_exceeded", "model context limit exceeded", 0)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrRuntimeUnavailable):
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
	case errors.Is(err, ErrQueueFull):
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "rate_limited", "tokenizer is busy", 1)
	default:
		writeServiceError(w, http.StatusServiceUnavailable, "server_error", "tokenizer_unavailable", "tokenizer is unavailable", 1)
	}
}

func decodeServiceRequest(w http.ResponseWriter, r *http.Request, target any, requiredFields ...string) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.ContentLength > maxBodyBytes {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxBodyBytes || !json.Valid(data) || duplicateJSON(data) != nil {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return false
	}
	for _, field := range requiredFields {
		raw, exists := fields[field]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		writeServiceError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request is invalid", 0)
		return false
	}
	return true
}

func sameModelSet(expected, actual []Model) bool {
	if len(expected) == 0 || len(expected) != len(actual) {
		return false
	}
	want := make(map[string]Model, len(expected))
	for _, model := range expected {
		want[model.ID] = model
	}
	for _, model := range actual {
		if expectedModel, exists := want[model.ID]; !exists || expectedModel != model {
			return false
		}
		delete(want, model.ID)
	}
	return len(want) == 0
}

func writeMethodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	writeServiceError(w, http.StatusMethodNotAllowed, "invalid_request_error", "invalid_request", "method not allowed", 0)
}

func writeServiceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeServiceError(w http.ResponseWriter, status int, errorType, code, message string, retryAfter int) {
	requestID := serviceRequestID()
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeServiceJSON(w, status, map[string]any{
		"error":      map[string]any{"message": message, "type": errorType, "param": nil, "code": code},
		"request_id": requestID,
	})
}

func serviceRequestID() string {
	requestID, err := domain.NewID()
	if err == nil {
		return requestID.String()
	}
	var value [16]byte
	binary.BigEndian.PutUint64(value[:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(value[8:], fallbackRequestIDs.Add(1))
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
