package billing

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"
)

func TestJournalValidatesBalancedPostingsAndCanonicalValues(t *testing.T) {
	account := testID(t)
	entry := Entry{OperationID: "reserve:" + testID(t) + ":" + testID(t), Kind: "reserve", CreatedAt: time.Now().UTC(), Postings: []Posting{
		posting(BucketAvailable, account, 10), posting(BucketReserved, account, -10),
	}}
	if err := validateJournalEntry(entry); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	entry.Postings[1].Delta = "-9"
	if err := validateJournalEntry(entry); err == nil {
		t.Fatal("unbalanced entry was accepted")
	}
	entry.Postings[1].Delta = "-010"
	if err := validateJournalEntry(entry); err == nil {
		t.Fatal("noncanonical posting was accepted")
	}
	entry.Postings[1].Delta = "-10"
	entry.Postings[1].AccountID = ""
	if err := validateJournalEntry(entry); err == nil {
		t.Fatal("account liability posting without an account was accepted")
	}
}

func TestJournalFingerprintIsStableAcrossHeadRetries(t *testing.T) {
	account := testID(t)
	entry := Entry{ID: testID(t), OperationID: "reserve:" + testID(t), Kind: "reserve", CreatedAt: time.Now().UTC(),
		Postings: []Posting{posting(BucketAvailable, account, 7), posting(BucketReserved, account, -7)}}
	first, err := journalFingerprint(entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.ID = testID(t)
	entry.PreviousHash = strings.Repeat("a", 64)
	entry.EntryHash = strings.Repeat("b", 64)
	second, err := journalFingerprint(entry)
	if err != nil || first != second {
		t.Fatalf("head retry changed operation fingerprint: %s %s %v", first, second, err)
	}
	entry.CreatedAt = entry.CreatedAt.Add(time.Hour)
	withDifferentTimestamp, err := journalFingerprint(entry)
	if err != nil || first != withDifferentTimestamp {
		t.Fatalf("operation retry timestamp changed the fingerprint: %s %s %v", first, withDifferentTimestamp, err)
	}
	entry.Postings[0].Delta = "8"
	third, err := journalFingerprint(entry)
	if err != nil || third == first {
		t.Fatalf("changed posting did not change operation fingerprint: %v", err)
	}
}

func TestRequestTransitionValidatesReserveRetryAndSettlement(t *testing.T) {
	ledger := NewLedger()
	now := time.Date(2026, 10, 9, 23, 58, 0, 0, time.UTC)
	account, key, ownerOne, ownerTwo := testID(t), testID(t), testID(t), testID(t)
	requestID, attemptOne, attemptTwo := testID(t), testID(t), testID(t)
	credit, _ := domain.NewPiconero(1_000)
	capLimit, _ := domain.NewPiconero(9_007_199_254_740_993)
	if _, err := ledger.CreditTest(account, "credit-transition", credit, now); err != nil {
		t.Fatal(err)
	}
	price := testPrice(t, 1_000_000, 1_000_000, 8_000)
	params := ReserveParams{RequestID: requestID, AttemptID: attemptOne, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: ownerOne,
		InputTokens: 2, MaxOutputTokens: 10, DailySpendCap: capLimit, Price: price, AcceptedAt: now}
	reservation, entry, err := ledger.Reserve(params)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := ledger.Snapshot().Requests[requestID]
	capKey := account + ":" + key + ":" + now.Format("2006-01-02")
	reserveCap := CapAdjustment{Key: capKey, Limit: capLimit.String(), SpentDelta: "0", ReservedDelta: reservation.Amount}
	if err := validateRequestTransition(entry, requestID, checkpoint, reserveCap); err != nil {
		t.Fatalf("reserve transition rejected: %v", err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil || !strings.Contains(string(encoded), `"daily_spend_cap_piconero":"9007199254740993"`) {
		t.Fatalf("request checkpoint lost integer money precision: %s err=%v", encoded, err)
	}
	badCap := reserveCap
	badCap.ReservedDelta = "1"
	if err := validateRequestTransition(entry, requestID, checkpoint, badCap); err == nil {
		t.Fatal("reserve accepted a cap delta different from the journal postings")
	}

	refund, err := ledger.FailAttempt(requestID, attemptOne, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = ledger.Snapshot().Requests[requestID]
	refundCap := CapAdjustment{Key: capKey, Limit: capLimit.String(), SpentDelta: "0", ReservedDelta: "-" + reservation.Amount}
	if err := validateRequestTransition(refund, requestID, checkpoint, refundCap); err != nil {
		t.Fatalf("retry refund transition rejected: %v", err)
	}

	params.AttemptID, params.OwnerID, params.AcceptedAt = attemptTwo, ownerTwo, now.Add(2*time.Minute)
	reservation, entry, err = ledger.Reserve(params)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = ledger.Snapshot().Requests[requestID]
	reserveCap.ReservedDelta = reservation.Amount
	if err := validateRequestTransition(entry, requestID, checkpoint, reserveCap); err != nil {
		t.Fatalf("second reserve transition rejected: %v", err)
	}

	entry, err = ledger.Settle(requestID, attemptTwo, 3, false, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = ledger.Snapshot().Requests[requestID]
	quote, err := CalculateQuote(2, 3, 10, price, false)
	if err != nil {
		t.Fatal(err)
	}
	settleCap := CapAdjustment{Key: capKey, Limit: capLimit.String(), SpentDelta: strconv.FormatInt(quote.ActualCost.Int64(), 10),
		ReservedDelta: "-" + reservation.Amount}
	if err := validateRequestTransition(entry, requestID, checkpoint, settleCap); err != nil {
		t.Fatalf("settlement transition rejected: %v", err)
	}
	settleCap.SpentDelta = "0"
	if err := validateRequestTransition(entry, requestID, checkpoint, settleCap); err == nil {
		t.Fatal("settlement accepted an incorrect spend-cap delta")
	}
}

func TestRedisScriptUsesStringMoneyArithmetic(t *testing.T) {
	if strings.Contains(appendLedgerScript, "tonumber") {
		t.Fatal("Redis financial script converts string values to floating point")
	}
	if !strings.Contains(appendLedgerScript, "XADD") || !strings.Contains(appendLedgerScript, "HSET") {
		t.Fatal("journal, balance and outbox are not written by the same Redis script")
	}
	firstMutation := strings.Index(appendLedgerScript, "redis.call('HSET'")
	capValidation := strings.Index(appendLedgerScript, "return {-4,'spend cap exceeded'}")
	verificationCapValidation := strings.Index(appendLedgerScript, "return {-6,'verification budget exceeded'}")
	if firstMutation < 0 || capValidation < 0 || verificationCapValidation < 0 ||
		firstMutation < capValidation || firstMutation < verificationCapValidation {
		t.Fatal("request and cap validation must complete before the script's first write")
	}
	for _, required := range []string{"KEYS[6],requestField,requestJSON", "KEYS[7],'limit:'..capKey"} {
		if !strings.Contains(appendLedgerScript, required) {
			t.Fatalf("Redis script is missing atomic request/cap write %q", required)
		}
	}
	for _, required := range []string{"KEYS[9]", "KEYS[10]", "CORDOVA_VERIFICATION_POSTING_MISMATCH",
		"CORDOVA_MISSING_VERIFICATION_REVENUE", "priorDay~=day", "verification_expense"} {
		if !strings.Contains(appendLedgerScript, required) {
			t.Fatalf("Redis script is missing A19 journal invariant %q", required)
		}
	}
	if _, err := domain.ParsePiconero("9007199254740993"); err != nil {
		t.Fatalf("contract fixture above 2^53 was rejected: %v", err)
	}
}
