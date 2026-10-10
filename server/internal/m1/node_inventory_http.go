package m1

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func (h *Handler) nodesInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	_, account, ok := h.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}
	for key := range r.URL.Query() {
		if key != "limit" && key != "cursor" {
			writeError(w, http.StatusBadRequest, "invalid_request", "The node list query is invalid.")
			return
		}
	}
	limit := 50
	if values, exists := r.URL.Query()["limit"]; exists {
		parsed, err := strconv.Atoi(strings.Join(values, ","))
		if err != nil || len(values) != 1 || parsed < 1 || parsed > maxNodePageSize {
			writeError(w, http.StatusBadRequest, "invalid_request", "The node list limit is invalid.")
			return
		}
		limit = parsed
	}
	cursor := ""
	if values, exists := r.URL.Query()["cursor"]; exists {
		if len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "The node list cursor is invalid.")
			return
		}
		cursor = values[0]
	}
	page, err := h.nodes.listNodes(r.Context(), account.ID, limit, cursor)
	if err != nil {
		writeNodeInventoryError(w, err, "Node inventory could not be loaded.")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) linkNode(w http.ResponseWriter, r *http.Request) {
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
	var input struct {
		UserCode string `json:"user_code"`
		Name     string `json:"name"`
	}
	if !decodeBody(w, r, &input) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node link request is invalid.")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node name is invalid.")
		return
	}
	record, err := h.nodes.approveEnrollmentWithName(r.Context(), account.ID, input.UserCode, name)
	if err != nil {
		writeNodeInventoryError(w, err, "The node could not be linked.")
		return
	}
	view, err := h.nodes.nodeView(r.Context(), record)
	if err != nil {
		writeNodeInventoryError(w, err, "The linked node could not be loaded.")
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *Handler) nodeDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		_, account, ok := h.currentSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		view, err := h.nodes.getNodeForOwner(r.Context(), account.ID, id)
		if err != nil {
			writeNodeInventoryError(w, err, "The node could not be loaded.")
			return
		}
		writeJSON(w, http.StatusOK, view)
	case http.MethodPatch:
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
			Name string `json:"name"`
		}
		if !decodeBody(w, r, &input) {
			writeError(w, http.StatusBadRequest, "invalid_request", "The node name is invalid.")
			return
		}
		view, err := h.nodes.renameNode(r.Context(), account.ID, id, strings.TrimSpace(input.Name))
		if err != nil {
			writeNodeInventoryError(w, err, "The node name could not be changed.")
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		methodNotAllowed(w, "GET, PATCH")
	}
}

func (h *Handler) revokeNode(w http.ResponseWriter, r *http.Request) {
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
	var input struct{}
	if !decodeBody(w, r, &input) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The node revoke request is invalid.")
		return
	}
	if err := h.nodes.revokeNode(r.Context(), account.ID, r.PathValue("id")); err != nil {
		writeNodeInventoryError(w, err, "The node could not be revoked.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeNodeInventoryError(w http.ResponseWriter, err error, unavailable string) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "The node request is invalid.")
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNodeUnauthorized):
		writeError(w, http.StatusNotFound, "not_found", "The node was not found.")
	case errors.Is(err, ErrNodeExpired):
		writeError(w, http.StatusGone, "not_found", "The enrollment code has expired.")
	case errors.Is(err, ErrNodeConflict):
		writeError(w, http.StatusConflict, "conflict", "The enrollment was already approved or is no longer available.")
	case errors.Is(err, ErrNodeThrottled):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Try again later.")
	default:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", unavailable)
	}
}
