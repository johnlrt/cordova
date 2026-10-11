package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

type Handler struct {
	store        *Store
	cfg          Config
	nodes        *nodeService
	catalog      NodeCatalogSnapshot
	ledger       AccountingReader
	authenticate api.Authenticator
	requests     *api.RequestRegistry
	ready        func(context.Context) error
}

type AccountingReader interface {
	Balance(context.Context, string) (billing.Balance, error)
	Request(context.Context, string) (billing.RequestCheckpoint, uint64, error)
	Operation(context.Context, string) (billing.Entry, error)
	Ready() bool
}

func NewHandler(cfg Config, store *Store, provider inference.Provider, readiness ...func(context.Context) error) (http.Handler, error) {
	return newHandler(cfg, store, provider, nil, nil, readiness...)
}

func NewHandlerWithLedger(cfg Config, store *Store, provider inference.Provider, ledger AccountingReader,
	readiness ...func(context.Context) error) (http.Handler, error) {
	if ledger == nil {
		return nil, errors.New("M3 request ledger is required")
	}
	return newHandler(cfg, store, provider, nil, ledger, readiness...)
}

func NewHandlerWithLedgerAndExecutor(cfg Config, store *Store, executor api.CompletionExecutor, ledger AccountingReader,
	readiness ...func(context.Context) error) (http.Handler, error) {
	if ledger == nil || executor == nil {
		return nil, errors.New("M3 ledger and completion executor are required")
	}
	return newHandler(cfg, store, nil, executor, ledger, readiness...)
}

func newHandler(cfg Config, store *Store, provider inference.Provider, executor api.CompletionExecutor, ledger AccountingReader,
	readiness ...func(context.Context) error) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("M1 store is required")
	}
	var chat http.Handler
	var err error
	requests := api.NewRequestRegistry()
	if executor != nil {
		chat, err = api.NewWithAuthAndExecutorRuntime(executor, store.Authenticate, store.AddAccountedUsage, store, requests)
	} else {
		chat, err = api.NewWithAuth(provider, store.Authenticate, store.AddUsage)
	}
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	catalog, err := ParseNodeCatalog(cfg.NodeCatalogSnapshot, cfg.NodeCatalogPublicKey)
	if err != nil {
		return nil, err
	}
	h := &Handler{store: store, cfg: cfg, catalog: catalog, ledger: ledger, authenticate: store.Authenticate, requests: requests}
	if len(cfg.NodeSecretEncryptionKey) > 0 || len(cfg.NodeJWTSigningKey) > 0 {
		h.nodes, err = store.ensureNodeService(cfg)
		if err != nil {
			return nil, err
		}
	}
	if len(readiness) > 0 {
		h.ready = readiness[0]
	}
	mode := cfg.InferenceMode
	if mode == "" {
		mode = "mock"
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "stage": "m1", "mode": mode})
	})
	mux.Handle("/v1/", chat)
	mux.HandleFunc("/api/auth/register", h.register)
	mux.HandleFunc("/api/auth/login", h.login)
	mux.HandleFunc("/api/auth/recover", h.recover)
	mux.HandleFunc("/api/auth/logout", h.logout)
	mux.HandleFunc("/api/session", h.session)
	mux.HandleFunc("/api/keys", h.keys)
	mux.HandleFunc("/api/keys/", h.revokeKey)
	mux.HandleFunc("/api/usage", h.usage)
	mux.HandleFunc("/api/billing/balance", h.billingBalance)
	mux.HandleFunc("/api/requests/{id}", h.requestStatus)
	mux.HandleFunc("/api/live-ticket", h.liveTicket)
	mux.HandleFunc("/api/live", h.live)
	if h.nodes != nil {
		mux.HandleFunc("/api/nodes", h.nodesInventory)
		mux.HandleFunc("/api/nodes/link", h.linkNode)
		mux.HandleFunc("/api/nodes/{id}", h.nodeDetail)
		mux.HandleFunc("/api/nodes/{id}/revoke", h.revokeNode)
		mux.HandleFunc("/api/node/enrollments", h.createNodeEnrollment)
		mux.HandleFunc("/api/node/enrollments/token", h.pollNodeEnrollment)
		mux.HandleFunc("/api/node/enrollments/approve", h.approveNodeEnrollment)
		mux.HandleFunc("/api/node/auth/challenge", h.nodeAuthChallenge)
		mux.HandleFunc("/api/node/auth/token", h.nodeAuthToken)
		mux.HandleFunc("/api/node/ws", h.nodeWebSocket)
	}
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		var err error
		if h.ready != nil {
			err = h.ready(r.Context())
		} else {
			err = store.redis.Ping(r.Context()).Err()
			if err == nil && ledger != nil && !ledger.Ready() {
				err = billing.ErrJournalCircuitOpen
			}
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Storage is unavailable.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "mode": mode})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Unknown endpoint.")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if w.Header().Get("X-Request-ID") == "" {
			if id, err := domain.NewID(); err == nil {
				w.Header().Set("X-Request-ID", id.String())
			} else {
				http.Error(w, "request unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !h.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Use the Cordova site to create an account.")
		return
	}
	var input struct {
		Username             string `json:"username"`
		Password             string `json:"password"`
		PrivacyNoticeVersion string `json:"privacy_notice_version"`
	}
	if !decodeBody(w, r, &input) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Registration details are invalid.")
		return
	}
	account, codes, err := h.store.Register(r.Context(), input.Username, input.Password, input.PrivacyNoticeVersion, remoteAddress(r, h.cfg.TrustProxy))
	if err != nil {
		h.writeStoreError(w, err, "Account could not be created.")
		return
	}
	h.revokeCurrent(r)
	session, csrf, expires, err := h.store.CreateSession(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "The account was created but a session could not be started. Sign in to continue.")
		return
	}
	h.setCookie(w, session, expires)
	writeJSON(w, http.StatusCreated, map[string]any{"account": account, "csrf_token": csrf, "expires_at": expires.UTC().Format(time.RFC3339Nano), "recovery_codes": codes})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !h.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Use the Cordova site to sign in.")
		return
	}
	var input struct {
		Username string  `json:"username"`
		Password string  `json:"password"`
		TOTPCode *string `json:"totp_code,omitempty"`
	}
	if !decodeBody(w, r, &input) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Sign-in details are invalid.")
		return
	}
	account, err := h.store.Login(r.Context(), input.Username, input.Password, remoteAddress(r, h.cfg.TrustProxy))
	if err != nil {
		h.writeStoreError(w, err, "Username or password is incorrect.")
		return
	}
	if account.TOTPEnabled {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Username or password is incorrect.")
		return
	}
	h.revokeCurrent(r)
	view := publicAccount(account)
	session, csrf, expires, err := h.store.CreateSession(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "A session could not be started.")
		return
	}
	h.setCookie(w, session, expires)
	writeJSON(w, http.StatusOK, map[string]any{"account": view, "csrf_token": csrf, "expires_at": expires.UTC().Format(time.RFC3339Nano)})
}

func (h *Handler) recover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !h.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Use the Cordova site to recover an account.")
		return
	}
	var input struct {
		Username     string `json:"username"`
		RecoveryCode string `json:"recovery_code"`
		NewPassword  string `json:"new_password"`
	}
	if !decodeBody(w, r, &input) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Recovery details are invalid.")
		return
	}
	account, err := h.store.Recover(r.Context(), input.Username, input.RecoveryCode, input.NewPassword, remoteAddress(r, h.cfg.TrustProxy))
	if err != nil {
		h.writeStoreError(w, err, "Account recovery could not be completed.")
		return
	}
	session, csrf, expires, err := h.store.CreateSession(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "The password changed but a session could not be started. Sign in to continue.")
		return
	}
	h.setCookie(w, session, expires)
	writeJSON(w, http.StatusOK, map[string]any{"account": publicAccount(account), "csrf_token": csrf, "expires_at": expires.UTC().Format(time.RFC3339Nano)})
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	session, account, ok := h.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	csrf := h.store.csrfForCookie(cookie.Value)
	writeJSON(w, http.StatusOK, map[string]any{"account": account, "csrf_token": csrf, "expires_at": time.Unix(session.ExpiresAt, 0).UTC().Format(time.RFC3339Nano)})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	session, _, ok := h.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	if !h.csrf(r, session) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Reload the page and try again.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	if cookie != nil {
		_ = h.store.RevokeSession(r.Context(), cookie.Value)
	}
	h.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) keys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		_, account, ok := h.currentSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		keys, err := h.store.ListKeys(r.Context(), account.ID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "API keys could not be loaded.")
			return
		}
		for i := range keys {
			keys[i].Verifier = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": keys, "next_cursor": nil})
	case http.MethodPost:
		session, account, ok := h.currentSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		if !h.csrf(r, session) {
			writeError(w, http.StatusForbidden, "csrf_failed", "Reload the page and try again.")
			return
		}
		var input struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
			ModelIDs    []string `json:"model_ids"`
			DailyCap    string   `json:"daily_spend_cap_piconero"`
		}
		if !decodeBody(w, r, &input) {
			writeError(w, http.StatusBadRequest, "invalid_request", "API key details are invalid.")
			return
		}
		if account.PrivacyNoticeVersion != h.cfg.PrivacyNoticeVersion {
			writeError(w, http.StatusForbidden, "verification_required", "Accept the current privacy notice before creating an API key.")
			return
		}
		key, secret, err := h.store.CreateAPIKey(r.Context(), account.ID, input.Name, input.Permissions, input.ModelIDs, input.DailyCap)
		if err != nil {
			h.writeStoreError(w, err, "API key could not be created.")
			return
		}
		key.Verifier = ""
		writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	session, account, ok := h.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	if !h.csrf(r, session) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Reload the page and try again.")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "not_found", "API key was not found.")
		return
	}
	if err := h.store.RevokeKey(r.Context(), account.ID, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "API key was not found.")
		} else {
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "API key could not be revoked.")
		}
		return
	}
	if h.requests != nil {
		h.requests.CancelKey(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	accountID := ""
	keyID := ""
	principal, authenticated := h.store.Authenticate(r)
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	if principal.Kind == "key" && !principal.Scopes["usage:read"] {
		writeError(w, http.StatusForbidden, "forbidden", "This credential cannot read usage metadata.")
		return
	}
	accountID = principal.AccountID
	keyID = principal.KeyID
	rows, err := h.store.Usage(r.Context(), accountID, keyID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Usage is not available right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows, "next_cursor": nil})
}

func (h *Handler) billingBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	principal, ok := h.authenticate(r)
	if !ok || principal.Kind != "session" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to view the balance.")
		return
	}
	if h.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Billing is not available right now.")
		return
	}
	balance, err := h.ledger.Balance(r.Context(), principal.AccountID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Balance is not available right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available_piconero": balance.Available, "reserved_piconero": balance.Reserved,
		"earnings_pending_piconero": balance.OwnerPending, "earnings_available_piconero": balance.OwnerAvailable,
		"payout_reserved_piconero": balance.PayoutReserved, "currency": "XMR", "updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

type requestStatusView struct {
	RequestID    string           `json:"request_id"`
	ModelID      string           `json:"model_id"`
	Status       string           `json:"status"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	Charged      *domain.Piconero `json:"charged_piconero"`
	LatencyMS    *int64           `json:"latency_ms"`
	CreatedAt    time.Time        `json:"created_at"`
	FinishedAt   *time.Time       `json:"finished_at"`
}

func (h *Handler) requestStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	accountID, keyID, authStatus := h.requestStatusPrincipal(r)
	if authStatus != 0 {
		code, message := "unauthenticated", "Sign in or use an authorized API key."
		if authStatus == http.StatusForbidden {
			code, message = "forbidden", "This API key cannot read request history."
		}
		writeError(w, authStatus, code, message)
		return
	}
	if h.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Request status is not available right now.")
		return
	}
	requestID := r.PathValue("id")
	checkpoint, _, err := h.ledger.Request(r.Context(), requestID)
	if errors.Is(err, billing.ErrRequestNotFound) || errors.Is(err, billing.ErrInvalidOperation) {
		writeError(w, http.StatusNotFound, "not_found", "Request was not found.")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Request status is not available right now.")
		return
	}
	if checkpoint.AccountID != accountID || keyID != "" && checkpoint.APIKeyID != keyID {
		writeError(w, http.StatusNotFound, "not_found", "Request was not found.")
		return
	}
	status := string(checkpoint.Status)
	recovering := !h.ledger.Ready()
	if recovering {
		status = "recovering"
	}
	charged, _ := domain.NewPiconero(0)
	var outputTokens int64
	var terminalOperation string
	if !recovering {
		for attemptID, attempt := range checkpoint.Attempts {
			switch attempt.Reservation.Status {
			case billing.AttemptSettled:
				quote, quoteErr := billing.CalculateQuote(checkpoint.InputTokens, attempt.OutputTokens, checkpoint.MaxOutputTokens, checkpoint.Price, false)
				if quoteErr != nil {
					writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Request accounting needs reconciliation.")
					return
				}
				charged, outputTokens, terminalOperation = quote.ActualCost, attempt.OutputTokens, "settle:"+requestID
			case billing.AttemptCancelled:
				quote, quoteErr := billing.CalculateQuote(checkpoint.InputTokens, attempt.OutputTokens, checkpoint.MaxOutputTokens, checkpoint.Price, true)
				if quoteErr != nil {
					writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Request accounting needs reconciliation.")
					return
				}
				charged, outputTokens, terminalOperation = quote.ActualCost, attempt.OutputTokens, "cancel:"+requestID
			case billing.AttemptRefunded:
				if attempt.Reservation.OwnerID == checkpoint.OwnerID && checkpoint.Status == billing.RequestRefunded {
					outputTokens = attempt.OutputTokens
					terminalOperation = "refund:" + requestID + ":" + attemptID
				}
			}
		}
	}
	var finishedAt *time.Time
	var latencyMS *int64
	if terminalOperation != "" && !recovering {
		var operation billing.Entry
		var opErr error
		if checkpoint.Status == billing.RequestRefunded {
			operation, opErr = h.ledger.Operation(r.Context(), "abandon:"+requestID)
			if errors.Is(opErr, billing.ErrRequestNotFound) {
				operation, opErr = h.ledger.Operation(r.Context(), terminalOperation)
			}
		} else {
			operation, opErr = h.ledger.Operation(r.Context(), terminalOperation)
		}
		if opErr != nil {
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Request status is not available right now.")
			return
		}
		finished := operation.CreatedAt.UTC()
		finishedAt = &finished
		latency := finished.Sub(checkpoint.AcceptedAt).Milliseconds()
		if latency < 0 {
			latency = 0
		}
		latencyMS = &latency
	}
	var chargedValue *domain.Piconero
	if !recovering {
		chargedValue = &charged
	}
	writeJSON(w, http.StatusOK, requestStatusView{RequestID: requestID, ModelID: checkpoint.ModelID, Status: status,
		InputTokens: checkpoint.InputTokens, OutputTokens: outputTokens, Charged: chargedValue,
		LatencyMS: latencyMS, CreatedAt: checkpoint.AcceptedAt.UTC(), FinishedAt: finishedAt})
}

func (h *Handler) requestStatusPrincipal(r *http.Request) (accountID, keyID string, status int) {
	if h.authenticate == nil {
		return "", "", http.StatusUnauthorized
	}
	principal, authenticated := h.authenticate(r)
	if !authenticated || principal.AccountID == "" {
		return "", "", http.StatusUnauthorized
	}
	switch principal.Kind {
	case "session":
		return principal.AccountID, "", 0
	case "key":
		if !principal.Scopes["usage:read"] {
			return "", "", http.StatusForbidden
		}
		if principal.KeyID == "" {
			return "", "", http.StatusUnauthorized
		}
		return principal.AccountID, principal.KeyID, 0
	default:
		return "", "", http.StatusUnauthorized
	}
}

func (h *Handler) liveTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	session, account, ok := h.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	if !h.csrf(r, session) {
		writeError(w, http.StatusForbidden, "csrf_failed", "Reload the page and try again.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	ticket, err := h.store.CreateLiveTicket(r.Context(), account.ID, cookie.Value)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Live updates are unavailable right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expires_at": time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano)})
}

func (h *Handler) currentSession(r *http.Request) (sessionRecord, Account, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return sessionRecord{}, Account{}, false
	}
	return h.store.Session(r.Context(), cookie.Value)
}
func (h *Handler) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == h.cfg.Origin && validOrigin(h.cfg.Origin)
}
func validOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
func (h *Handler) csrf(r *http.Request, session sessionRecord) bool {
	if !h.sameOrigin(r) {
		return false
	}
	csrf := r.Header.Get("X-CSRF-Token")
	if csrf == "" {
		return false
	}
	return h.store.ValidateCSRF(session, csrf)
}
func hmacEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
func (h *Handler) setCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
}
func (h *Handler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
func (h *Handler) revokeCurrent(r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = h.store.RevokeSession(r.Context(), cookie.Value)
	}
}
func (h *Handler) writeStoreError(w http.ResponseWriter, err error, invalidMessage string) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", invalidMessage)
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "That account already exists.")
	case errors.Is(err, ErrThrottled):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Try again later.")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Username or password is incorrect.")
	default:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "The request could not be completed right now.")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBody))
	if err != nil || !json.Valid(body) || duplicateJSON(body) != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}
func duplicateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("too deep")
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
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
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
		return errors.New("unexpected delimiter")
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		value, err := domain.NewID()
		if err != nil {
			http.Error(w, "request unavailable", http.StatusServiceUnavailable)
			return
		}
		id = value.String()
		w.Header().Set("X-Request-ID", id)
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": errorKind(status), "param": nil, "code": code}, "request_id": id})
}
func errorKind(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "invalid_request_error"
	}
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request", "Use the documented HTTP method.")
}
