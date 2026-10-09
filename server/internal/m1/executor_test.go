package m1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
	"cordova/server/internal/request"
)

type executorLedger struct{ ledger *billing.Ledger }

func (l executorLedger) Reserve(ctx context.Context, p billing.ReserveParams) (billing.Reservation, billing.Entry, error) {
	if err := ctx.Err(); err != nil {
		return billing.Reservation{}, billing.Entry{}, err
	}
	return l.ledger.Reserve(p)
}
func (l executorLedger) FailAttempt(ctx context.Context, requestID, attemptID string, retry bool, at time.Time) (billing.Entry, error) {
	if err := ctx.Err(); err != nil {
		return billing.Entry{}, err
	}
	return l.ledger.FailAttempt(requestID, attemptID, retry, at)
}
func (l executorLedger) Settle(ctx context.Context, requestID, attemptID string, tokens int64, cancelled bool, at time.Time) (billing.Entry, error) {
	if err := ctx.Err(); err != nil {
		return billing.Entry{}, err
	}
	return l.ledger.Settle(requestID, attemptID, tokens, cancelled, at)
}
func (l executorLedger) AbandonRetry(ctx context.Context, requestID string, at time.Time) (billing.Entry, error) {
	if err := ctx.Err(); err != nil {
		return billing.Entry{}, err
	}
	return l.ledger.AbandonRetry(requestID, at)
}

type fixedTokenizer struct{}

func (fixedTokenizer) CountInput(context.Context, string, []inference.Message) (int64, error) {
	return 1, nil
}
func (fixedTokenizer) CountOutput(context.Context, string, string) (int64, error) { return 1, nil }

type acceptedRouter struct{ owner string }

func (r acceptedRouter) Acquire(context.Context, string, map[string]struct{}, time.Time) (request.Attempt, error) {
	return acceptedAttempt{owner: r.owner}, nil
}

type acceptedAttempt struct{ owner string }

func (a acceptedAttempt) OwnerID() string { return a.owner }
func (a acceptedAttempt) Run(context.Context, inference.Request, func(string) error) (string, error) {
	return "", errors.New("coordinator bypassed accepted-attempt path")
}
func (a acceptedAttempt) RunAccepted(_ context.Context, _ inference.Request, accepted func() error, emit func(string) error) (string, error) {
	if accepted != nil {
		if err := accepted(); err != nil {
			return "", err
		}
	}
	if err := emit("hello"); err != nil {
		return "", err
	}
	return "stop", nil
}
func (acceptedAttempt) Release(context.Context) {}

func TestAccountedExecutorConnectsAPIIdentityToTerminalLedgerEntry(t *testing.T) {
	ledger := billing.NewLedger()
	accountID, keyID, ownerID, requestID := newTestID(t), newTestID(t), newTestID(t), newTestID(t)
	credit, _ := domain.NewPiconero(1_000)
	if _, err := ledger.CreditTest(accountID, "accounted-executor-credit", credit, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	version, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	inputRate, _ := domain.NewPiconero(1_000_000)
	outputRate, _ := domain.NewPiconero(2_000_000)
	coordinator, err := request.NewCoordinator(executorLedger{ledger}, fixedTokenizer{}, acceptedRouter{owner: ownerID})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewAccountedExecutor(coordinator, billing.Price{Version: version, InputRate: inputRate, OutputRate: outputRate, OwnerShareBPS: 5_000})
	if err != nil {
		t.Fatal(err)
	}
	principal := api.Principal{AccountID: accountID, KeyID: keyID, Kind: "key", DailySpendCapPiconero: "100",
		Scopes: map[string]bool{"chat:write": true}, AllowedModels: map[string]bool{api.ModelID: true}}
	var accepted bool
	var output strings.Builder
	result, failure := executor.Execute(context.Background(), api.CompletionRequest{ID: requestID, Principal: principal,
		ModelID: api.ModelID, Input: inference.Request{Messages: []inference.Message{{Role: "user", Content: "private prompt"}}, MaxTokens: 8},
		Streaming: true, Accepted: func() error { accepted = true; return nil }}, func(text string) error {
		if !accepted {
			t.Fatal("content was emitted before node acceptance")
		}
		output.WriteString(text)
		return nil
	})
	if failure != nil || !accepted || output.String() != "hello" || result.Text != "hello" || result.Status != "settled" || result.Charged != "3" || result.Usage.TotalTokens != 2 {
		t.Fatalf("result=%+v failure=%+v accepted=%v output=%q", result, failure, accepted, output.String())
	}
	balance, err := ledger.Balance(accountID)
	if err != nil || balance.Available.Int64() != 997 || balance.Reserved.Int64() != 0 {
		t.Fatalf("settled balance=%+v err=%v", balance, err)
	}
	ownerBalance, err := ledger.Balance(ownerID)
	if err != nil || ownerBalance.OwnerPending.Int64() != 1 {
		t.Fatalf("owner balance=%+v err=%v", ownerBalance, err)
	}
	journal, err := json.Marshal(ledger.Entries())
	if err != nil || strings.Contains(string(journal), "private prompt") || strings.Contains(string(journal), "hello") {
		t.Fatalf("ledger contains request content: %s err=%v", journal, err)
	}
}

func newTestID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
