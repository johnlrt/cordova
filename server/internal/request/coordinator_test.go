package request

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

type referenceLedger struct{ ledger *billing.Ledger }

func (r referenceLedger) Reserve(_ context.Context, p billing.ReserveParams) (billing.Reservation, billing.Entry, error) {
	return r.ledger.Reserve(p)
}
func (r referenceLedger) FailAttempt(_ context.Context, requestID, attemptID string, retry bool, at time.Time) (billing.Entry, error) {
	return r.ledger.FailAttempt(requestID, attemptID, retry, at)
}
func (r referenceLedger) Settle(_ context.Context, requestID, attemptID string, tokens int64, cancelled bool, at time.Time) (billing.Entry, error) {
	return r.ledger.Settle(requestID, attemptID, tokens, cancelled, at)
}
func (r referenceLedger) AbandonRetry(_ context.Context, requestID string, at time.Time) (billing.Entry, error) {
	return r.ledger.AbandonRetry(requestID, at)
}

type uncertainReserveLedger struct{ Ledger }

func (uncertainReserveLedger) Reserve(context.Context, billing.ReserveParams) (billing.Reservation, billing.Entry, error) {
	return billing.Reservation{}, billing.Entry{}, billing.ErrJournalUncertain
}

type wordTokenizer struct {
	inputErr  error
	outputErr error
}

type preparedTokenizer struct {
	wordTokenizer
	prepared inference.PreparedInput
	err      error
}

func (t preparedTokenizer) PrepareInput(context.Context, string, []inference.Message) (inference.PreparedInput, error) {
	return t.prepared, t.err
}

func (t wordTokenizer) CountInput(_ context.Context, _ string, messages []inference.Message) (int64, error) {
	if t.inputErr != nil {
		return 0, t.inputErr
	}
	var count int64
	for _, message := range messages {
		count += int64(len(strings.Fields(message.Content)))
	}
	return count, nil
}
func (t wordTokenizer) CountOutput(_ context.Context, _ string, text string) (int64, error) {
	if t.outputErr != nil {
		return 0, t.outputErr
	}
	return int64(len(strings.Fields(text))), nil
}

type mockRouter struct {
	attempts []*mockAttempt
	acquired int
	lastExcl map[string]struct{}
	routeErr []error
}

func (r *mockRouter) Acquire(_ context.Context, _ string, excluded map[string]struct{}, _ time.Time) (Attempt, error) {
	r.lastExcl = excluded
	if len(r.routeErr) > 0 {
		err := r.routeErr[0]
		r.routeErr = r.routeErr[1:]
		if err != nil {
			return nil, err
		}
	}
	for len(r.attempts) > 0 {
		attempt := r.attempts[0]
		r.attempts = r.attempts[1:]
		if _, blocked := excluded[attempt.owner]; blocked {
			continue
		}
		r.acquired++
		return attempt, nil
	}
	return nil, ErrNoCapacity
}

type mockAttempt struct {
	owner         string
	chunks        []string
	finish        string
	runErr        error
	released      bool
	receivedInput inference.Request
}

type reportingMockAttempt struct {
	*mockAttempt
	inputClaim  *uint32
	outputClaim *uint32
	report      OutcomeReport
}

type outcomeTestRouter struct{ attempt Attempt }

func (r outcomeTestRouter) Acquire(context.Context, string, map[string]struct{}, time.Time) (Attempt, error) {
	return r.attempt, nil
}

func (a *reportingMockAttempt) RecordOutcome(_ context.Context, report OutcomeReport) error {
	a.report = report
	return nil
}

func (a *reportingMockAttempt) OutputClaims() (*uint32, *uint32) {
	return a.inputClaim, a.outputClaim
}

func (a *mockAttempt) OwnerID() string { return a.owner }
func (a *mockAttempt) Run(_ context.Context, input inference.Request, emit func(string) error) (string, error) {
	a.receivedInput = input
	for _, chunk := range a.chunks {
		if err := emit(chunk); err != nil {
			return "", err
		}
	}
	if a.runErr != nil {
		return "", a.runErr
	}
	if a.finish == "" {
		a.finish = "stop"
	}
	return a.finish, nil
}
func (a *mockAttempt) Release(context.Context) { a.released = true }

func TestCoordinatorSettlesNonStreamedOutputBeforeReturningAndStoresNoText(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "alpha beta")
	attempt := &mockAttempt{owner: owner, chunks: []string{"alpha ", "beta"}, finish: "length"}
	coordinator.router = &mockRouter{attempts: []*mockAttempt{attempt}}
	result, err := coordinator.Execute(context.Background(), params, input, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "alpha beta" || result.FinishReason != "length" || result.Usage.PromptTokens != 2 ||
		result.Usage.CompletionTokens != 2 || result.Attempts != 1 || !attempt.released {
		t.Fatalf("unexpected successful result: %+v released=%v", result, attempt.released)
	}
	accountBalance, err := ledger.Balance(params.AccountID)
	if err != nil || accountBalance.Available.Int64() != 994 || accountBalance.Reserved.Int64() != 0 {
		t.Fatalf("request was not settled before return: balance=%+v err=%v", accountBalance, err)
	}
	encoded, err := json.Marshal(ledger.Snapshot())
	if err != nil || strings.Contains(string(encoded), "alpha") || strings.Contains(string(encoded), "beta") {
		t.Fatalf("request content was persisted: err=%v", err)
	}
}

func TestCoordinatorReportsOnlyServerMeasuredOutcomeMetadata(t *testing.T) {
	_, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "trusted input")
	inputClaim, outputClaim := uint32(2), uint32(2)
	attempt := &reportingMockAttempt{
		mockAttempt: &mockAttempt{owner: owner, chunks: []string{"server ", "counted"}},
		inputClaim:  &inputClaim, outputClaim: &outputClaim,
	}
	coordinator.router = outcomeTestRouter{attempt: attempt}
	_, err := coordinator.Execute(context.Background(), params, input, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.report.RequestID != params.RequestID || attempt.report.ModelID != params.ModelID ||
		attempt.report.Outcome != "success" || attempt.report.ElapsedMS == 0 || attempt.report.FirstTokenMS == nil ||
		attempt.report.VerifiedOutputTokens != 2 || attempt.report.ClaimDeviation {
		t.Fatalf("unexpected node outcome report: %+v", attempt.report)
	}
	if strings.Contains(string(mustMarshal(t, attempt.report)), "server") || strings.Contains(string(mustMarshal(t, attempt.report)), "trusted input") {
		t.Fatal("node outcome report contains request or response content")
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestCoordinatorRetriesOnePreContentFailureOnDistinctOwner(t *testing.T) {
	ledger, coordinator, params, input, ownerOne := setupCoordinator(t, wordTokenizer{}, "private text")
	ownerTwo := newID(t)
	first := &mockAttempt{owner: ownerOne, runErr: errors.New("node disconnected")}
	second := &mockAttempt{owner: ownerTwo, chunks: []string{"ok"}}
	router := &mockRouter{attempts: []*mockAttempt{first, second}}
	coordinator.router = router
	result, err := coordinator.Execute(context.Background(), params, input, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempts != 2 || result.Text != "ok" || !first.released || !second.released || router.acquired != 2 {
		t.Fatalf("retry result=%+v acquired=%d released=%v/%v", result, router.acquired, first.released, second.released)
	}
	if _, reused := router.lastExcl[ownerOne]; !reused {
		t.Fatalf("retry did not fence failed owner: excluded=%v", router.lastExcl)
	}
	checkpoint := ledger.Snapshot().Requests[params.RequestID]
	if checkpoint.Status != billing.RequestSettled || len(checkpoint.Attempts) != 2 {
		t.Fatalf("retry lifecycle ended in an invalid state: %+v", checkpoint)
	}
}

func TestCoordinatorClientCancelSettlesOnlySuccessfullyFlushedPrefix(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "hello world")
	attempt := &mockAttempt{owner: owner, chunks: []string{"one ", "two"}}
	coordinator.router = &mockRouter{attempts: []*mockAttempt{attempt}}
	flushed := 0
	result, err := coordinator.Execute(context.Background(), params, input, true, func(string) error {
		flushed++
		if flushed == 2 {
			return errors.New("client disconnected")
		}
		return nil
	})
	if !errors.Is(err, ErrClientCancelled) || result.Text != "one " || result.Usage.CompletionTokens != 1 || !attempt.released {
		t.Fatalf("cancel result=%+v err=%v released=%v", result, err, attempt.released)
	}
	checkpoint := ledger.Snapshot().Requests[params.RequestID]
	if checkpoint.Status != billing.RequestCancelled {
		t.Fatalf("cancelled request status=%s", checkpoint.Status)
	}
	var cancelledAttempt billing.AttemptCheckpoint
	for _, value := range checkpoint.Attempts {
		cancelledAttempt = value
	}
	if cancelledAttempt.OutputTokens != 1 || cancelledAttempt.Reservation.Status != billing.AttemptCancelled {
		t.Fatalf("cancel settled unaccepted output: %+v", cancelledAttempt)
	}
}

func TestCoordinatorAbandonsRetryWindowWhenNoSecondOwnerExists(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "hello world")
	first := &mockAttempt{owner: owner, runErr: errors.New("node disconnected")}
	coordinator.router = &mockRouter{attempts: []*mockAttempt{first}}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected no capacity after retry refund, got %v", err)
	}
	checkpoint := ledger.Snapshot().Requests[params.RequestID]
	if checkpoint.Status != billing.RequestRefunded {
		t.Fatalf("unavailable retry window remained open: %s", checkpoint.Status)
	}
	if _, ok := ledger.Snapshot().Fingerprints["abandon:"+params.RequestID]; !ok {
		t.Fatal("request abandonment was not journaled")
	}
}

func TestCoordinatorPreservesModelRoutingErrorsBeforeReservation(t *testing.T) {
	for _, routeErr := range []error{ErrModelWarming, ErrModelNotFound} {
		t.Run(routeErr.Error(), func(t *testing.T) {
			ledger, coordinator, params, input, _ := setupCoordinator(t, wordTokenizer{}, "hello world")
			coordinator.router = &mockRouter{routeErr: []error{routeErr}}
			if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, routeErr) {
				t.Fatalf("Execute() error = %v, want %v", err, routeErr)
			}
			if entries := ledger.Entries(); len(entries) != 1 {
				t.Fatalf("routing error reserved funds: entries=%d", len(entries))
			}
		})
	}
}

func TestCoordinatorRefundsRetryReservationWhenSecondRouteIsWarming(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "hello world")
	first := &mockAttempt{owner: owner, runErr: errors.New("node disconnected")}
	coordinator.router = &mockRouter{attempts: []*mockAttempt{first}, routeErr: []error{nil, ErrModelWarming}}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, ErrModelWarming) {
		t.Fatalf("Execute() error = %v, want model warming", err)
	}
	checkpoint := ledger.Snapshot().Requests[params.RequestID]
	if checkpoint.Status != billing.RequestRefunded {
		t.Fatalf("retry reservation remained open after route warming: %s", checkpoint.Status)
	}
}

func TestCoordinatorTokenizerFailureDoesNotReserveOrRetry(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{inputErr: errors.New("helper unavailable")}, "hello world")
	coordinator.router = &mockRouter{attempts: []*mockAttempt{{owner: owner}}}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, ErrTokenizerUnavailable) {
		t.Fatalf("tokenizer failure=%v", err)
	}
	if len(ledger.Entries()) != 1 {
		t.Fatalf("tokenizer outage wrote financial records: entries=%d", len(ledger.Entries()))
	}
}

func TestCoordinatorPassesExactPreparedIDsToAttemptAndEnforcesContext(t *testing.T) {
	prepared := inference.PreparedInput{
		TokenIDs: []uint32{17, 29}, TokenCount: 2, ContextTokens: 8,
		CatalogVersion: "00000000-0000-4000-8000-000000000001",
	}
	attempt := &mockAttempt{owner: newID(t), chunks: []string{"ok"}}
	_, coordinator, params, input, _ := setupCoordinator(t, preparedTokenizer{prepared: prepared}, "hello world")
	coordinator.router = &mockRouter{attempts: []*mockAttempt{attempt}}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); err != nil {
		t.Fatal(err)
	}
	if attempt.receivedInput.CatalogVersion != prepared.CatalogVersion || attempt.receivedInput.InputTokens != prepared.TokenCount ||
		len(attempt.receivedInput.InputTokenIDs) != 2 || attempt.receivedInput.InputTokenIDs[0] != 17 || attempt.receivedInput.InputTokenIDs[1] != 29 {
		t.Fatalf("attempt did not receive exact prepared prompt: %+v", attempt.receivedInput)
	}

	prepared.ContextTokens = 3
	ledger, coordinator, params, input, owner := setupCoordinator(t, preparedTokenizer{prepared: prepared}, "hello world")
	coordinator.router = &mockRouter{attempts: []*mockAttempt{{owner: owner}}}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("context overflow error=%v", err)
	}
	if len(ledger.Entries()) != 1 {
		t.Fatalf("context overflow reserved funds: entries=%d", len(ledger.Entries()))
	}
}

func TestCoordinatorMapsTokenizerContextErrorBeforeReservation(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t,
		preparedTokenizer{err: inference.ErrContextExceeded}, "hello world")
	coordinator.router = &mockRouter{attempts: []*mockAttempt{{owner: owner}}}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("context error=%v, want input limit", err)
	}
	if len(ledger.Entries()) != 1 {
		t.Fatalf("context error reserved funds: entries=%d", len(ledger.Entries()))
	}
}

func TestCoordinatorKeepsUncertainFirstReserveRecovering(t *testing.T) {
	_, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "hello world")
	coordinator.ledger = uncertainReserveLedger{Ledger: coordinator.ledger}
	coordinator.router = &mockRouter{attempts: []*mockAttempt{{owner: owner}}}
	result, err := coordinator.Execute(context.Background(), params, input, false, nil)
	if !errors.Is(err, billing.ErrJournalUncertain) || result.Status != "recovering" {
		t.Fatalf("uncertain reserve result=%+v err=%v", result, err)
	}
}

func TestCoordinatorDoesNotRetryAfterStreamingContentWasFlushed(t *testing.T) {
	ledger, coordinator, params, input, ownerOne := setupCoordinator(t, wordTokenizer{}, "hello world")
	ownerTwo := newID(t)
	first := &mockAttempt{owner: ownerOne, chunks: []string{"partial "}, runErr: errors.New("node disconnected")}
	second := &mockAttempt{owner: ownerTwo, chunks: []string{"must not run"}}
	router := &mockRouter{attempts: []*mockAttempt{first, second}}
	coordinator.router = router
	var delivered strings.Builder
	result, err := coordinator.Execute(context.Background(), params, input, true, func(chunk string) error {
		delivered.WriteString(chunk)
		return nil
	})
	if !errors.Is(err, ErrNodeFailed) || delivered.String() != "partial " || result.Attempts != 1 || router.acquired != 1 {
		t.Fatalf("stream failure retried or hid accepted prefix: result=%+v delivered=%q err=%v acquired=%d", result, delivered.String(), err, router.acquired)
	}
	if checkpoint := ledger.Snapshot().Requests[params.RequestID]; checkpoint.Status != billing.RequestRefunded {
		t.Fatalf("failed streamed attempt was not refunded: %s", checkpoint.Status)
	}
}

func TestCoordinatorKeepsCompletedNonStreamChargeWhenFinalWriteFails(t *testing.T) {
	ledger, coordinator, params, input, owner := setupCoordinator(t, wordTokenizer{}, "hello world")
	attempt := &mockAttempt{owner: owner, chunks: []string{"done"}}
	coordinator.router = &mockRouter{attempts: []*mockAttempt{attempt}}
	writeErr := errors.New("client stopped reading")
	result, err := coordinator.Execute(context.Background(), params, input, false, func(string) error { return writeErr })
	if !errors.Is(err, writeErr) || result.Text != "done" || result.Usage.CompletionTokens != 1 {
		t.Fatalf("unexpected result after final write failure: result=%+v err=%v", result, err)
	}
	checkpoint := ledger.Snapshot().Requests[params.RequestID]
	if checkpoint.Status != billing.RequestSettled {
		t.Fatalf("completed nonstream inference was not settled before response write: %s", checkpoint.Status)
	}
}

func TestCoordinatorDoesNotRetryWhenOutputTokenizerFails(t *testing.T) {
	ledger, coordinator, params, input, ownerOne := setupCoordinator(t, wordTokenizer{outputErr: errors.New("helper unavailable")}, "hello world")
	ownerTwo := newID(t)
	router := &mockRouter{attempts: []*mockAttempt{
		{owner: ownerOne, chunks: []string{"generated"}},
		{owner: ownerTwo, chunks: []string{"must not run"}},
	}}
	coordinator.router = router
	result, err := coordinator.Execute(context.Background(), params, input, false, nil)
	if !errors.Is(err, ErrTokenizerUnavailable) || result.Attempts != 1 || router.acquired != 1 {
		t.Fatalf("output tokenizer failure retried: result=%+v err=%v acquired=%d", result, err, router.acquired)
	}
	if checkpoint := ledger.Snapshot().Requests[params.RequestID]; checkpoint.Status != billing.RequestRefunded {
		t.Fatalf("tokenizer failure left request nonterminal: %s", checkpoint.Status)
	}
}

func TestCoordinatorRejectsNilAttemptWithoutPanic(t *testing.T) {
	ledger, coordinator, params, input, _ := setupCoordinator(t, wordTokenizer{}, "hello world")
	coordinator.router = &nilAttemptRouter{}
	if _, err := coordinator.Execute(context.Background(), params, input, false, nil); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("nil attempt error=%v", err)
	}
	if len(ledger.Entries()) != 1 {
		t.Fatalf("nil attempt created financial entries: %d", len(ledger.Entries()))
	}
}

func TestCoordinatorSettlesZeroPricedMockWithoutSpendableFunds(t *testing.T) {
	ledger := billing.NewLedger()
	accountID, keyID, ownerID, requestID := newID(t), newID(t), newID(t), newID(t)
	capAmount, _ := domain.NewPiconero(0)
	zero, _ := domain.NewPiconero(0)
	priceVersion, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(referenceLedger{ledger: ledger}, wordTokenizer{}, &mockRouter{attempts: []*mockAttempt{{owner: ownerID, chunks: []string{"hello ", "world"}}}})
	if err != nil {
		t.Fatal(err)
	}
	input := inference.Request{Messages: []inference.Message{{Role: "user", Content: "hello world"}}, MaxTokens: 2}
	result, err := coordinator.Execute(context.Background(), Params{RequestID: requestID, AccountID: accountID, APIKeyID: keyID,
		ModelID: "cordova-chat-small-v1", DailySpendCap: capAmount,
		Price: billing.Price{Version: priceVersion, InputRate: zero, OutputRate: zero}, AcceptedAt: time.Now().UTC()}, input, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != string(billing.RequestSettled) || result.Charged.Int64() != 0 || result.Usage.TotalTokens != 4 {
		t.Fatalf("zero-price result=%+v", result)
	}
	balance, err := ledger.Balance(accountID)
	if err != nil || balance.Available.Int64() != 0 || balance.Reserved.Int64() != 0 {
		t.Fatalf("zero-price balance=%+v err=%v", balance, err)
	}
}

type nilAttemptRouter struct{}

func (*nilAttemptRouter) Acquire(context.Context, string, map[string]struct{}, time.Time) (Attempt, error) {
	return nil, nil
}

func setupCoordinator(t *testing.T, tokenizer Tokenizer, _ string) (*billing.Ledger, *Coordinator, Params, inference.Request, string) {
	t.Helper()
	ledger := billing.NewLedger()
	account, key, owner := newID(t), newID(t), newID(t)
	credit, _ := domain.NewPiconero(1_000)
	if _, err := ledger.CreditTest(account, "request-coordinator-credit", credit, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	capAmount, _ := domain.NewPiconero(1_000)
	priceVersion, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	price := billing.Price{Version: priceVersion, InputRate: mustMoney(t, 1_000_000), OutputRate: mustMoney(t, 2_000_000), OwnerShareBPS: 5_000}
	requestID := newID(t)
	router := &mockRouter{attempts: []*mockAttempt{{owner: owner}}}
	coordinator, err := NewCoordinator(referenceLedger{ledger: ledger}, tokenizer, router)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	params := Params{RequestID: requestID, AccountID: account, APIKeyID: key, ModelID: "cordova-chat-small-v1", DailySpendCap: capAmount,
		Price: price, AcceptedAt: now, Deadline: now.Add(time.Minute)}
	input := inference.Request{Messages: []inference.Message{{Role: "user", Content: "hello world"}}, MaxTokens: 2}
	return ledger, coordinator, params, input, owner
}

func mustMoney(t *testing.T, value int64) domain.Piconero {
	t.Helper()
	money, err := domain.NewPiconero(value)
	if err != nil {
		t.Fatal(err)
	}
	return money
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
