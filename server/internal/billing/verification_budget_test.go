package billing

import (
	"testing"
	"time"

	"cordova/server/internal/domain"
)

func TestCalculateVerificationBudgetUsesOperatorSeedBeforeRevenue(t *testing.T) {
	budget, err := CalculateVerificationBudget(VerificationBudgetState{
		OperatorFundedSeed: verificationPiconero(t, 700), Spent: verificationPiconero(t, 120), Reserved: verificationPiconero(t, 80),
	})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Source != VerificationBudgetOperator || budget.Limit.Int64() != 700 || budget.Remaining.Int64() != 500 {
		t.Fatalf("unexpected seed budget: %+v", budget)
	}
}

func TestCalculateVerificationBudgetUsesFivePercentOfGrossRevenue(t *testing.T) {
	budget, err := CalculateVerificationBudget(VerificationBudgetState{
		GrossInferenceRevenue: verificationPiconero(t, 100_001), OperatorFundedSeed: verificationPiconero(t, MaxOperatorSeedVerificationBudget),
		Spent: verificationPiconero(t, 1_000), Reserved: verificationPiconero(t, 2_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Source != VerificationBudgetRevenue || budget.Limit.Int64() != 5_000 || budget.Remaining.Int64() != 2_000 {
		t.Fatalf("unexpected revenue budget: %+v", budget)
	}
}

func TestCalculateVerificationBudgetDoesNotRoundUpOrReuseSeedAfterRevenue(t *testing.T) {
	budget, err := CalculateVerificationBudget(VerificationBudgetState{
		GrossInferenceRevenue: verificationPiconero(t, 19), OperatorFundedSeed: verificationPiconero(t, MaxOperatorSeedVerificationBudget),
	})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Source != VerificationBudgetRevenue || budget.Limit.Int64() != 0 || budget.Remaining.Int64() != 0 {
		t.Fatalf("unexpected zero revenue cap: %+v", budget)
	}
}

func TestCalculateVerificationBudgetClosesWhenExistingUsageExceedsNewRevenueCap(t *testing.T) {
	budget, err := CalculateVerificationBudget(VerificationBudgetState{
		GrossInferenceRevenue: verificationPiconero(t, 100), OperatorFundedSeed: verificationPiconero(t, 500),
		Spent: verificationPiconero(t, 10), Reserved: verificationPiconero(t, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Limit.Int64() != 5 || budget.Remaining.Int64() != 0 {
		t.Fatalf("unexpected exhausted budget: %+v", budget)
	}
}

func TestCalculateVerificationBudgetRejectsInvalidFundingInputs(t *testing.T) {
	tests := []VerificationBudgetState{
		{OperatorFundedSeed: verificationPiconero(t, MaxOperatorSeedVerificationBudget+1)},
	}
	for i, state := range tests {
		if _, err := CalculateVerificationBudget(state); err == nil {
			t.Errorf("case %d accepted invalid state: %+v", i, state)
		}
	}
}

func TestVerificationBudgetSettlementUsesAuditedGrossPostings(t *testing.T) {
	requestID, attemptID, accountID, ownerID, priceID := testID(t), testID(t), testID(t), testID(t), testID(t)
	createdAt := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	entry := Entry{ID: testID(t), OperationID: "settle:" + requestID, Kind: "settle", RequestID: requestID,
		AttemptID: attemptID, PriceVersion: priceID, CreatedAt: createdAt, Postings: []Posting{
			posting(BucketReserved, accountID, 10_001), posting(BucketAvailable, accountID, -1),
			posting(BucketOwnerPending, ownerID, -5_000), posting(BucketEquity, "", -5_000),
		}, VerificationBudget: &VerificationBudgetChange{Day: "2026-10-12", GrossRevenueDelta: "10000",
			OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: "0"}}
	if err := validateJournalEntry(entry); err != nil {
		t.Fatalf("valid gross-revenue settlement rejected: %v", err)
	}

	badGross := entry
	change := *entry.VerificationBudget
	change.GrossRevenueDelta = "9999"
	badGross.VerificationBudget = &change
	if err := validateJournalEntry(badGross); err == nil {
		t.Fatal("settlement gross revenue that differs from its financial postings was accepted")
	}

	missingBudget := entry
	missingBudget.VerificationBudget = nil
	if err := validateJournalEntry(missingBudget); err == nil {
		t.Fatal("positive gross settlement without a daily budget delta was accepted")
	}
}

func TestVerificationBudgetSpendPostsExpenseAndRejectsLegacyEquityOnlyForm(t *testing.T) {
	referenceID, ownerID := testID(t), testID(t)
	entry := Entry{ID: testID(t), OperationID: "verification-spend:" + referenceID, Kind: "verification_spend",
		ReferenceID: referenceID, CreatedAt: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Postings: []Posting{posting(BucketVerificationReserved, "", -600), posting(BucketOwnerPending, ownerID, -250),
			posting(BucketEquity, "", 600), posting(BucketVerificationExpense, "", 250)},
		VerificationBudget: &VerificationBudgetChange{Day: "2026-10-12", GrossRevenueDelta: "0",
			OperatorSeedDelta: "0", SpentDelta: "250", ReservedDelta: "-600"}}
	if err := validateJournalEntry(entry); err != nil {
		t.Fatalf("valid verification expense rejected: %v", err)
	}

	legacy := entry
	legacy.Postings = []Posting{posting(BucketVerificationReserved, "", -600), posting(BucketOwnerPending, ownerID, -250),
		posting(BucketEquity, "", 850)}
	if err := validateJournalEntry(legacy); err == nil {
		t.Fatal("verification expense hidden in equity instead of its planned bucket was accepted")
	}
}

func TestApplyVerificationBudgetChangeReplaysDailySeedAndUsage(t *testing.T) {
	day := "2026-10-12"
	totals := make(map[string]verificationBudgetTotals)
	changes := []*VerificationBudgetChange{
		{Day: day, GrossRevenueDelta: "0", OperatorSeedDelta: "1000", SpentDelta: "0", ReservedDelta: "0"},
		{Day: day, GrossRevenueDelta: "0", OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: "600"},
		{Day: day, GrossRevenueDelta: "0", OperatorSeedDelta: "0", SpentDelta: "250", ReservedDelta: "-600"},
	}
	for _, change := range changes {
		if err := applyVerificationBudgetChange(totals, change); err != nil {
			t.Fatalf("valid journal delta did not replay: %v", err)
		}
	}
	state := totals[day]
	budget, err := CalculateVerificationBudget(VerificationBudgetState{
		OperatorFundedSeed: verificationPiconero(t, state.operatorSeed),
		Spent:              verificationPiconero(t, state.spent),
		Reserved:           verificationPiconero(t, state.reserved),
	})
	if err != nil || budget.Remaining.Int64() != 750 {
		t.Fatalf("replayed budget=%+v err=%v", budget, err)
	}
	if err := applyVerificationBudgetChange(totals, &VerificationBudgetChange{Day: day, GrossRevenueDelta: "0",
		OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: "-1"}); err == nil {
		t.Fatal("replay accepted a reservation release below zero")
	}
}

func verificationPiconero(t *testing.T, value int64) domain.Piconero {
	t.Helper()
	amount, err := domain.NewPiconero(value)
	if err != nil {
		t.Fatal(err)
	}
	return amount
}
