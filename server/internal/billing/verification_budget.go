package billing

import (
	"strconv"

	"cordova/server/internal/domain"
)

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

// VerificationBudgetChange is the content-free, auditable A19 counter update
// stored inside the same journal entry as its balanced financial postings.
// Deltas are canonical signed decimal piconero strings.
type VerificationBudgetChange struct {
	Day               string `json:"utc_day"`
	GrossRevenueDelta string `json:"gross_revenue_delta_piconero"`
	OperatorSeedDelta string `json:"operator_seed_delta_piconero"`
	SpentDelta        string `json:"spent_delta_piconero"`
	ReservedDelta     string `json:"reserved_delta_piconero"`
}

type verificationBudgetTotals struct {
	grossRevenue int64
	operatorSeed int64
	spent        int64
	reserved     int64
}

type verificationReservationState struct {
	day    string
	amount int64
}

func validateVerificationBudgetChange(entry Entry) error {
	change := entry.VerificationBudget
	if change == nil {
		if entry.Kind == "settle" || entry.Kind == "cancel" {
			gross, err := settlementGrossFromPostings(entry)
			if err != nil || gross != 0 {
				return ErrInvalidOperation
			}
		}
		return nil
	}
	if !validAcceptedDay(change.Day) {
		return ErrInvalidOperation
	}
	gross, err := parseSigned(change.GrossRevenueDelta)
	if err != nil {
		return err
	}
	seed, err := parseSigned(change.OperatorSeedDelta)
	if err != nil {
		return err
	}
	spent, err := parseSigned(change.SpentDelta)
	if err != nil {
		return err
	}
	reserved, err := parseSigned(change.ReservedDelta)
	if err != nil {
		return err
	}

	var expected []Posting
	switch entry.Kind {
	case "settle", "cancel":
		postingGross, postingErr := settlementGrossFromPostings(entry)
		if entry.RequestID == "" || change.Day != entry.CreatedAt.UTC().Format("2006-01-02") ||
			postingErr != nil || gross <= 0 || gross != postingGross || seed != 0 || spent != 0 || reserved != 0 {
			return ErrInvalidOperation
		}
	case "verification_seed":
		if !validID(entry.ReferenceID) || change.Day != entry.CreatedAt.UTC().Format("2006-01-02") ||
			gross != 0 || seed <= 0 || seed > MaxOperatorSeedVerificationBudget || spent != 0 || reserved != 0 {
			return ErrInvalidOperation
		}
		expected = []Posting{posting(BucketCashHot, "", seed), posting(BucketEquity, "", -seed)}
	case "verification_reserve":
		if !validID(entry.ReferenceID) || change.Day != entry.CreatedAt.UTC().Format("2006-01-02") ||
			gross != 0 || seed != 0 || spent != 0 || reserved <= 0 {
			return ErrInvalidOperation
		}
		expected = []Posting{posting(BucketVerificationReserved, "", reserved), posting(BucketEquity, "", -reserved)}
	case "verification_spend":
		if !validID(entry.ReferenceID) || gross != 0 || seed != 0 || spent <= 0 || reserved >= 0 || spent > -reserved {
			return ErrInvalidOperation
		}
		ownerID := ""
		for _, item := range entry.Postings {
			if item.Bucket == BucketOwnerPending {
				if ownerID != "" || item.AccountID == "" || item.Delta != signedAmount(-spent) {
					return ErrInvalidOperation
				}
				ownerID = item.AccountID
			}
		}
		if !validID(ownerID) {
			return ErrInvalidOperation
		}
		expected = []Posting{posting(BucketVerificationReserved, "", reserved),
			posting(BucketOwnerPending, ownerID, -spent), posting(BucketEquity, "", -reserved),
			posting(BucketVerificationExpense, "", spent)}
	case "verification_release":
		if !validID(entry.ReferenceID) || gross != 0 || seed != 0 || spent != 0 || reserved >= 0 {
			return ErrInvalidOperation
		}
		expected = []Posting{posting(BucketVerificationReserved, "", reserved), posting(BucketEquity, "", -reserved)}
	default:
		return ErrInvalidOperation
	}
	if entry.Kind == "settle" || entry.Kind == "cancel" {
		return nil
	}
	if !samePostings(entry.Postings, expected) {
		return ErrInvalidOperation
	}
	return nil
}

func settlementGrossFromPostings(entry Entry) (int64, error) {
	var ownerCredit, platformTake int64
	ownerSeen, equitySeen := false, false
	for _, item := range entry.Postings {
		switch item.Bucket {
		case BucketOwnerPending:
			if ownerSeen {
				return 0, ErrInvalidOperation
			}
			ownerSeen = true
			delta, err := parseSigned(item.Delta)
			if err != nil || delta > 0 {
				return 0, ErrInvalidOperation
			}
			ownerCredit = -delta
		case BucketEquity:
			if equitySeen || item.AccountID != "" {
				return 0, ErrInvalidOperation
			}
			equitySeen = true
			delta, err := parseSigned(item.Delta)
			if err != nil || delta > 0 {
				return 0, ErrInvalidOperation
			}
			platformTake = -delta
		}
	}
	if !ownerSeen || !equitySeen {
		return 0, ErrInvalidOperation
	}
	gross, ok := checkedAdd(ownerCredit, platformTake)
	if !ok || gross > domain.MaxPiconero {
		return 0, domain.ErrPiconeroOverflow
	}
	return gross, nil
}

func signedAmount(value int64) string {
	return strconv.FormatInt(value, 10)
}

func applyVerificationBudgetChange(totals map[string]verificationBudgetTotals, change *VerificationBudgetChange) error {
	if change == nil {
		return nil
	}
	grossDelta, err := parseSigned(change.GrossRevenueDelta)
	if err != nil {
		return ErrInvalidSnapshot
	}
	seedDelta, err := parseSigned(change.OperatorSeedDelta)
	if err != nil {
		return ErrInvalidSnapshot
	}
	spentDelta, err := parseSigned(change.SpentDelta)
	if err != nil {
		return ErrInvalidSnapshot
	}
	reservedDelta, err := parseSigned(change.ReservedDelta)
	if err != nil {
		return ErrInvalidSnapshot
	}
	state := totals[change.Day]
	values := []*int64{&state.grossRevenue, &state.operatorSeed, &state.spent, &state.reserved}
	deltas := []int64{grossDelta, seedDelta, spentDelta, reservedDelta}
	for index, value := range values {
		next, ok := checkedAdd(*value, deltas[index])
		if !ok || next < 0 || next > domain.MaxPiconero {
			return ErrInvalidSnapshot
		}
		*value = next
	}
	if state.operatorSeed > MaxOperatorSeedVerificationBudget {
		return ErrInvalidSnapshot
	}
	totals[change.Day] = state
	return nil
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
