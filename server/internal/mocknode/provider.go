package mocknode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"cordova/server/internal/api"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"

	"github.com/gorilla/websocket"
)

type Provider struct {
	url    string
	token  string
	dialer websocket.Dialer
}

func NewProvider(endpoint, token string, roots *x509.CertPool) (*Provider, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(token) < 32 || roots == nil {
		return nil, errors.New("mock node requires a wss endpoint and a private token")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: u.Hostname()}
	return &Provider{url: endpoint, token: token, dialer: websocket.Dialer{Proxy: nil, TLSClientConfig: tlsConfig, HandshakeTimeout: 5 * time.Second, EnableCompression: false, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}}, nil
}

func (p *Provider) Generate(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
	if req.MaxTokens < 1 || req.MaxTokens > 2048 || len(req.Messages) == 0 || len(req.Messages) > 128 {
		return inference.Result{}, errors.New("invalid mock inference request")
	}
	requestID, err := domain.NewID()
	if err != nil {
		return inference.Result{}, err
	}
	header := http.Header{"Authorization": []string{"Bearer " + p.token}}
	conn, response, err := p.dialer.DialContext(ctx, p.url, header)
	if err != nil {
		return inference.Result{}, errors.New("mock node is unavailable")
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrameBytes)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "request cancelled"), time.Now().Add(time.Second))
	})
	defer stop()
	messages := make([]message, len(req.Messages))
	inputTokens := 0
	for i, item := range req.Messages {
		if len(item.Content) > 256<<10 || !utf8.ValidString(item.Content) {
			return inference.Result{}, errors.New("invalid mock message")
		}
		messages[i] = message{Role: item.Role, Content: item.Content}
		inputTokens += len(strings.Fields(item.Content))
	}
	frameOut := request{Version: 1, Type: "generate", RequestID: requestID.String(), Model: api.ModelID, MaxTokens: req.MaxTokens, Messages: messages}
	encoded, err := json.Marshal(frameOut)
	if err != nil || len(encoded) > maxFrameBytes {
		return inference.Result{}, errors.New("mock node request exceeds the protocol frame limit")
	}
	writeDeadline := time.Now().Add(120 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(writeDeadline) {
		writeDeadline = deadline
	}
	_ = conn.SetWriteDeadline(writeDeadline)
	if err := conn.WriteMessage(websocket.TextMessage, encoded); err != nil {
		return inference.Result{}, errors.New("mock node request failed")
	}
	expectedSequence := 1
	outputBytes := 0
	var output strings.Builder
	completed := false
	finishReason := ""
	for !completed {
		if err := ctx.Err(); err != nil {
			return inference.Result{}, err
		}
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return inference.Result{}, errors.New("mock node disconnected during inference")
		}
		if kind != websocket.TextMessage {
			return inference.Result{}, errors.New("mock node sent a non-text frame")
		}
		var frameIn frame
		if err := decode(data, &frameIn); err != nil {
			return inference.Result{}, errors.New("mock node sent an invalid frame")
		}
		if frameIn.Version != 1 || frameIn.RequestID != requestID.String() || frameIn.Sequence != expectedSequence {
			return inference.Result{}, errors.New("mock node frame sequence is invalid")
		}
		expectedSequence++
		switch frameIn.Type {
		case "delta":
			if frameIn.Text == "" || len(frameIn.Text) > maxDeltaBytes || !utf8.ValidString(frameIn.Text) {
				return inference.Result{}, errors.New("mock node delta is invalid")
			}
			outputBytes += len(frameIn.Text)
			if outputBytes > 1<<20 {
				return inference.Result{}, errors.New("mock node output is too large")
			}
			output.WriteString(frameIn.Text)
			if err := emit(frameIn.Text); err != nil {
				return inference.Result{}, err
			}
		case "complete":
			if frameIn.Usage == nil || (frameIn.FinishReason != "stop" && frameIn.FinishReason != "length") {
				return inference.Result{}, errors.New("mock node completion is invalid")
			}
			finishReason = frameIn.FinishReason
			completed = true
		default:
			return inference.Result{}, fmt.Errorf("mock node sent unsupported frame")
		}
	}
	if err := ctx.Err(); err != nil {
		return inference.Result{}, err
	}
	completionTokens := len(strings.Fields(output.String()))
	return inference.Result{FinishReason: finishReason, Usage: inference.Usage{PromptTokens: inputTokens, CompletionTokens: completionTokens, TotalTokens: inputTokens + completionTokens}}, nil
}

func (p *Provider) Ping(ctx context.Context) error {
	u, err := url.Parse(p.url)
	if err != nil {
		return errors.New("mock node endpoint is invalid")
	}
	u.Scheme = "https"
	u.Path = "/healthz"
	u.RawPath = ""
	transport := &http.Transport{TLSClientConfig: p.dialer.TLSClientConfig.Clone(), Proxy: nil, MaxIdleConns: 1, IdleConnTimeout: 5 * time.Second}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("mock node health request is invalid")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("mock node is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("mock node is unavailable")
	}
	return nil
}
