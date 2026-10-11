package billing

import "cordova/server/internal/domain"

// MaxOperatorSeedVerificationBudget is the A19 daily seed ceiling before any
// inference revenue has been recorded for the UTC day. The amount supplied as
// OperatorFundedSeed must come from an explicitly funded operator balance.
const MaxOperatorSeedVerificationBudget int64 = 1_000_000_000

type VerificationBudgetSource string

const (
	VerificationBudgetDisabled VerificationBudgetSource = "disabled"
	VerificationBudgetOperator VerificationBudgetSource = "operator_seed"
	VerificationBudgetRevenue  VerificationBudgetSource = "inference_revenue"
)

// VerificationBudgetState is a read of the accounting inputs for one UTC day.
// It is deliberately a calculation input, not a counter or reservation store;
// spend and reservation updates must be committed atomically by the ledger.
type VerificationBudgetState struct {
	GrossInferenceRevenue domain.Piconero
	OperatorFundedSeed    domain.Piconero
	Spent                 domain.Piconero
	Reserved              domain.Piconero
}

type VerificationBudget struct {
	Limit     domain.Piconero
	Remaining domain.Piconero
	Source    VerificationBudgetSource
}

// CalculateVerificationBudget applies A19 using exact integer piconero math.
// Revenue-backed capacity is floor(5% of gross inference revenue). The
// operator seed is available only until the UTC day has any gross revenue.
// Historical seed spending can exceed a newly active revenue cap; in that
// case the remaining amount is zero and no further verification is admitted.
func CalculateVerificationBudget(state VerificationBudgetState) (VerificationBudget, error) {
	gross := state.GrossInferenceRevenue.Int64()
	seed := state.OperatorFundedSeed.Int64()
	spent := state.Spent.Int64()
	reserved := state.Reserved.Int64()
	if gross < 0 || seed < 0 || seed > MaxOperatorSeedVerificationBudget || spent < 0 || reserved < 0 {
		return VerificationBudget{}, ErrInvalidOperation
	}

	limit := int64(0)
	source := VerificationBudgetDisabled
	if gross > 0 {
		limit = gross / 20
		source = VerificationBudgetRevenue
	} else if seed > 0 {
		limit = seed
		source = VerificationBudgetOperator
	}

	remaining := int64(0)
	if spent < limit && reserved < limit-spent {
		remaining = limit - spent - reserved
	}
	limitAmount, err := domain.NewPiconero(limit)
	if err != nil {
		return VerificationBudget{}, ErrInvalidOperation
	}
	remainingAmount, err := domain.NewPiconero(remaining)
	if err != nil {
		return VerificationBudget{}, ErrInvalidOperation
	}
	return VerificationBudget{Limit: limitAmount, Remaining: remainingAmount, Source: source}, nil
}
