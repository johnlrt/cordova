package tokenizer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"cordova/server/internal/inference"
)

func (r *modelRunner) verifyProperties(ctx context.Context) error {
	data, err := r.getJSON(ctx, "/props")
	if err != nil || duplicateJSON(data) != nil {
		return errors.New("llama-server properties could not be verified")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("llama-server properties could not be verified")
	}
	for _, field := range []string{"total_slots", "model_path", "chat_template"} {
		if _, ok := fields[field]; !ok {
			return errors.New("llama-server properties are incomplete")
		}
	}
	var properties nativeProperties
	if json.Unmarshal(data, &properties) != nil || properties.TotalSlots != 1 || properties.ChatTemplate == nil ||
		!samePath(r.artifact, properties.ModelPath) {
		return errors.New("llama-server model identity does not match the signed catalog")
	}
	templateHash := sha256.Sum256([]byte(*properties.ChatTemplate))
	if hex.EncodeToString(templateHash[:]) != r.model.TemplateSHA256 {
		return errors.New("llama-server chat template does not match the signed catalog")
	}
	return nil
}

func samePath(first, second string) bool {
	firstClean := filepath.Clean(first)
	secondClean := filepath.Clean(second)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(firstClean, secondClean)
	}
	return firstClean == secondClean
}

func (r *modelRunner) get(ctx context.Context, path string, target any) error {
	data, err := r.getJSON(ctx, path)
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}
	return decodeNativeJSON(data, target, true)
}

func (r *modelRunner) post(ctx context.Context, path string, payload, target any) error {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > maxNativeRequestBytes {
		return ErrRuntimeUnavailable
	}
	return r.do(ctx, http.MethodPost, path, bytes.NewReader(data), target)
}

func (r *modelRunner) getJSON(ctx context.Context, path string) ([]byte, error) {
	return r.doBytes(ctx, http.MethodGet, path, nil)
}

func (r *modelRunner) do(ctx context.Context, method, path string, body io.Reader, target any) error {
	data, err := r.doBytes(ctx, method, path, body)
	if err != nil {
		return err
	}
	return decodeNativeJSON(data, target, true)
}

func (r *modelRunner) doBytes(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	endpoint, err := url.JoinPath(r.baseURL, path)
	if err != nil {
		return nil, ErrRuntimeUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, ErrRuntimeUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+r.apiKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, ErrRuntimeUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxNativeResponseBytes))
		return nil, ErrRuntimeUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxNativeResponseBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxNativeResponseBytes {
		return nil, ErrRuntimeUnavailable
	}
	return data, nil
}

func decodeNativeJSON(data []byte, target any, strict bool) error {
	if target == nil || !json.Valid(data) || duplicateJSON(data) != nil {
		return ErrRuntimeUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrRuntimeUnavailable
	}
	return nil
}

func (r *ProcessRuntime) CheckReady(ctx context.Context, expected []Model) ([]Model, error) {
	if ctx == nil || len(expected) == 0 {
		return nil, ErrRuntimeUnavailable
	}
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, ErrRuntimeUnavailable
	}
	actual := make([]*modelRunner, 0, len(expected))
	seen := make(map[string]struct{}, len(expected))
	modelCount := len(r.models)
	for _, model := range expected {
		runner, ok := r.models[model.ID]
		if !ok || runner.model != model {
			r.mu.RUnlock()
			return nil, ErrRuntimeUnavailable
		}
		if _, duplicate := seen[model.ID]; duplicate {
			r.mu.RUnlock()
			return nil, ErrRuntimeUnavailable
		}
		seen[model.ID] = struct{}{}
		actual = append(actual, runner)
	}
	r.mu.RUnlock()
	if len(seen) != modelCount {
		return nil, ErrRuntimeUnavailable
	}
	for _, runner := range actual {
		if err := runner.verifyProperties(ctx); err != nil {
			return nil, ErrRuntimeUnavailable
		}
	}
	return append([]Model(nil), expected...), nil
}

func (r *ProcessRuntime) PrepareInput(ctx context.Context, model Model, messages []inference.Message) ([]uint32, error) {
	runner, err := r.runner(model)
	if err != nil || !validMessages(messages) {
		return nil, ErrRuntimeUnavailable
	}
	nativeMessages := make([]nativeMessage, 0, len(messages))
	for _, message := range messages {
		nativeMessages = append(nativeMessages, nativeMessage{Role: message.Role, Content: message.Content})
	}
	var formatted nativeTemplateResponse
	if err := runner.post(ctx, "/apply-template", nativeTemplateRequest{Messages: nativeMessages}, &formatted); err != nil ||
		formatted.Prompt == nil || !utf8.ValidString(*formatted.Prompt) {
		return nil, ErrRuntimeUnavailable
	}
	var tokenized nativeTokenizeResponse
	if err := runner.post(ctx, "/tokenize", nativeTokenizeRequest{
		Content: *formatted.Prompt, AddSpecial: model.AddBOS, ParseSpecial: true, WithPieces: false,
	}, &tokenized); err != nil {
		return nil, ErrRuntimeUnavailable
	}
	return tokenized.Tokens, nil
}

func (r *ProcessRuntime) CountOutput(ctx context.Context, model Model, text string) (uint32, error) {
	runner, err := r.runner(model)
	if err != nil || !utf8.ValidString(text) || len(text) > maxCountTextBytes {
		return 0, ErrRuntimeUnavailable
	}
	var tokenized nativeTokenizeResponse
	if err := runner.post(ctx, "/tokenize", nativeTokenizeRequest{
		Content: text, AddSpecial: false, ParseSpecial: false, WithPieces: false,
	}, &tokenized); err != nil {
		return 0, ErrRuntimeUnavailable
	}
	if uint64(len(tokenized.Tokens)) > uint64(^uint32(0)) {
		return 0, ErrRuntimeUnavailable
	}
	return uint32(len(tokenized.Tokens)), nil
}

func (r *ProcessRuntime) runner(model Model) (*modelRunner, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrRuntimeUnavailable
	}
	runner, ok := r.models[model.ID]
	if !ok || runner.model != model {
		return nil, ErrRuntimeUnavailable
	}
	select {
	case <-runner.done:
		return nil, ErrRuntimeUnavailable
	default:
	}
	return runner, nil
}

func (r *ProcessRuntime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	runners := make([]*modelRunner, 0, len(r.models))
	for _, runner := range r.models {
		runners = append(runners, runner)
	}
	r.mu.Unlock()
	var closeErr error
	for _, runner := range runners {
		if err := runner.stop(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (r *modelRunner) stop() error {
	select {
	case <-r.done:
	default:
		if r.command.Process != nil {
			_ = r.command.Process.Kill()
		}
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			return errors.New("llama-server child did not stop")
		}
	}
	r.client.CloseIdleConnections()
	if err := os.Remove(r.keyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("tokenizer child credential could not be removed")
	}
	return nil
}
