package m1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/request"
)

func TestBillingBalanceRequiresSessionAndReturnsExactMoneyStrings(t *testing.T) {
	available, _ := domain.NewPiconero(9_007_199_254_740_993)
	reserved, _ := domain.NewPiconero(17)
	reader := &fakeAccountingReader{ready: true, balance: billing.Balance{Available: available, Reserved: reserved}}
	h := &Handler{ledger: reader, authenticate: func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: testM1ID(t), Kind: "session"}, true
	}}
	recorder := httptest.NewRecorder()
	h.billingBalance(recorder, httptest.NewRequest(http.MethodGet, "/api/billing/balance", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("balance status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Available string `json:"available_piconero"`
		Reserved  string `json:"reserved_piconero"`
		Currency  string `json:"currency"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Available != "9007199254740993" || response.Reserved != "17" || response.Currency != "XMR" || response.UpdatedAt == "" {
		t.Fatalf("balance did not preserve exact money values: %+v", response)
	}

	h.authenticate = func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: testM1ID(t), Kind: "key", KeyID: testM1ID(t), Scopes: map[string]bool{"usage:read": true}}, true
	}
	recorder = httptest.NewRecorder()
	h.billingBalance(recorder, httptest.NewRequest(http.MethodGet, "/api/billing/balance", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("API key balance access status=%d, want 401", recorder.Code)
	}
}

func TestRequestStatusIsOwnerScopedAndReportsSettledCharge(t *testing.T) {
	accountID, keyID, ownerID, requestID, attemptID := testM1ID(t), testM1ID(t), testM1ID(t), testM1ID(t), testM1ID(t)
	version, _ := domain.ParseID("00000000-0000-4000-8000-000000000001")
	rate, _ := domain.NewPiconero(1_000_000)
	accepted := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	finished := accepted.Add(1250 * time.Millisecond)
	checkpoint := billing.RequestCheckpoint{
		AccountID: accountID, APIKeyID: keyID, ModelID: "cordova-chat-small-v1", InputTokens: 2, MaxOutputTokens: 10,
		AcceptedAt: accepted, AcceptedDay: accepted.Format("2006-01-02"), Price: billing.Price{Version: version, InputRate: rate, OutputRate: rate, OwnerShareBPS: 5_000},
		Status: billing.RequestSettled,
		Attempts: map[string]billing.AttemptCheckpoint{attemptID: {
			Reservation:  billing.Reservation{RequestID: requestID, AttemptID: attemptID, OwnerID: ownerID, Status: billing.AttemptSettled},
			OutputTokens: 3,
		}},
	}
	reader := &fakeAccountingReader{ready: true, requestID: requestID, checkpoint: checkpoint,
		operation: billing.Entry{OperationID: "settle:" + requestID, CreatedAt: finished}}
	h := &Handler{ledger: reader, authenticate: func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: accountID, Kind: "session"}, true
	}}
	recorder := httptest.NewRecorder()
	h.requestStatus(recorder, requestRequest(requestID))
	if recorder.Code != http.StatusOK {
		t.Fatalf("request status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response requestStatusView
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RequestID != requestID || response.Status != "settled" || response.OutputTokens != 3 || response.Charged == nil || response.Charged.Int64() != 5 ||
		response.LatencyMS == nil || *response.LatencyMS != 1250 || response.FinishedAt == nil || !response.FinishedAt.Equal(finished) {
		t.Fatalf("settled request metadata is incorrect: %+v", response)
	}

	h.authenticate = func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: testM1ID(t), Kind: "session"}, true
	}
	recorder = httptest.NewRecorder()
	h.requestStatus(recorder, requestRequest(requestID))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-account request lookup status=%d, want 404", recorder.Code)
	}

	h.authenticate = func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: accountID, Kind: "key", KeyID: testM1ID(t), Scopes: map[string]bool{"usage:read": true}}, true
	}
	recorder = httptest.NewRecorder()
	h.requestStatus(recorder, requestRequest(requestID))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-key request lookup status=%d, want 404", recorder.Code)
	}

	h.authenticate = func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: accountID, Kind: "key", KeyID: keyID}, true
	}
	recorder = httptest.NewRecorder()
	h.requestStatus(recorder, requestRequest(requestID))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("API key without usage:read status=%d, want 403", recorder.Code)
	}
}

func TestRequestStatusUsesUnknownChargeWhileLedgerIsRecovering(t *testing.T) {
	accountID, requestID, attemptID := testM1ID(t), testM1ID(t), testM1ID(t)
	accepted := time.Now().UTC().Truncate(time.Second)
	reader := &fakeAccountingReader{ready: false, requestID: requestID, checkpoint: billing.RequestCheckpoint{
		AccountID: accountID, APIKeyID: testM1ID(t), ModelID: "cordova-chat-small-v1", InputTokens: 2, MaxOutputTokens: 10,
		AcceptedAt: accepted, AcceptedDay: accepted.Format("2006-01-02"), Status: billing.RequestRunning,
		Attempts: map[string]billing.AttemptCheckpoint{attemptID: {Reservation: billing.Reservation{OwnerID: testM1ID(t), Status: billing.AttemptReserved}}},
	}}
	h := &Handler{ledger: reader, authenticate: func(*http.Request) (api.Principal, bool) {
		return api.Principal{AccountID: accountID, Kind: "session"}, true
	}}
	recorder := httptest.NewRecorder()
	h.requestStatus(recorder, requestRequest(requestID))
	if recorder.Code != http.StatusOK {
		t.Fatalf("recovering request status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "recovering" || response["charged_piconero"] != nil || response["finished_at"] != nil || response["latency_ms"] != nil {
		t.Fatalf("recovery response implied a terminal result: %+v", response)
	}
	if reader.operationCalls != 0 {
		t.Fatalf("recovery response read uncertain terminal operation %d times", reader.operationCalls)
	}
}

func TestCompletionRoutingErrorsUseContractStatusAndRetryAfter(t *testing.T) {
	cases := []struct {
		err        error
		statusCode int
		code       string
		retryAfter int
	}{
		{err: request.ErrModelWarming, statusCode: http.StatusServiceUnavailable, code: "model_warming", retryAfter: 5},
		{err: request.ErrNoCapacity, statusCode: http.StatusServiceUnavailable, code: "no_capacity", retryAfter: 2},
		{err: request.ErrModelNotFound, statusCode: http.StatusNotFound, code: "model_not_found"},
	}
	for _, test := range cases {
		failure := mapCompletionError(test.err)
		if failure.StatusCode != test.statusCode || failure.Code != test.code || failure.RetryAfterSeconds != test.retryAfter {
			t.Errorf("mapCompletionError(%v) = %+v, want status=%d code=%q retry=%d", test.err, failure,
				test.statusCode, test.code, test.retryAfter)
		}
	}
}

type fakeAccountingReader struct {
	ready          bool
	balance        billing.Balance
	requestID      string
	checkpoint     billing.RequestCheckpoint
	operation      billing.Entry
	operationCalls int
}

func (f *fakeAccountingReader) Ready() bool { return f.ready }

func (f *fakeAccountingReader) Balance(_ context.Context, _ string) (billing.Balance, error) {
	return f.balance, nil
}

func (f *fakeAccountingReader) Request(_ context.Context, id string) (billing.RequestCheckpoint, uint64, error) {
	if id != f.requestID {
		return billing.RequestCheckpoint{}, 0, billing.ErrRequestNotFound
	}
	return f.checkpoint, 1, nil
}

func (f *fakeAccountingReader) Operation(_ context.Context, _ string) (billing.Entry, error) {
	f.operationCalls++
	return f.operation, nil
}

func requestRequest(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/requests/"+id, nil)
	r.SetPathValue("id", id)
	return r
}

func testM1ID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
