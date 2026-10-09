package mocknode

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"cordova/server/internal/api"
	"cordova/server/internal/domain"

	"github.com/gorilla/websocket"
)

type Server struct {
	tokenHash [32]byte
	delay     time.Duration
	slots     chan struct{}
}

func NewServer(token string, delay time.Duration) (http.Handler, error) {
	if len(token) < 32 {
		return nil, ErrConfiguration{}
	}
	s := &Server{tokenHash: sha256.Sum256([]byte(token)), delay: delay, slots: make(chan struct{}, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ws", s.serve)
	return mux, nil
}

type ErrConfiguration struct{}

func (ErrConfiguration) Error() string { return "mock node requires a token of at least 32 characters" }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	providedHash := sha256.Sum256([]byte(provided))
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(providedHash[:], s.tokenHash[:]) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, EnableCompression: false, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseProtocolError, "invalid request"), time.Now().Add(time.Second))
		return
	}
	var request request
	if err := decode(data, &request); err != nil || request.Version != 1 || request.Type != "generate" || request.Model != api.ModelID || request.MaxTokens < 1 || request.MaxTokens > 2048 || len(request.Messages) == 0 || len(request.Messages) > 128 {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid request"), time.Now().Add(time.Second))
		return
	}
	if _, err := domain.ParseID(request.RequestID); err != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid request"), time.Now().Add(time.Second))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	user := false
	inputTokens := 0
	for _, message := range request.Messages {
		if (message.Role != "system" && message.Role != "user" && message.Role != "assistant") || len(message.Content) > 256<<10 || !utf8.ValidString(message.Content) {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid request"), time.Now().Add(time.Second))
			return
		}
		if message.Role == "user" {
			user = true
		}
		inputTokens += len(strings.Fields(message.Content))
	}
	if !user {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid request"), time.Now().Add(time.Second))
		return
	}
	words := strings.Fields("[MOCK] Cordova gateway is connected. This is a synthetic response; no AI model is running yet.")
	finish := "stop"
	if len(words) > request.MaxTokens {
		words = words[:request.MaxTokens]
		finish = "length"
	}
	sequence := 1
	for i, word := range words {
		if s.delay > 0 {
			timer := time.NewTimer(s.delay)
			<-timer.C
		}
		if i > 0 {
			word = " " + word
		}
		out := frame{Version: 1, Type: "delta", RequestID: request.RequestID, Sequence: sequence, Text: word}
		sequence++
		if err := conn.WriteJSON(out); err != nil {
			return
		}
	}
	outputTokens := len(words)
	complete := frame{Version: 1, Type: "complete", RequestID: request.RequestID, Sequence: sequence, FinishReason: finish, Usage: &usage{PromptTokens: inputTokens, CompletionTokens: outputTokens, TotalTokens: inputTokens + outputTokens}}
	_ = conn.WriteJSON(complete)
}
