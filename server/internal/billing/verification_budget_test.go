package billing

import (
	"testing"

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

func verificationPiconero(t *testing.T, value int64) domain.Piconero {
	t.Helper()
	amount, err := domain.NewPiconero(value)
	if err != nil {
		t.Fatal(err)
	}
	return amount
}
