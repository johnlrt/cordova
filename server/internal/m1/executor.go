package m1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/request"
)

// AccountedExecutor binds validated API requests to the M3 coordinator. The
// supplied price snapshot is immutable for the logical request and all retry
// attempts.
type AccountedExecutor struct {
	coordinator *request.Coordinator
	price       billing.Price
}

var _ api.CompletionExecutor = (*AccountedExecutor)(nil)

func NewAccountedExecutor(coordinator *request.Coordinator, price billing.Price) (*AccountedExecutor, error) {
	if coordinator == nil || !price.Version.Valid() || price.OwnerShareBPS > 10_000 {
		return nil, errors.New("accounted executor requires a coordinator and valid price snapshot")
	}
	return &AccountedExecutor{coordinator: coordinator, price: price}, nil
}

func (e *AccountedExecutor) Execute(ctx context.Context, input api.CompletionRequest, emit func(string) error) (api.CompletionResult, *api.CompletionFailure) {
	if input.Principal.Kind != "key" || !input.Principal.Scopes["chat:write"] || input.Principal.KeyID == "" {
		return api.CompletionResult{}, failure(http.StatusForbidden, "forbidden", "This API key cannot create chat completions.", 0)
	}
	cap, err := domain.ParsePiconero(input.Principal.DailySpendCapPiconero)
	if err != nil {
		return api.CompletionResult{}, failure(http.StatusUnauthorized, "unauthenticated", "A valid API key is required.", 0)
	}
	result, err := e.coordinator.Execute(ctx, request.Params{
		RequestID: input.ID, AccountID: input.Principal.AccountID, APIKeyID: input.Principal.KeyID,
		ModelID: input.ModelID, DailySpendCap: cap, Price: e.price, AcceptedAt: time.Now().UTC(),
		OnAccepted: input.Accepted,
	}, input.Input, input.Streaming, emit)
	converted := api.CompletionResult{Text: result.Text, Usage: result.Usage, FinishReason: result.FinishReason,
		Status: result.Status, Charged: result.Charged.String()}
	if err != nil {
		return converted, mapCompletionError(err)
	}
	return converted, nil
}

func mapCompletionError(err error) *api.CompletionFailure {
	switch {
	case errors.Is(err, billing.ErrInsufficientFunds):
		return failure(http.StatusTooManyRequests, "insufficient_funds", "The account does not have enough available test credits.", 0)
	case errors.Is(err, billing.ErrSpendCapExceeded):
		return failure(http.StatusTooManyRequests, "spend_cap_exceeded", "The API key daily spend cap would be exceeded.", 0)
	case errors.Is(err, request.ErrTokenizerUnavailable):
		return failure(http.StatusServiceUnavailable, "tokenizer_unavailable", "Authoritative tokenization is unavailable.", 5)
	case errors.Is(err, request.ErrInputLimit):
		return failure(http.StatusBadRequest, "invalid_request", "The request exceeds the admitted model context.", 0)
	case errors.Is(err, request.ErrOutputLimit):
		return failure(http.StatusServiceUnavailable, "node_failed", "The inference attempt exceeded its allowed output.", 2)
	case errors.Is(err, request.ErrNoCapacity):
		return failure(http.StatusServiceUnavailable, "no_capacity", "No eligible inference capacity is available.", 2)
	case errors.Is(err, request.ErrModelWarming):
		return failure(http.StatusServiceUnavailable, "model_warming", "Eligible nodes are preparing the requested model.", 5)
	case errors.Is(err, request.ErrModelNotFound):
		return failure(http.StatusNotFound, "model_not_found", "The requested model is not in the verified catalog.", 0)
	case errors.Is(err, request.ErrNodeFailed):
		return failure(http.StatusServiceUnavailable, "node_failed", "The inference node could not complete the request.", 2)
	case errors.Is(err, request.ErrClientCancelled):
		return failure(http.StatusRequestTimeout, "timeout", "The client cancelled the request.", 0)
	case errors.Is(err, billing.ErrJournalCircuitOpen), errors.Is(err, billing.ErrJournalUncertain):
		return failure(http.StatusServiceUnavailable, "storage_unavailable", "Billing storage is recovering.", 5)
	case errors.Is(err, billing.ErrAlreadyTerminal), errors.Is(err, billing.ErrConflict), errors.Is(err, billing.ErrRetryUnavailable):
		return failure(http.StatusConflict, "conflict", "The request accounting state changed; query request status.", 0)
	default:
		return failure(http.StatusServiceUnavailable, "storage_unavailable", "The request could not be durably accounted.", 5)
	}
}

func failure(status int, code, message string, retryAfter int) *api.CompletionFailure {
	return &api.CompletionFailure{StatusCode: status, Code: code, Message: message, RetryAfterSeconds: retryAfter}
}
