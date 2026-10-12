package billing

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"cordova/server/internal/domain"
	"github.com/redis/go-redis/v9"
)

// RedisLedger is the request lifecycle built on RedisJournal. Call Recover
// after fencing node sessions and before serving financial requests.
type RedisLedger struct {
	journal *RedisJournal
}

func NewRedisLedger(journal *RedisJournal) (*RedisLedger, error) {
	if journal == nil {
		return nil, errors.New("Redis journal is required")
	}
	return &RedisLedger{journal: journal}, nil
}

// CreditTest creates isolated test credits. Production deposits must use the
// chain watcher and their own idempotent journal transition.
func (l *RedisLedger) CreditTest(ctx context.Context, accountID, creditID string, amount domain.Piconero, at time.Time) (Entry, error) {
	if !validID(accountID) || creditID == "" || len(creditID) > 120 || amount.Int64() <= 0 || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	return l.journal.Append(ctx, Entry{OperationID: "test-credit:" + creditID, Kind: "test_credit", CreatedAt: at.UTC(), Postings: []Posting{
		posting(BucketCashHot, "", amount.Int64()), posting(BucketAvailable, accountID, -amount.Int64()),
	}})
}

// VerificationBudget reads one day's exact piconero counters. Admission must
// still use ReserveVerificationBudget, whose cap check occurs in the journal's
// atomic append script.
func (l *RedisLedger) VerificationBudget(ctx context.Context, at time.Time) (VerificationBudget, error) {
	if at.IsZero() {
		return VerificationBudget{}, ErrInvalidOperation
	}
	if !l.journal.Ready() {
		return VerificationBudget{}, ErrJournalCircuitOpen
	}
	day := at.UTC().Format("2006-01-02")
	values, err := l.journal.client.HMGet(ctx, l.journal.verificationKey,
		"gross:"+day, "seed:"+day, "spent:"+day, "reserved:"+day).Result()
	if err != nil {
		return VerificationBudget{}, l.journal.markUncertain(err)
	}
	if len(values) != 4 {
		return VerificationBudget{}, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	present := 0
	for _, value := range values {
		if value != nil {
			present++
		}
	}
	if present != 0 && present != len(values) {
		return VerificationBudget{}, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	read := func(index int) (domain.Piconero, error) {
		value := values[index]
		if value == nil {
			return domain.NewPiconero(0)
		}
		text, ok := value.(string)
		if !ok {
			return domain.Piconero{}, ErrInvalidSnapshot
		}
		return domain.ParsePiconero(text)
	}
	gross, err := read(0)
	if err != nil {
		return VerificationBudget{}, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	seed, err := read(1)
	if err != nil {
		return VerificationBudget{}, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	spent, err := read(2)
	if err != nil {
		return VerificationBudget{}, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	reserved, err := read(3)
	if err != nil {
		return VerificationBudget{}, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	return CalculateVerificationBudget(VerificationBudgetState{GrossInferenceRevenue: gross, OperatorFundedSeed: seed,
		Spent: spent, Reserved: reserved})
}

// FundOperatorVerificationSeed journals verified operator funding for the
// A19 pre-revenue seed. Call only after the payment worker has independently
// confirmed receipt of the operator transfer.
func (l *RedisLedger) FundOperatorVerificationSeed(ctx context.Context, fundingID string, amount domain.Piconero, at time.Time) (Entry, error) {
	if !validID(fundingID) || amount.Int64() <= 0 || amount.Int64() > MaxOperatorSeedVerificationBudget || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	if !l.journal.Ready() {
		return Entry{}, ErrJournalCircuitOpen
	}
	operationID := "verification-seed:" + fundingID
	if prior, err := l.journal.Operation(ctx, operationID); err == nil {
		if prior.Kind == "verification_seed" && prior.ReferenceID == fundingID && prior.VerificationBudget != nil &&
			prior.VerificationBudget.OperatorSeedDelta == amount.String() {
			return prior, nil
		}
		return Entry{}, ErrConflict
	} else if !errors.Is(err, ErrRequestNotFound) {
		return Entry{}, err
	}
	day := at.UTC().Format("2006-01-02")
	return l.journal.Append(ctx, Entry{OperationID: operationID, Kind: "verification_seed",
		ReferenceID: fundingID, CreatedAt: at.UTC(), Postings: []Posting{posting(BucketCashHot, "", amount.Int64()),
			posting(BucketEquity, "", -amount.Int64())}, VerificationBudget: &VerificationBudgetChange{Day: day,
			GrossRevenueDelta: "0", OperatorSeedDelta: amount.String(), SpentDelta: "0", ReservedDelta: "0"}})
}

// ReserveVerificationBudget atomically reserves verification spend against the
// daily A19 cap and writes the matching financial reservation entry.
func (l *RedisLedger) ReserveVerificationBudget(ctx context.Context, referenceID string, amount domain.Piconero, at time.Time) (Entry, error) {
	if !validID(referenceID) || amount.Int64() <= 0 || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	if !l.journal.Ready() {
		return Entry{}, ErrJournalCircuitOpen
	}
	operationID := "verification-reserve:" + referenceID
	if prior, err := l.journal.Operation(ctx, operationID); err == nil {
		if prior.Kind == "verification_reserve" && prior.ReferenceID == referenceID && prior.VerificationBudget != nil &&
			prior.VerificationBudget.ReservedDelta == amount.String() {
			return prior, nil
		}
		return Entry{}, ErrConflict
	} else if !errors.Is(err, ErrRequestNotFound) {
		return Entry{}, err
	}
	day := at.UTC().Format("2006-01-02")
	return l.journal.Append(ctx, Entry{OperationID: operationID, Kind: "verification_reserve",
		ReferenceID: referenceID, CreatedAt: at.UTC(), Postings: []Posting{posting(BucketVerificationReserved, "", amount.Int64()),
			posting(BucketEquity, "", -amount.Int64())}, VerificationBudget: &VerificationBudgetChange{Day: day,
			GrossRevenueDelta: "0", OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: amount.String()}})
}

// SettleVerificationBudget converts one live reservation into owner earnings.
// The reservation's day and amount are read from the committed reserve entry;
// callers cannot substitute a later date or draw on another verification.
func (l *RedisLedger) SettleVerificationBudget(ctx context.Context, referenceID, ownerID string, actual domain.Piconero, at time.Time) (Entry, error) {
	if !validID(referenceID) || !validID(ownerID) || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	if !l.journal.Ready() {
		return Entry{}, ErrJournalCircuitOpen
	}
	if actual.Int64() == 0 {
		return l.ReleaseVerificationBudget(ctx, referenceID, at)
	}
	operationID := "verification-spend:" + referenceID
	if prior, err := l.journal.Operation(ctx, operationID); err == nil {
		if prior.Kind == "verification_spend" && prior.ReferenceID == referenceID && prior.VerificationBudget != nil &&
			prior.VerificationBudget.SpentDelta == actual.String() && hasOwnerPosting(prior, ownerID, actual.Int64()) {
			return prior, nil
		}
		return Entry{}, ErrConflict
	} else if !errors.Is(err, ErrRequestNotFound) {
		return Entry{}, err
	}
	reserve, amount, err := l.verificationReservation(ctx, referenceID)
	if err != nil {
		return Entry{}, err
	}
	if actual.Int64() > amount {
		return Entry{}, ErrInvalidOperation
	}
	return l.journal.Append(ctx, Entry{OperationID: operationID, Kind: "verification_spend", ReferenceID: referenceID,
		CreatedAt: at.UTC(), Postings: []Posting{posting(BucketVerificationReserved, "", -amount),
			posting(BucketOwnerPending, ownerID, -actual.Int64()), posting(BucketEquity, "", amount),
			posting(BucketVerificationExpense, "", actual.Int64())},
		VerificationBudget: &VerificationBudgetChange{Day: reserve.VerificationBudget.Day, GrossRevenueDelta: "0",
			OperatorSeedDelta: "0", SpentDelta: actual.String(), ReservedDelta: signedAmount(-amount)}})
}

// ReleaseVerificationBudget returns an unused verification reservation. The
// transition is idempotent by reservation ID and is fenced by the same journal.
func (l *RedisLedger) ReleaseVerificationBudget(ctx context.Context, referenceID string, at time.Time) (Entry, error) {
	if !validID(referenceID) || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	if !l.journal.Ready() {
		return Entry{}, ErrJournalCircuitOpen
	}
	operationID := "verification-release:" + referenceID
	if prior, err := l.journal.Operation(ctx, operationID); err == nil {
		if prior.Kind == "verification_release" && prior.ReferenceID == referenceID && prior.VerificationBudget != nil {
			return prior, nil
		}
		return Entry{}, ErrConflict
	} else if !errors.Is(err, ErrRequestNotFound) {
		return Entry{}, err
	}
	reserve, amount, err := l.verificationReservation(ctx, referenceID)
	if err != nil {
		return Entry{}, err
	}
	return l.journal.Append(ctx, Entry{OperationID: operationID, Kind: "verification_release", ReferenceID: referenceID,
		CreatedAt: at.UTC(), Postings: []Posting{posting(BucketVerificationReserved, "", -amount), posting(BucketEquity, "", amount)},
		VerificationBudget: &VerificationBudgetChange{Day: reserve.VerificationBudget.Day, GrossRevenueDelta: "0",
			OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: signedAmount(-amount)}})
}

func hasOwnerPosting(entry Entry, ownerID string, amount int64) bool {
	for _, item := range entry.Postings {
		if item.Bucket == BucketOwnerPending && item.AccountID == ownerID && item.Delta == signedAmount(-amount) {
			return true
		}
	}
	return false
}

func (l *RedisLedger) verificationReservation(ctx context.Context, referenceID string) (Entry, int64, error) {
	if !l.journal.Ready() {
		return Entry{}, 0, ErrJournalCircuitOpen
	}
	reserve, err := l.journal.Operation(ctx, "verification-reserve:"+referenceID)
	if err != nil {
		return Entry{}, 0, err
	}
	if reserve.Kind != "verification_reserve" || reserve.ReferenceID != referenceID || reserve.VerificationBudget == nil {
		return Entry{}, 0, ErrConflict
	}
	current, err := l.journal.client.HGet(ctx, l.journal.verificationReservationsKey, referenceID).Result()
	if err == redis.Nil {
		return Entry{}, 0, ErrConflict
	}
	if err != nil {
		return Entry{}, 0, l.journal.markUncertain(err)
	}
	amount, err := domain.ParsePiconero(reserve.VerificationBudget.ReservedDelta)
	if err != nil || amount.Int64() <= 0 || current != amount.String()+"|"+reserve.VerificationBudget.Day {
		return Entry{}, 0, l.journal.markUncertain(ErrInvalidSnapshot)
	}
	return reserve, amount.Int64(), nil
}

// Reserve atomically checks available funds and the acceptance-day cap,
// journals the reservation, and waits for the configured WAITAOF barrier.
func (l *RedisLedger) Reserve(ctx context.Context, params ReserveParams) (Reservation, Entry, error) {
	if err := validateReserveParams(params); err != nil {
		return Reservation{}, Entry{}, err
	}
	if !l.journal.Ready() {
		return Reservation{}, Entry{}, ErrJournalCircuitOpen
	}
	day := params.AcceptedAt.UTC().Format("2006-01-02")
	request, revision, err := l.journal.Request(ctx, params.RequestID)
	newRequest := errors.Is(err, ErrRequestNotFound)
	if err != nil && !newRequest {
		return Reservation{}, Entry{}, err
	}
	if newRequest {
		request = RequestCheckpoint{AccountID: params.AccountID, APIKeyID: params.APIKeyID, ModelID: params.ModelID, OwnerID: params.OwnerID,
			InputTokens: params.InputTokens, MaxOutputTokens: params.MaxOutputTokens, DailySpendCap: params.DailySpendCap,
			AcceptedDay: day, AcceptedAt: params.AcceptedAt.UTC(), Price: params.Price, Status: RequestRunning,
			Attempts: make(map[string]AttemptCheckpoint, 1)}
		revision = 0
	} else {
		if !sameRequestParams(request, params) {
			return Reservation{}, Entry{}, ErrConflict
		}
		if prior, exists := request.Attempts[params.AttemptID]; exists {
			if reservationMatches(prior.Reservation, params) && prior.Reservation.Status == AttemptReserved {
				entry, err := l.journal.Operation(ctx, "reserve:"+params.RequestID+":"+params.AttemptID)
				return prior.Reservation, entry, err
			}
			return Reservation{}, Entry{}, ErrRetryUnavailable
		}
		if request.Status != RequestAccepted || len(request.Attempts) >= maxAttempts {
			return Reservation{}, Entry{}, ErrRetryUnavailable
		}
		for _, previous := range request.Attempts {
			if previous.Reservation.OwnerID == params.OwnerID {
				return Reservation{}, Entry{}, ErrRetryUnavailable
			}
		}
		params.AcceptedAt = request.AcceptedAt
		params.DailySpendCap, _ = domain.NewPiconero(request.DailySpendCap.Int64())
		params.Price = request.Price
		request.OwnerID = params.OwnerID
		request.Status = RequestRunning
	}
	quote, err := CalculateQuote(params.InputTokens, 0, params.MaxOutputTokens, params.Price, false)
	if err != nil {
		return Reservation{}, Entry{}, err
	}
	reservation := Reservation{RequestID: params.RequestID, AttemptID: params.AttemptID, ModelID: params.ModelID, AccountID: params.AccountID,
		APIKeyID: params.APIKeyID, OwnerID: params.OwnerID, InputTokens: params.InputTokens, MaxOutputTokens: params.MaxOutputTokens,
		DailySpendCap: params.DailySpendCap.String(), Price: params.Price, AcceptedDay: params.AcceptedAt.UTC().Format("2006-01-02"),
		Amount: quote.Reservation.String(), Status: AttemptReserved}
	request.Attempts[params.AttemptID] = AttemptCheckpoint{Reservation: reservation}
	entry := Entry{OperationID: "reserve:" + params.RequestID + ":" + params.AttemptID, Kind: "reserve", RequestID: params.RequestID,
		AttemptID: params.AttemptID, PriceVersion: params.Price.Version.String(), CreatedAt: params.AcceptedAt.UTC(), Postings: []Posting{
			posting(BucketAvailable, params.AccountID, quote.Reservation.Int64()), posting(BucketReserved, params.AccountID, -quote.Reservation.Int64()),
		}}
	cap := CapAdjustment{Key: params.AccountID + ":" + params.APIKeyID + ":" + reservation.AcceptedDay,
		Limit: params.DailySpendCap.String(), SpentDelta: "0", ReservedDelta: reservation.Amount}
	entry, err = l.journal.AppendRequest(ctx, entry, params.RequestID, revision, request, cap)
	if err != nil {
		return Reservation{}, Entry{}, err
	}
	return reservation, entry, nil
}

// FailAttempt fully releases one failed reservation. retryAllowed leaves the
// request open only when a second distinct owner can still be reserved.
func (l *RedisLedger) FailAttempt(ctx context.Context, requestID, attemptID string, retryAllowed bool, at time.Time) (Entry, error) {
	request, revision, err := l.journal.Request(ctx, requestID)
	if err != nil {
		return Entry{}, err
	}
	if prior, exists := request.Attempts[attemptID]; exists && prior.Reservation.Status == AttemptRefunded {
		return l.journal.Operation(ctx, "refund:"+requestID+":"+attemptID)
	}
	if request.Status != RequestRunning || at.IsZero() {
		return Entry{}, ErrAlreadyTerminal
	}
	attempt, ok := request.Attempts[attemptID]
	if !ok || attempt.Reservation.Status != AttemptReserved || attempt.OutputTokens != 0 {
		return Entry{}, ErrConflict
	}
	amount, err := domain.ParsePiconero(attempt.Reservation.Amount)
	if err != nil {
		return Entry{}, err
	}
	attempt.Reservation.Status = AttemptRefunded
	request.Attempts[attemptID] = attempt
	if retryAllowed && len(request.Attempts) < maxAttempts {
		request.Status = RequestAccepted
	} else {
		request.Status = RequestRefunded
	}
	entry := Entry{OperationID: "refund:" + requestID + ":" + attemptID, Kind: "attempt_refund", RequestID: requestID,
		AttemptID: attemptID, PriceVersion: request.Price.Version.String(), CreatedAt: at.UTC(), Postings: []Posting{
			posting(BucketReserved, request.AccountID, amount.Int64()), posting(BucketAvailable, request.AccountID, -amount.Int64()),
		}}
	cap := CapAdjustment{Key: request.AccountID + ":" + request.APIKeyID + ":" + request.AcceptedDay,
		Limit: request.DailySpendCap.String(), SpentDelta: "0", ReservedDelta: negativeAmount(amount)}
	return l.journal.AppendRequest(ctx, entry, requestID, revision, request, cap)
}

// AbandonRetry terminally closes a refunded request when live routing cannot
// produce a distinct second owner. It is idempotent and leaves financial caps
// and balances unchanged.
func (l *RedisLedger) AbandonRetry(ctx context.Context, requestID string, at time.Time) (Entry, error) {
	if at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	request, revision, err := l.journal.Request(ctx, requestID)
	if err != nil {
		return Entry{}, err
	}
	if request.Status == RequestRefunded {
		entry, operationErr := l.journal.Operation(ctx, "abandon:"+requestID)
		if operationErr == nil {
			return entry, nil
		}
		if errors.Is(operationErr, ErrRequestNotFound) {
			return Entry{}, ErrAlreadyTerminal
		}
		return Entry{}, operationErr
	}
	if request.Status != RequestAccepted || len(request.Attempts) == 0 {
		return Entry{}, ErrAlreadyTerminal
	}
	for _, attempt := range request.Attempts {
		if attempt.Reservation.Status != AttemptRefunded || attempt.OutputTokens != 0 {
			return Entry{}, ErrConflict
		}
	}
	attemptID, err := ownerAttemptID(request)
	if err != nil {
		return Entry{}, err
	}
	request.Status = RequestRefunded
	entry := Entry{OperationID: "abandon:" + requestID, Kind: "request_abandoned", RequestID: requestID,
		AttemptID: attemptID, PriceVersion: request.Price.Version.String(), CreatedAt: at.UTC(), Postings: []Posting{
			posting(BucketAvailable, request.AccountID, 0), posting(BucketReserved, request.AccountID, 0),
		}}
	cap := CapAdjustment{Key: request.AccountID + ":" + request.APIKeyID + ":" + request.AcceptedDay,
		Limit: request.DailySpendCap.String(), SpentDelta: "0", ReservedDelta: "0"}
	return l.journal.AppendRequest(ctx, entry, requestID, revision, request, cap)
}

// Settle commits a successful output or the verified client-cancel prefix.
// It is idempotent for the same terminal operation and rejects a competing one.
func (l *RedisLedger) Settle(ctx context.Context, requestID, attemptID string, outputTokens int64, cancelled bool, at time.Time) (Entry, error) {
	request, revision, err := l.journal.Request(ctx, requestID)
	if err != nil {
		return Entry{}, err
	}
	operationID := "settle:" + requestID
	kind := "settle"
	if cancelled {
		operationID, kind = "cancel:"+requestID, "cancel"
	}
	if request.Status == RequestSettled || request.Status == RequestCancelled || request.Status == RequestRefunded {
		if prior, exists := request.Attempts[attemptID]; exists &&
			((cancelled && request.Status == RequestCancelled) || (!cancelled && request.Status == RequestSettled)) &&
			prior.Reservation.Status == terminalAttemptStatus(cancelled) && prior.OutputTokens == outputTokens {
			return l.journal.Operation(ctx, operationID)
		}
		return Entry{}, ErrAlreadyTerminal
	}
	if request.Status != RequestRunning || at.IsZero() {
		return Entry{}, ErrConflict
	}
	attempt, ok := request.Attempts[attemptID]
	if !ok || attempt.Reservation.Status != AttemptReserved {
		return Entry{}, ErrConflict
	}
	quote, err := CalculateQuote(request.InputTokens, outputTokens, request.MaxOutputTokens, request.Price, cancelled)
	if err != nil {
		return Entry{}, err
	}
	reservation, err := domain.ParsePiconero(attempt.Reservation.Amount)
	if err != nil || quote.Reservation.Int64() != reservation.Int64() {
		return Entry{}, ErrConflict
	}
	request.Status = RequestSettled
	attempt.OutputTokens = outputTokens
	attempt.Reservation.Status = AttemptSettled
	if cancelled {
		request.Status = RequestCancelled
		attempt.Reservation.Status = AttemptCancelled
	}
	request.Attempts[attemptID] = attempt
	entry := Entry{OperationID: operationID, Kind: kind, RequestID: requestID, AttemptID: attemptID,
		PriceVersion: request.Price.Version.String(), CreatedAt: at.UTC(), Postings: []Posting{
			posting(BucketReserved, request.AccountID, reservation.Int64()), posting(BucketAvailable, request.AccountID, -quote.Refund.Int64()),
			posting(BucketOwnerPending, attempt.Reservation.OwnerID, -quote.OwnerCredit.Int64()), posting(BucketEquity, "", -quote.PlatformTake.Int64()),
		}}
	if quote.ActualCost.Int64() > 0 {
		entry.VerificationBudget = &VerificationBudgetChange{Day: at.UTC().Format("2006-01-02"),
			GrossRevenueDelta: quote.ActualCost.String(), OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: "0"}
	}
	cap := CapAdjustment{Key: request.AccountID + ":" + request.APIKeyID + ":" + request.AcceptedDay,
		Limit: request.DailySpendCap.String(), SpentDelta: quote.ActualCost.String(), ReservedDelta: negativeAmount(reservation)}
	return l.journal.AppendRequest(ctx, entry, requestID, revision, request, cap)
}

func (l *RedisLedger) Balance(ctx context.Context, accountID string) (Balance, error) {
	if !validID(accountID) {
		return Balance{}, ErrInvalidOperation
	}
	if !l.journal.Ready() {
		return Balance{}, ErrJournalCircuitOpen
	}
	values := []string{BucketAvailable, BucketReserved, BucketOwnerPending, BucketOwnerAvailable, BucketPayoutReserved}
	amounts := make([]domain.Piconero, len(values))
	for index, bucket := range values {
		posted, err := l.journal.client.HGet(ctx, l.journal.balancesKey, bucket+":"+accountID).Result()
		if err != nil && err != redis.Nil {
			return Balance{}, l.journal.markUncertain(err)
		}
		if err == redis.Nil {
			posted = "0"
		}
		signed, err := parseSigned(posted)
		if err != nil || signed > 0 || signed == -1<<63 {
			return Balance{}, l.journal.markUncertain(ErrInvalidSnapshot)
		}
		amount, err := domain.NewPiconero(-signed)
		if err != nil {
			return Balance{}, l.journal.markUncertain(err)
		}
		amounts[index] = amount
	}
	return Balance{Available: amounts[0], Reserved: amounts[1], OwnerPending: amounts[2], OwnerAvailable: amounts[3], PayoutReserved: amounts[4]}, nil
}

func validateReserveParams(params ReserveParams) error {
	if !validID(params.RequestID) || !validID(params.AttemptID) || !validID(params.AccountID) || !validID(params.APIKeyID) || !validID(params.OwnerID) ||
		!validModelID(params.ModelID) || params.InputTokens < 0 || params.InputTokens > maxRequestInputTokens || params.MaxOutputTokens < 1 || params.MaxOutputTokens > maxRequestOutputTokens ||
		params.AcceptedAt.IsZero() || params.DailySpendCap.Int64() < 0 || !params.Price.Version.Valid() || params.Price.OwnerShareBPS > 10_000 {
		return ErrInvalidOperation
	}
	_, err := CalculateQuote(params.InputTokens, 0, params.MaxOutputTokens, params.Price, false)
	return err
}

func sameRequestParams(request RequestCheckpoint, params ReserveParams) bool {
	return request.AccountID == params.AccountID && request.APIKeyID == params.APIKeyID && request.ModelID == params.ModelID && request.InputTokens == params.InputTokens &&
		request.MaxOutputTokens == params.MaxOutputTokens && request.DailySpendCap.Int64() == params.DailySpendCap.Int64() &&
		samePrice(request.Price, params.Price)
}

func reservationMatches(reservation Reservation, params ReserveParams) bool {
	return reservation.ModelID == params.ModelID && reservation.AccountID == params.AccountID && reservation.APIKeyID == params.APIKeyID && reservation.OwnerID == params.OwnerID &&
		reservation.InputTokens == params.InputTokens && reservation.MaxOutputTokens == params.MaxOutputTokens && samePrice(reservation.Price, params.Price)
}

func terminalAttemptStatus(cancelled bool) AttemptStatus {
	if cancelled {
		return AttemptCancelled
	}
	return AttemptSettled
}

func (j *RedisJournal) Operation(ctx context.Context, operationID string) (Entry, error) {
	if operationID == "" || len(operationID) > 160 {
		return Entry{}, ErrInvalidOperation
	}
	encoded, err := j.client.HGet(ctx, j.operationsKey, "entry:"+operationID).Result()
	if err == redis.Nil {
		return Entry{}, ErrRequestNotFound
	}
	if err != nil {
		return Entry{}, j.markUncertain(err)
	}
	var entry Entry
	if json.Unmarshal([]byte(encoded), &entry) != nil || entry.OperationID != operationID {
		return Entry{}, j.markUncertain(ErrInvalidSnapshot)
	}
	hash, err := hashEntry(entry)
	if err != nil || hash != entry.EntryHash {
		return Entry{}, j.markUncertain(ErrInvalidSnapshot)
	}
	return entry, nil
}

func (l *RedisLedger) Recover(ctx context.Context, at time.Time) (int, error) {
	return l.journal.Recover(ctx, at)
}

func (l *RedisLedger) Ready() bool { return l.journal.Ready() }

func (l *RedisLedger) Request(ctx context.Context, requestID string) (RequestCheckpoint, uint64, error) {
	return l.journal.Request(ctx, requestID)
}

func (l *RedisLedger) PendingUsageRequestIDs(ctx context.Context) ([]string, error) {
	return l.journal.PendingUsageRequestIDs(ctx)
}

func (l *RedisLedger) Operation(ctx context.Context, operationID string) (Entry, error) {
	return l.journal.Operation(ctx, operationID)
}
