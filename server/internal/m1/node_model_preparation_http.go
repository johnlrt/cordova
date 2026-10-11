package m1

import (
	"errors"
	"net/http"
)

type nodeModelPreparationResponse struct {
	ModelID string                     `json:"model_id"`
	Status  NodeModelPreparationStatus `json:"status"`
}

func (h *Handler) prepareNodeModel(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusBadRequest, "invalid_request", "The model preparation request is invalid.")
		return
	}
	nodeID, modelID := r.PathValue("id"), r.PathValue("model_id")
	if !validModelSlug(modelID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The model identifier is invalid.")
		return
	}
	if _, err := h.nodes.getNodeForOwner(r.Context(), account.ID, nodeID); err != nil {
		writeNodeInventoryError(w, err, "The node could not be loaded.")
		return
	}
	if h.modelPreparer == nil {
		writeNodePreparationUnavailable(w)
		return
	}
	status, err := h.modelPreparer.PrepareNodeModel(r.Context(), nodeID, modelID)
	if err != nil {
		writeNodePreparationError(w, err)
		return
	}
	response := nodeModelPreparationResponse{ModelID: modelID, Status: status}
	if status == NodeModelPreparationReady {
		writeJSON(w, http.StatusOK, response)
		return
	}
	if status != NodeModelPreparationScheduled && status != NodeModelPreparationBenchmarking {
		writeNodePreparationUnavailable(w)
		return
	}
	writeJSON(w, http.StatusAccepted, response)
}

func writeNodePreparationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "The model preparation request is invalid.")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "model_not_found", "The model is not in the signed catalog.")
	case errors.Is(err, ErrNodePreparationActive), errors.Is(err, ErrNodeBenchmarkActive):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "model_warming", "The node is already preparing a model.")
	case errors.Is(err, ErrNodeSessionUnavailable), errors.Is(err, ErrNodeNoCapacity),
		errors.Is(err, ErrNodeModelPreparationUnavailable):
		writeNodePreparationUnavailable(w)
	default:
		writeNodePreparationUnavailable(w)
	}
}

func writeNodePreparationUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	writeError(w, http.StatusServiceUnavailable, "no_capacity", "The node cannot prepare this model right now.")
}
