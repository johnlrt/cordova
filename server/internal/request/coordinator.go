// Package request coordinates trusted token accounting, routed attempts and
// durable request settlement. It never persists prompt or completion text.
package request

import (
	"context"
	"errors"
	"log"
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
	ErrModelWarming         = errors.New("model is warming on eligible nodes")
	ErrModelNotFound        = errors.New("model is not in the verified catalog")
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

// InputPreparer returns the exact IDs used for routing when the authoritative
// tokenizer supports the C9 prepare operation. Legacy mock tokenizers may only
// implement Tokenizer and remain limited to non-billable test traffic.
type InputPreparer interface {
	PrepareInput(context.Context, string, []inference.Message) (inference.PreparedInput, error)
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

// AcceptedAttempt reports the remote node's inference.accepted acknowledgement
// before any content can be forwarded to a streaming client.
type AcceptedAttempt interface {
	RunAccepted(context.Context, inference.Request, func() error, func(string) error) (finishReason string, err error)
}

// OutcomeReporter accepts server-measured, content-free attempt metadata for
// durable routing/fraud projections. Implementations may not receive text.
type OutcomeReporter interface {
	RecordOutcome(context.Context, OutcomeReport) error
}

type OutcomeReport struct {
	RequestID            string
	AttemptID            string
	ModelID              string
	Outcome              string
	ElapsedMS            uint32
	FirstTokenMS         *uint32
	VerifiedOutputTokens uint32
	ClaimDeviation       bool
}

type OutputClaimAuditor interface {
	OutputClaims() (inputTokens, outputTokens *uint32)
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
	OnAccepted    func() error
}

type Result struct {
	Text         string
	Usage        inference.Usage
	FinishReason string
	Attempts     int
	Status       string
	Charged      domain.Piconero
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

	var inputTokens int64
	var err error
	if preparer, ok := c.tokenizer.(InputPreparer); ok {
		prepared, err := preparer.PrepareInput(runCtx, params.ModelID, input.Messages)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return Result{}, ErrClientCancelled
			}
			if errors.Is(err, inference.ErrContextExceeded) {
				return Result{}, ErrInputLimit
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return Result{}, ErrTokenizerUnavailable
			}
			return Result{}, ErrTokenizerUnavailable
		}
		inputTokens = prepared.TokenCount
		if inputTokens < 1 || inputTokens > maxInputTokens || len(prepared.TokenIDs) != int(inputTokens) ||
			prepared.ContextTokens == 0 || prepared.ContextTokens > 32_768 || !domainID(prepared.CatalogVersion) {
			return Result{}, ErrTokenizerUnavailable
		}
		for _, tokenID := range prepared.TokenIDs {
			if tokenID > 2_147_483_647 {
				return Result{}, ErrTokenizerUnavailable
			}
		}
		if uint64(inputTokens)+uint64(input.MaxTokens) > uint64(prepared.ContextTokens) {
			return Result{}, ErrInputLimit
		}
		input.InputTokenIDs = append([]uint32(nil), prepared.TokenIDs...)
		input.InputTokens = inputTokens
		input.CatalogVersion = prepared.CatalogVersion
	} else {
		inputTokens, err = c.tokenizer.CountInput(runCtx, params.ModelID, input.Messages)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return Result{}, ErrClientCancelled
			}
			return Result{}, ErrTokenizerUnavailable
		}
		if inputTokens < 0 || inputTokens > maxInputTokens {
			return Result{}, ErrInputLimit
		}
	}

	excludedOwners := make(map[string]struct{}, 1)
	requestOpenForRetry := false
	for attemptNumber := 1; attemptNumber <= 2; attemptNumber++ {
		attempt, acquireErr := c.router.Acquire(runCtx, params.ModelID, excludedOwners, params.Deadline)
		if acquireErr != nil {
			routeErr := normalizeRouteError(acquireErr)
			if requestOpenForRetry {
				if abandonErr := c.abandonRetry(ctx, params); abandonErr != nil {
					return Result{Status: "recovering"}, errors.Join(routeErr, abandonErr)
				}
				return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, routeErr
			}
			return Result{}, routeErr
		}
		if attempt == nil {
			if requestOpenForRetry {
				if err := c.abandonRetry(ctx, params); err != nil {
					return Result{Status: "recovering"}, err
				}
				return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, ErrNoCapacity
			}
			return Result{}, ErrNoCapacity
		}
		ownerID := attempt.OwnerID()
		if !domainID(ownerID) {
			release(attempt)
			if requestOpenForRetry {
				if err := c.abandonRetry(ctx, params); err != nil {
					return Result{Status: "recovering"}, errors.Join(ErrNoCapacity, err)
				}
				return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, ErrNoCapacity
			}
			return Result{}, ErrNoCapacity
		}
		if _, alreadyUsed := excludedOwners[ownerID]; alreadyUsed {
			release(attempt)
			if requestOpenForRetry {
				if err := c.abandonRetry(ctx, params); err != nil {
					return Result{Status: "recovering"}, err
				}
				return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, ErrNoCapacity
			}
			return Result{}, ErrNoCapacity
		}
		attemptID, idErr := domain.NewID()
		if idErr != nil {
			release(attempt)
			if requestOpenForRetry {
				if abandonErr := c.abandonRetry(ctx, params); abandonErr != nil {
					return Result{Status: "recovering"}, errors.Join(ErrInvalidRequest, abandonErr)
				}
				return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, ErrInvalidRequest
			}
			return Result{}, ErrInvalidRequest
		}
		reserve := billing.ReserveParams{RequestID: params.RequestID, AttemptID: attemptID.String(), ModelID: params.ModelID,
			AccountID: params.AccountID, APIKeyID: params.APIKeyID, OwnerID: ownerID, InputTokens: inputTokens,
			MaxOutputTokens: int64(input.MaxTokens), DailySpendCap: params.DailySpendCap, Price: params.Price, AcceptedAt: params.AcceptedAt}
		if _, _, err := c.ledger.Reserve(runCtx, reserve); err != nil {
			release(attempt)
			if errors.Is(err, billing.ErrJournalUncertain) || errors.Is(err, billing.ErrJournalCircuitOpen) {
				return Result{Status: "recovering"}, err
			}
			if requestOpenForRetry {
				if abandonErr := c.abandonRetry(ctx, params); abandonErr != nil && !errors.Is(abandonErr, billing.ErrAlreadyTerminal) {
					return Result{Status: "recovering"}, errors.Join(err, abandonErr)
				}
				return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, err
			}
			return Result{}, err
		}
		outcome := c.runAttempt(runCtx, params.ModelID, input, stream, emit, params.OnAccepted, attempt)
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
		if reporter, ok := attempt.(OutcomeReporter); ok {
			reportOutcome := "failure"
			verifiedOutputTokens := outcome.outputTokens
			if outcome.clientCancelled {
				reportOutcome = "cancelled"
				verifiedOutputTokens = outcome.acceptedTokens
			} else if outcome.err == nil {
				reportOutcome = "success"
			}
			claimDeviation := false
			if auditor, ok := attempt.(OutputClaimAuditor); ok {
				claimedInput, claimedOutput := auditor.OutputClaims()
				claimDeviation = claimedInput != nil && uint64(*claimedInput) != uint64(inputTokens) ||
					claimedOutput != nil && uint64(*claimedOutput) != uint64(verifiedOutputTokens)
			}
			var verified uint32
			if verifiedOutputTokens > 0 {
				if verifiedOutputTokens > int64(^uint32(0)) {
					verified = ^uint32(0)
				} else {
					verified = uint32(verifiedOutputTokens)
				}
			}
			reportCtx, reportCancel := context.WithTimeout(context.Background(), 2*time.Second)
			reportErr := reporter.RecordOutcome(reportCtx, OutcomeReport{
				RequestID: params.RequestID, AttemptID: attemptID.String(), ModelID: params.ModelID,
				Outcome: reportOutcome, ElapsedMS: outcome.elapsedMS, FirstTokenMS: outcome.firstTokenMS,
				VerifiedOutputTokens: verified, ClaimDeviation: claimDeviation,
			})
			reportCancel()
			if reportErr != nil {
				// Ranking metadata never changes settlement semantics; a Redis outage
				// will also prevent financial persistence and be surfaced there.
				log.Printf("node outcome projection append failed for attempt %s", attemptID.String())
			}
		}
		if outcome.clientCancelled {
			settleCtx, settleCancel := financialContext(ctx)
			_, settleErr := c.ledger.Settle(settleCtx, params.RequestID, attemptID.String(), outcome.acceptedTokens, true, time.Now().UTC())
			settleCancel()
			if settleErr != nil {
				return Result{Status: "recovering"}, settleErr
			}
			quote, quoteErr := billing.CalculateQuote(inputTokens, outcome.acceptedTokens, int64(input.MaxTokens), params.Price, true)
			if quoteErr != nil {
				return Result{Status: "recovering"}, quoteErr
			}
			return Result{Text: outcome.acceptedText, Usage: usage(inputTokens, outcome.acceptedTokens),
				FinishReason: "stop", Attempts: attemptNumber, Status: string(billing.RequestCancelled), Charged: quote.ActualCost}, ErrClientCancelled
		}
		if outcome.err == nil {
			settleCtx, settleCancel := financialContext(ctx)
			_, settleErr := c.ledger.Settle(settleCtx, params.RequestID, attemptID.String(), outcome.outputTokens, false, time.Now().UTC())
			settleCancel()
			if settleErr != nil {
				return Result{Status: "recovering"}, settleErr
			}
			quote, quoteErr := billing.CalculateQuote(inputTokens, outcome.outputTokens, int64(input.MaxTokens), params.Price, false)
			if quoteErr != nil {
				return Result{Status: "recovering"}, quoteErr
			}
			result := Result{Text: outcome.text, Usage: usage(inputTokens, outcome.outputTokens),
				FinishReason: outcome.finishReason, Attempts: attemptNumber, Status: string(billing.RequestSettled), Charged: quote.ActualCost}
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
			return Result{Status: "recovering"}, failErr
		}
		if !retryAllowed {
			return Result{Attempts: attemptNumber, Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, outcome.err
		}
		excludedOwners[ownerID] = struct{}{}
		requestOpenForRetry = true
	}
	if requestOpenForRetry {
		if err := c.abandonRetry(ctx, params); err != nil {
			return Result{Status: "recovering"}, err
		}
		return Result{Usage: usage(inputTokens, 0), Status: string(billing.RequestRefunded)}, ErrNoCapacity
	}
	return Result{}, ErrNoCapacity
}

func normalizeRouteError(err error) error {
	switch {
	case errors.Is(err, ErrModelWarming):
		return ErrModelWarming
	case errors.Is(err, ErrModelNotFound):
		return ErrModelNotFound
	default:
		return ErrNoCapacity
	}
}

type attemptOutcome struct {
	text            string
	acceptedText    string
	finishReason    string
	outputTokens    int64
	acceptedTokens  int64
	clientCancelled bool
	elapsedMS       uint32
	firstTokenMS    *uint32
	err             error
}

func (c *Coordinator) runAttempt(ctx context.Context, modelID string, input inference.Request, stream bool,
	emit func(string) error, onAccepted func() error, attempt Attempt) (out attemptOutcome) {
	startedAt := time.Now()
	defer func() {
		elapsed := time.Since(startedAt).Milliseconds()
		if elapsed < 1 {
			elapsed = 1
		}
		if elapsed > 120_000 {
			elapsed = 120_000
		}
		out.elapsedMS = uint32(elapsed)
	}()
	defer release(attempt)
	var generated strings.Builder
	var acceptedText strings.Builder
	var callbackErr error
	forward := func(chunk string) error {
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
		if out.firstTokenMS == nil {
			firstToken := time.Since(startedAt).Milliseconds()
			if firstToken < 0 {
				firstToken = 0
			}
			if firstToken > 120_000 {
				firstToken = 120_000
			}
			firstTokenMS := uint32(firstToken)
			out.firstTokenMS = &firstTokenMS
		}
		if len(chunk) > maxDeltaBytes || !utf8.ValidString(chunk) || generated.Len()+len(chunk) > maxOutputBytes {
			callbackErr = ErrOutputLimit
			return callbackErr
		}
		generated.WriteString(chunk)
		tokens, err := c.tokenizer.CountOutput(ctx, modelID, generated.String())
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				callbackErr = context.Canceled
				out.clientCancelled = true
			} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				callbackErr = ErrNodeFailed
			} else {
				callbackErr = ErrTokenizerUnavailable
			}
			return callbackErr
		}
		if tokens < 0 || tokens > int64(input.MaxTokens) {
			callbackErr = ErrOutputLimit
			return callbackErr
		}
		out.outputTokens = tokens
		if stream {
			if err := emit(chunk); err != nil {
				if errors.Is(err, ErrNodeFailed) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
					callbackErr = err
				} else {
					callbackErr = ErrClientCancelled
					out.clientCancelled = true
				}
				return callbackErr
			}
			acceptedText.WriteString(chunk)
			out.acceptedTokens = tokens
			out.acceptedText = acceptedText.String()
		}
		return nil
	}
	var finishReason string
	var runErr error
	if sessionAttempt, ok := attempt.(AcceptedAttempt); ok {
		finishReason, runErr = sessionAttempt.RunAccepted(ctx, input, onAccepted, forward)
	} else {
		if onAccepted != nil {
			if err := onAccepted(); err != nil {
				out.clientCancelled = true
				return out
			}
		}
		finishReason, runErr = attempt.Run(ctx, input, forward)
	}
	if callbackErr != nil {
		if errors.Is(callbackErr, ErrClientCancelled) || errors.Is(callbackErr, context.Canceled) {
			out.clientCancelled = true
			if stream {
				// The last successful flush is the only prefix accepted for billing.
				out.acceptedText = acceptedText.String()
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
				out.acceptedText = acceptedText.String()
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
		if errors.Is(ctx.Err(), context.Canceled) {
			out.clientCancelled = true
			if !stream {
				out.acceptedText = ""
				out.acceptedTokens = 0
			}
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			out.err = ErrNodeFailed
		} else {
			out.err = ErrTokenizerUnavailable
		}
		return out
	}
	if tokens < 0 || tokens > int64(input.MaxTokens) {
		out.err = ErrOutputLimit
		return out
	}
	out.outputTokens = tokens
	out.text = text
	if stream {
		out.acceptedText = acceptedText.String()
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
