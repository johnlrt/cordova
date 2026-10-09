// Package request coordinates trusted token accounting, routed attempts and
// durable request settlement. It never persists prompt or completion text.
package request

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

var (
	ErrInvalidRequest       = errors.New("invalid request coordinator input")
	ErrTokenizerUnavailable = errors.New("authoritative tokenizer is unavailable")
	ErrInputLimit           = errors.New("authoritative input limit exceeded")
	ErrOutputLimit          = errors.New("authoritative output limit exceeded")
	ErrNoCapacity           = errors.New("no eligible inference capacity")
	ErrNodeFailed           = errors.New("inference attempt failed")
	ErrClientCancelled      = errors.New("client cancelled the request")
)

const (
	maxInputTokens = int64(1_000_000)
	maxOutputBytes = 1 << 20
	maxDeltaBytes  = 16 << 10
	requestTimeout = 120 * time.Second
	cleanupTimeout = 2 * time.Second
)

type Ledger interface {
	Reserve(context.Context, billing.ReserveParams) (billing.Reservation, billing.Entry, error)
	FailAttempt(context.Context, string, string, bool, time.Time) (billing.Entry, error)
	Settle(context.Context, string, string, int64, bool, time.Time) (billing.Entry, error)
	AbandonRetry(context.Context, string, time.Time) (billing.Entry, error)
}

// Tokenizer must use the admitted model's exact template for input and the
// canonical decoded output with special-token parsing disabled for completion.
type Tokenizer interface {
	CountInput(context.Context, string, []inference.Message) (int64, error)
	CountOutput(context.Context, string, string) (int64, error)
}

// Router returns an already claimed, session-bound attempt lease. A retry must
// exclude every owner in excludedOwners and remain within deadline.
type Router interface {
	Acquire(context.Context, string, map[string]struct{}, time.Time) (Attempt, error)
}

type Attempt interface {
	OwnerID() string
	Run(context.Context, inference.Request, func(string) error) (finishReason string, err error)
	Release(context.Context)
}

type Params struct {
	RequestID     string
	AccountID     string
	APIKeyID      string
	ModelID       string
	DailySpendCap domain.Piconero
	Price         billing.Price
	AcceptedAt    time.Time
	Deadline      time.Time
}

type Result struct {
	Text         string
	Usage        inference.Usage
	FinishReason string
	Attempts     int
}

type Coordinator struct {
	ledger    Ledger
	tokenizer Tokenizer
	router    Router
}

func NewCoordinator(ledger Ledger, tokenizer Tokenizer, router Router) (*Coordinator, error) {
	if ledger == nil || tokenizer == nil || router == nil {
		return nil, ErrInvalidRequest
	}
	return &Coordinator{ledger: ledger, tokenizer: tokenizer, router: router}, nil
}

// Execute reserves before dispatch, retries one failed pre-content attempt on
// a distinct owner, and durably settles before returning a successful result.
// In stream mode emit must flush a chunk before returning nil; only flushed
// chunks count as the accepted prefix on client cancellation.
func (c *Coordinator) Execute(ctx context.Context, params Params, input inference.Request, stream bool,
	emit func(string) error) (Result, error) {
	if ctx == nil || !domainID(params.RequestID) || !domainID(params.AccountID) || !domainID(params.APIKeyID) ||
		!validModelID(params.ModelID) || params.Price.OwnerShareBPS > 10_000 || !params.Price.Version.Valid() ||
		input.MaxTokens < 1 || input.MaxTokens > 2048 || len(input.Messages) == 0 || len(input.Messages) > 128 ||
		stream && emit == nil {
		return Result{}, ErrInvalidRequest
	}
	now := time.Now().UTC()
	if params.AcceptedAt.IsZero() {
		params.AcceptedAt = now
	} else {
		params.AcceptedAt = params.AcceptedAt.UTC()
	}
	if params.AcceptedAt.After(now) {
		return Result{}, ErrInvalidRequest
	}
	if params.Deadline.IsZero() {
		params.Deadline = params.AcceptedAt.Add(requestTimeout)
	} else {
		params.Deadline = params.Deadline.UTC()
	}
	if !params.Deadline.After(now) || params.Deadline.After(params.AcceptedAt.Add(requestTimeout)) {
		return Result{}, ErrInvalidRequest
	}
	runCtx, cancel := context.WithDeadline(ctx, params.Deadline)
	defer cancel()

	inputTokens, err := c.tokenizer.CountInput(runCtx, params.ModelID, input.Messages)
	if err != nil {
		return Result{}, ErrTokenizerUnavailable
	}
	if inputTokens < 0 || inputTokens > maxInputTokens {
		return Result{}, ErrInputLimit
	}

	excludedOwners := make(map[string]struct{}, 1)
	requestOpenForRetry := false
	for attemptNumber := 1; attemptNumber <= 2; attemptNumber++ {
		attempt, acquireErr := c.router.Acquire(runCtx, params.ModelID, excludedOwners, params.Deadline)
		if acquireErr != nil {
			if requestOpenForRetry {
				if abandonErr := c.abandonRetry(ctx, params); abandonErr != nil {
					return Result{}, errors.Join(ErrNoCapacity, abandonErr)
				}
			}
			return Result{}, ErrNoCapacity
		}
		if attempt == nil {
			if requestOpenForRetry {
				if err := c.abandonRetry(ctx, params); err != nil {
					return Result{}, err
				}
			}
			return Result{}, ErrNoCapacity
		}
		ownerID := attempt.OwnerID()
		if !domainID(ownerID) {
			release(attempt)
			if requestOpenForRetry {
				_ = c.abandonRetry(ctx, params)
			}
			return Result{}, ErrNoCapacity
		}
		if _, alreadyUsed := excludedOwners[ownerID]; alreadyUsed {
			release(attempt)
			if requestOpenForRetry {
				if err := c.abandonRetry(ctx, params); err != nil {
					return Result{}, err
				}
			}
			return Result{}, ErrNoCapacity
		}
		attemptID, idErr := domain.NewID()
		if idErr != nil {
			release(attempt)
			if requestOpenForRetry {
				_ = c.abandonRetry(ctx, params)
			}
			return Result{}, ErrInvalidRequest
		}
		reserve := billing.ReserveParams{RequestID: params.RequestID, AttemptID: attemptID.String(), ModelID: params.ModelID,
			AccountID: params.AccountID, APIKeyID: params.APIKeyID, OwnerID: ownerID, InputTokens: inputTokens,
			MaxOutputTokens: int64(input.MaxTokens), DailySpendCap: params.DailySpendCap, Price: params.Price, AcceptedAt: params.AcceptedAt}
		if _, _, err := c.ledger.Reserve(runCtx, reserve); err != nil {
			release(attempt)
			if requestOpenForRetry && !errors.Is(err, billing.ErrJournalUncertain) && !errors.Is(err, billing.ErrJournalCircuitOpen) {
				if abandonErr := c.abandonRetry(ctx, params); abandonErr != nil && !errors.Is(abandonErr, billing.ErrAlreadyTerminal) {
					return Result{}, errors.Join(err, abandonErr)
				}
			}
			return Result{}, err
		}
		outcome := c.runAttempt(runCtx, params.ModelID, input, stream, emit, attempt)
		if outcome.err == nil && runCtx.Err() != nil {
			if errors.Is(runCtx.Err(), context.Canceled) {
				outcome.clientCancelled = true
				if !stream {
					outcome.acceptedText = ""
					outcome.acceptedTokens = 0
				}
			} else {
				outcome.err = ErrNodeFailed
			}
		}
		if outcome.clientCancelled {
			settleCtx, settleCancel := financialContext(ctx)
			defer settleCancel()
			if _, err := c.ledger.Settle(settleCtx, params.RequestID, attemptID.String(), outcome.acceptedTokens, true, time.Now().UTC()); err != nil {
				return Result{}, err
			}
			return Result{Text: outcome.acceptedText, Usage: usage(inputTokens, outcome.acceptedTokens),
				FinishReason: "stop", Attempts: attemptNumber}, ErrClientCancelled
		}
		if outcome.err == nil {
			settleCtx, settleCancel := financialContext(ctx)
			_, settleErr := c.ledger.Settle(settleCtx, params.RequestID, attemptID.String(), outcome.outputTokens, false, time.Now().UTC())
			settleCancel()
			if settleErr != nil {
				return Result{}, settleErr
			}
			result := Result{Text: outcome.text, Usage: usage(inputTokens, outcome.outputTokens),
				FinishReason: outcome.finishReason, Attempts: attemptNumber}
			if !stream && emit != nil {
				if err := emit(outcome.text); err != nil {
					return result, err
				}
			}
			return result, nil
		}

		retryAllowed := attemptNumber == 1 && !streamContentDelivered(outcome, stream) &&
			!errors.Is(outcome.err, ErrTokenizerUnavailable) && !errors.Is(outcome.err, ErrOutputLimit) &&
			runCtx.Err() == nil
		financialCtx, financialCancel := financialContext(ctx)
		_, failErr := c.ledger.FailAttempt(financialCtx, params.RequestID, attemptID.String(), retryAllowed, time.Now().UTC())
		financialCancel()
		if failErr != nil {
			return Result{}, failErr
		}
		if !retryAllowed {
			return Result{Attempts: attemptNumber}, outcome.err
		}
		excludedOwners[ownerID] = struct{}{}
		requestOpenForRetry = true
	}
	if requestOpenForRetry {
		if err := c.abandonRetry(ctx, params); err != nil {
			return Result{}, err
		}
	}
	return Result{}, ErrNoCapacity
}

type attemptOutcome struct {
	text            string
	acceptedText    string
	finishReason    string
	outputTokens    int64
	acceptedTokens  int64
	clientCancelled bool
	err             error
}

func (c *Coordinator) runAttempt(ctx context.Context, modelID string, input inference.Request, stream bool,
	emit func(string) error, attempt Attempt) (out attemptOutcome) {
	defer release(attempt)
	var generated strings.Builder
	var accepted strings.Builder
	var callbackErr error
	finishReason, runErr := attempt.Run(ctx, input, func(chunk string) error {
		if callbackErr != nil {
			return callbackErr
		}
		if ctx.Err() != nil {
			out.clientCancelled = errors.Is(ctx.Err(), context.Canceled)
			callbackErr = ctx.Err()
			return callbackErr
		}
		if chunk == "" {
			return nil
		}
		if len(chunk) > maxDeltaBytes || !utf8.ValidString(chunk) || generated.Len()+len(chunk) > maxOutputBytes {
			callbackErr = ErrOutputLimit
			return callbackErr
		}
		generated.WriteString(chunk)
		tokens, err := c.tokenizer.CountOutput(ctx, modelID, generated.String())
		if err != nil {
			callbackErr = ErrTokenizerUnavailable
			return callbackErr
		}
		if tokens < 0 || tokens > int64(input.MaxTokens) {
			callbackErr = ErrOutputLimit
			return callbackErr
		}
		out.outputTokens = tokens
		if stream {
			if err := emit(chunk); err != nil {
				callbackErr = ErrClientCancelled
				out.clientCancelled = true
				return callbackErr
			}
			accepted.WriteString(chunk)
			out.acceptedTokens = tokens
			out.acceptedText = accepted.String()
		}
		return nil
	})
	if callbackErr != nil {
		if errors.Is(callbackErr, ErrClientCancelled) || errors.Is(callbackErr, context.Canceled) {
			out.clientCancelled = true
			if stream {
				// The last successful flush is the only prefix accepted for billing.
				out.acceptedText = accepted.String()
			} else {
				out.acceptedTokens = 0
				out.acceptedText = ""
			}
			return out
		}
		out.err = callbackErr
		return out
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			out.clientCancelled = true
			if stream {
				out.acceptedText = accepted.String()
			} else {
				out.acceptedText = ""
				out.acceptedTokens = 0
			}
			return out
		}
		out.err = ErrNodeFailed
		return out
	}
	if runErr != nil {
		out.err = ErrNodeFailed
		return out
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if finishReason != "stop" && finishReason != "length" {
		out.err = ErrNodeFailed
		return out
	}
	out.finishReason = finishReason
	text := generated.String()
	tokens, err := c.tokenizer.CountOutput(ctx, modelID, text)
	if err != nil {
		out.err = ErrTokenizerUnavailable
		return out
	}
	if tokens < 0 || tokens > int64(input.MaxTokens) {
		out.err = ErrOutputLimit
		return out
	}
	out.outputTokens = tokens
	out.text = text
	if stream {
		out.acceptedText = accepted.String()
		out.acceptedTokens = tokens
	}
	return out
}

func (c *Coordinator) abandonRetry(ctx context.Context, params Params) error {
	financialCtx, cancel := financialContext(ctx)
	defer cancel()
	_, err := c.ledger.AbandonRetry(financialCtx, params.RequestID, time.Now().UTC())
	return err
}

func financialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func release(attempt Attempt) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	attempt.Release(ctx)
}

func streamContentDelivered(out attemptOutcome, stream bool) bool {
	return stream && out.acceptedText != ""
}

func usage(input, output int64) inference.Usage {
	return inference.Usage{PromptTokens: int(input), CompletionTokens: int(output), TotalTokens: int(input + output)}
}

func domainID(value string) bool {
	_, err := domain.ParseID(value)
	return err == nil
}

func validModelID(value string) bool {
	return value != "" && len(value) <= 64 && strings.TrimSpace(value) == value
}
