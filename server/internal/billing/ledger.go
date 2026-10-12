// Package billing contains the checked request-accounting core. The ledger in
// this file is an in-memory reference implementation; production persistence
// and Redis durability barriers are provided by a separate store adapter.
package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cordova/server/internal/domain"
)

var (
	ErrInvalidOperation           = errors.New("invalid ledger operation")
	ErrInsufficientFunds          = errors.New("insufficient funds")
	ErrSpendCapExceeded           = errors.New("daily spend cap exceeded")
	ErrVerificationBudgetExceeded = errors.New("daily verification budget exceeded")
	ErrConflict                   = errors.New("request accounting conflict")
	ErrAlreadyTerminal            = errors.New("request is already terminal")
	ErrRetryUnavailable           = errors.New("request retry is unavailable")
)

const (
	BucketAvailable            = "user.available"
	BucketReserved             = "user.reserved"
	BucketOwnerPending         = "owner.pending"
	BucketOwnerAvailable       = "owner.available"
	BucketPayoutReserved       = "owner.payout_reserved"
	BucketCashHot              = "cash.hot"
	BucketEquity               = "equity"
	BucketNetworkFee           = "network_fee_expense"
	BucketVerificationExpense  = "verification_expense"
	BucketVerificationReserved = "platform.verification_reserved"
	maxAttempts                = 2
	maxRequestInputTokens      = 1_000_000
	maxRequestOutputTokens     = 2_048
	defaultModelID             = "cordova-chat-small-v1"
)

// Price is copied into a request at acceptance and remains fixed across retries.
type Price struct {
	Version       domain.ID       `json:"version"`
	InputRate     domain.Piconero `json:"input_rate_piconero_per_million"`
	OutputRate    domain.Piconero `json:"output_rate_piconero_per_million"`
	OwnerShareBPS uint32          `json:"owner_share_bps"`
}

type Quote struct {
	InputCost    domain.Piconero `json:"input_cost_piconero"`
	OutputCost   domain.Piconero `json:"output_cost_piconero"`
	Reservation  domain.Piconero `json:"reservation_piconero"`
	ActualCost   domain.Piconero `json:"actual_cost_piconero"`
	OwnerCredit  domain.Piconero `json:"owner_credit_piconero"`
	PlatformTake domain.Piconero `json:"platform_take_piconero"`
	Refund       domain.Piconero `json:"refund_piconero"`
}

// CalculateQuote implements the C5 integer ceiling/floor rules. Cancellation
// with no accepted output is free, including the input-token charge.
func CalculateQuote(inputTokens, outputTokens, maxOutputTokens int64, price Price, cancelled bool) (Quote, error) {
	if inputTokens < 0 || outputTokens < 0 || maxOutputTokens < 0 || outputTokens > maxOutputTokens ||
		!price.Version.Valid() || price.OwnerShareBPS > 10_000 {
		return Quote{}, ErrInvalidOperation
	}
	inputCost, err := domain.CostForTokens(inputTokens, price.InputRate)
	if err != nil {
		return Quote{}, err
	}
	maxOutputCost, err := domain.CostForTokens(maxOutputTokens, price.OutputRate)
	if err != nil {
		return Quote{}, err
	}
	reservation, err := inputCost.Add(maxOutputCost)
	if err != nil {
		return Quote{}, err
	}
	if cancelled && outputTokens == 0 {
		return Quote{Reservation: reservation, Refund: reservation}, nil
	}
	outputCost, err := domain.CostForTokens(outputTokens, price.OutputRate)
	if err != nil {
		return Quote{}, err
	}
	actual, err := inputCost.Add(outputCost)
	if err != nil || actual.Int64() > reservation.Int64() {
		return Quote{}, domain.ErrPiconeroOverflow
	}
	owner, err := domain.OwnerShare(actual, price.OwnerShareBPS)
	if err != nil {
		return Quote{}, err
	}
	platform, err := actual.Sub(owner)
	if err != nil {
		return Quote{}, err
	}
	refund, err := reservation.Sub(actual)
	if err != nil {
		return Quote{}, err
	}
	return Quote{InputCost: inputCost, OutputCost: outputCost, Reservation: reservation,
		ActualCost: actual, OwnerCredit: owner, PlatformTake: platform, Refund: refund}, nil
}

type Posting struct {
	Bucket    string `json:"bucket"`
	AccountID string `json:"account_id"`
	Delta     string `json:"delta_piconero"`
}

type Entry struct {
	ID                 string                    `json:"ledger_id"`
	OperationID        string                    `json:"operation_id"`
	Kind               string                    `json:"kind"`
	RequestID          string                    `json:"request_id,omitempty"`
	AttemptID          string                    `json:"attempt_id,omitempty"`
	PriceVersion       string                    `json:"price_version,omitempty"`
	CreatedAt          time.Time                 `json:"created_at"`
	Postings           []Posting                 `json:"postings"`
	ReferenceID        string                    `json:"reference_id,omitempty"`
	VerificationBudget *VerificationBudgetChange `json:"verification_budget,omitempty"`
	PreviousHash       string                    `json:"previous_hash"`
	EntryHash          string                    `json:"entry_hash"`
}

type RequestStatus string

const (
	RequestAccepted  RequestStatus = "accepted"
	RequestRunning   RequestStatus = "running"
	RequestSettled   RequestStatus = "settled"
	RequestRefunded  RequestStatus = "refunded"
	RequestCancelled RequestStatus = "cancelled"
)

type AttemptStatus string

const (
	AttemptReserved  AttemptStatus = "reserved"
	AttemptSettled   AttemptStatus = "settled"
	AttemptRefunded  AttemptStatus = "refunded"
	AttemptCancelled AttemptStatus = "cancelled"
)

type ReserveParams struct {
	RequestID       string
	AttemptID       string
	ModelID         string
	AccountID       string
	APIKeyID        string
	OwnerID         string
	InputTokens     int64
	MaxOutputTokens int64
	DailySpendCap   domain.Piconero
	Price           Price
	AcceptedAt      time.Time
}

type Reservation struct {
	RequestID       string        `json:"request_id"`
	AttemptID       string        `json:"attempt_id"`
	ModelID         string        `json:"model_id"`
	AccountID       string        `json:"account_id"`
	APIKeyID        string        `json:"api_key_id"`
	OwnerID         string        `json:"owner_id"`
	InputTokens     int64         `json:"input_tokens"`
	MaxOutputTokens int64         `json:"max_output_tokens"`
	DailySpendCap   string        `json:"daily_spend_cap_piconero"`
	Price           Price         `json:"price"`
	AcceptedDay     string        `json:"accepted_day"`
	Amount          string        `json:"reserved_piconero"`
	Status          AttemptStatus `json:"status"`
}

type Balance struct {
	Available      domain.Piconero `json:"available_piconero"`
	Reserved       domain.Piconero `json:"reserved_piconero"`
	OwnerPending   domain.Piconero `json:"earnings_pending_piconero"`
	OwnerAvailable domain.Piconero `json:"earnings_available_piconero"`
	PayoutReserved domain.Piconero `json:"payout_reserved_piconero"`
}

type capState struct {
	Limit    int64
	Spent    int64
	Reserved int64
}

type attemptState struct {
	Reservation  Reservation
	OutputTokens int64
}

type requestState struct {
	AccountID       string
	APIKeyID        string
	ModelID         string
	OwnerID         string
	InputTokens     int64
	MaxOutputTokens int64
	DailySpendCap   int64
	AcceptedDay     string
	AcceptedAt      time.Time
	Price           Price
	Status          RequestStatus
	Attempts        map[string]*attemptState
}

// Ledger serializes operations and serves as the deterministic reference model
// for a future durable Store implementation. It never receives prompt text.
type Ledger struct {
	mu             sync.Mutex
	entries        []Entry
	operations     map[string]Entry
	balances       map[string]int64
	requests       map[string]*requestState
	caps           map[string]capState
	opFingerprints map[string]string
}

func NewLedger() *Ledger {
	return &Ledger{operations: make(map[string]Entry), balances: make(map[string]int64),
		requests: make(map[string]*requestState), caps: make(map[string]capState),
		opFingerprints: make(map[string]string)}
}

// CreditTest funds an account from test equity. It is not a deposit API.
func (l *Ledger) CreditTest(accountID, operationID string, amount domain.Piconero, at time.Time) (Entry, error) {
	if !validID(accountID) || operationID == "" || amount.Int64() <= 0 || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fingerprint := hashFingerprint(fmt.Sprintf("test-credit:%s:%s:%d", accountID, at.UTC().Format(time.RFC3339Nano), amount.Int64()))
	if prior, ok, err := l.idempotent(operationID, fingerprint); ok || err != nil {
		return prior, err
	}
	return l.commit(Entry{OperationID: operationID, Kind: "test_credit", CreatedAt: at.UTC(), Postings: []Posting{
		posting(BucketCashHot, "", amount.Int64()), posting(BucketAvailable, accountID, -amount.Int64()),
	}}, operationID, fingerprint)
}

func (l *Ledger) Reserve(p ReserveParams) (Reservation, Entry, error) {
	if !validID(p.RequestID) || !validID(p.AttemptID) || !validID(p.AccountID) || !validID(p.APIKeyID) || !validID(p.OwnerID) || !validModelID(p.ModelID) ||
		p.InputTokens < 0 || p.InputTokens > maxRequestInputTokens || p.MaxOutputTokens < 1 || p.MaxOutputTokens > maxRequestOutputTokens ||
		p.AcceptedAt.IsZero() || p.DailySpendCap.Int64() < 0 {
		return Reservation{}, Entry{}, ErrInvalidOperation
	}
	quote, err := CalculateQuote(p.InputTokens, 0, p.MaxOutputTokens, p.Price, false)
	if err != nil {
		return Reservation{}, Entry{}, err
	}
	amount := quote.Reservation.Int64()
	op := "reserve:" + p.RequestID + ":" + p.AttemptID
	l.mu.Lock()
	defer l.mu.Unlock()
	day := p.AcceptedAt.UTC().Format("2006-01-02")
	state := cloneRequest(l.requests[p.RequestID])
	capLimit := p.DailySpendCap.Int64()
	if state != nil {
		// A retry uses the original acceptance-day cap bucket and cap snapshot,
		// including when the retry crosses UTC midnight.
		day = state.AcceptedDay
		capLimit = state.DailySpendCap
		p.AcceptedAt = state.AcceptedAt
	}
	p.DailySpendCap, _ = domain.NewPiconero(capLimit)
	fingerprint := reserveFingerprint(p, day, amount)
	if prior, ok, err := l.idempotent(op, fingerprint); ok || err != nil {
		if err != nil {
			return Reservation{}, prior, err
		}
		if state := l.requests[p.RequestID]; state != nil {
			if attempt := state.Attempts[p.AttemptID]; attempt != nil {
				return attempt.Reservation, prior, nil
			}
		}
		return Reservation{}, prior, ErrConflict
	}
	if state == nil {
		state = &requestState{AccountID: p.AccountID, APIKeyID: p.APIKeyID, ModelID: p.ModelID, OwnerID: p.OwnerID,
			InputTokens: p.InputTokens, MaxOutputTokens: p.MaxOutputTokens, DailySpendCap: p.DailySpendCap.Int64(),
			AcceptedDay: day, AcceptedAt: p.AcceptedAt.UTC(), Price: p.Price, Status: RequestAccepted, Attempts: make(map[string]*attemptState)}
	} else if state.Status != RequestAccepted || state.AccountID != p.AccountID || state.APIKeyID != p.APIKeyID || state.ModelID != p.ModelID ||
		state.OwnerID == p.OwnerID || state.InputTokens != p.InputTokens || state.MaxOutputTokens != p.MaxOutputTokens ||
		!samePrice(state.Price, p.Price) || len(state.Attempts) >= maxAttempts || state.Attempts[p.AttemptID] != nil {
		return Reservation{}, Entry{}, ErrRetryUnavailable
	}
	state.OwnerID = p.OwnerID
	userAvailable, err := liabilityBalance(l.balances, BucketAvailable, p.AccountID)
	if err != nil {
		return Reservation{}, Entry{}, err
	}
	if userAvailable < amount {
		return Reservation{}, Entry{}, ErrInsufficientFunds
	}
	capKey := p.AccountID + ":" + p.APIKeyID + ":" + day
	cap, hasCap := l.caps[capKey]
	if !hasCap || p.DailySpendCap.Int64() < cap.Limit {
		cap.Limit = p.DailySpendCap.Int64()
	}
	if cap.Spent > cap.Limit-cap.Reserved || amount > cap.Limit-cap.Spent-cap.Reserved {
		return Reservation{}, Entry{}, ErrSpendCapExceeded
	}
	cap.Reserved += amount
	reservation := Reservation{RequestID: p.RequestID, AttemptID: p.AttemptID, ModelID: p.ModelID, AccountID: p.AccountID, APIKeyID: p.APIKeyID, OwnerID: p.OwnerID,
		InputTokens: p.InputTokens, MaxOutputTokens: p.MaxOutputTokens, DailySpendCap: strconv.FormatInt(capLimit, 10), Price: p.Price,
		AcceptedDay: day, Amount: strconv.FormatInt(amount, 10), Status: AttemptReserved}
	if state.Attempts == nil {
		state.Attempts = make(map[string]*attemptState)
	}
	state.Attempts[p.AttemptID] = &attemptState{Reservation: reservation}
	state.Status = RequestRunning
	entry, err := l.commit(Entry{OperationID: op, Kind: "reserve", RequestID: p.RequestID, AttemptID: p.AttemptID,
		PriceVersion: p.Price.Version.String(), CreatedAt: p.AcceptedAt.UTC(), Postings: []Posting{
			posting(BucketAvailable, p.AccountID, amount), posting(BucketReserved, p.AccountID, -amount),
		}}, op, fingerprint)
	if err != nil {
		return Reservation{}, Entry{}, err
	}
	l.requests[p.RequestID] = state
	l.caps[capKey] = cap
	return reservation, entry, nil
}

// FailAttempt releases an attempt in full. retryAllowed leaves the logical
// request accepted only when a second, distinct-owner attempt is still legal.
func (l *Ledger) FailAttempt(requestID, attemptID string, retryAllowed bool, at time.Time) (Entry, error) {
	return l.finishAttempt(requestID, attemptID, 0, false, retryAllowed, "failure", at)
}

// Settle records successful output. Cancelled output charges only a verified
// accepted prefix; a cancellation with no output accepted is free.
func (l *Ledger) Settle(requestID, attemptID string, outputTokens int64, cancelled bool, at time.Time) (Entry, error) {
	return l.finishAttempt(requestID, attemptID, outputTokens, cancelled, false, "settle", at)
}

func (l *Ledger) finishAttempt(requestID, attemptID string, outputTokens int64, cancelled, retryAllowed bool, mode string, at time.Time) (Entry, error) {
	if !validID(requestID) || !validID(attemptID) || outputTokens < 0 || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	operationID, kind := attemptOperation(requestID, attemptID, outputTokens, cancelled, retryAllowed, mode)
	fingerprint := hashFingerprint(fmt.Sprintf("%s:%d:%t:%t:%s", operationID, outputTokens, cancelled, retryAllowed, mode))
	if prior, ok, err := l.idempotent(operationID, fingerprint); ok || err != nil {
		return prior, err
	}
	state := cloneRequest(l.requests[requestID])
	if state == nil {
		return Entry{}, ErrConflict
	}
	if state.Status == RequestSettled || state.Status == RequestRefunded || state.Status == RequestCancelled {
		return Entry{}, ErrAlreadyTerminal
	}
	attempt := state.Attempts[attemptID]
	if attempt == nil || attempt.Reservation.Status != AttemptReserved {
		return Entry{}, ErrConflict
	}
	if outputTokens > state.MaxOutputTokens {
		return Entry{}, ErrInvalidOperation
	}
	reservation, err := domain.ParsePiconero(attempt.Reservation.Amount)
	if err != nil {
		return Entry{}, err
	}
	capKey := state.AccountID + ":" + state.APIKeyID + ":" + state.AcceptedDay
	cap := l.caps[capKey]
	if cap.Reserved < reservation.Int64() {
		return Entry{}, ErrConflict
	}
	var postings []Posting
	var actualCost int64
	newStatus := state.Status
	if mode == "failure" {
		if retryAllowed && len(state.Attempts) >= maxAttempts {
			return Entry{}, ErrRetryUnavailable
		}
		kind = "attempt_refund"
		operationID = "refund:" + requestID + ":" + attemptID
		attempt.Reservation.Status = AttemptRefunded
		if retryAllowed {
			newStatus = RequestAccepted
		} else {
			newStatus = RequestRefunded
		}
		postings = []Posting{posting(BucketReserved, state.AccountID, reservation.Int64()), posting(BucketAvailable, state.AccountID, -reservation.Int64())}
		cap.Reserved -= reservation.Int64()
	} else {
		quote, quoteErr := CalculateQuote(state.InputTokens, outputTokens, state.MaxOutputTokens, state.Price, cancelled)
		if quoteErr != nil {
			return Entry{}, quoteErr
		}
		if quote.Reservation.Int64() != reservation.Int64() || quote.ActualCost.Int64() > reservation.Int64() {
			return Entry{}, ErrConflict
		}
		if cancelled {
			kind = "cancel"
			operationID = "cancel:" + requestID
			newStatus = RequestCancelled
			attempt.Reservation.Status = AttemptCancelled
		} else {
			kind = "settle"
			operationID = "settle:" + requestID
			newStatus = RequestSettled
			attempt.Reservation.Status = AttemptSettled
		}
		attempt.OutputTokens = outputTokens
		postings = []Posting{posting(BucketReserved, state.AccountID, reservation.Int64()),
			posting(BucketAvailable, state.AccountID, -quote.Refund.Int64()),
			posting(BucketOwnerPending, state.OwnerID, -quote.OwnerCredit.Int64()),
			posting(BucketEquity, "", -quote.PlatformTake.Int64())}
		cap.Reserved -= reservation.Int64()
		cap.Spent += quote.ActualCost.Int64()
		actualCost = quote.ActualCost.Int64()
	}
	if cap.Reserved < 0 || cap.Spent > cap.Limit || cap.Reserved > cap.Limit-cap.Spent {
		return Entry{}, ErrConflict
	}
	state.Status = newStatus
	entry := Entry{OperationID: operationID, Kind: kind, RequestID: requestID, AttemptID: attemptID,
		PriceVersion: state.Price.Version.String(), CreatedAt: at.UTC(), Postings: postings}
	if actualCost > 0 {
		entry.VerificationBudget = &VerificationBudgetChange{Day: at.UTC().Format("2006-01-02"),
			GrossRevenueDelta: signedAmount(actualCost), OperatorSeedDelta: "0", SpentDelta: "0", ReservedDelta: "0"}
	}
	entry, err = l.commit(entry, operationID, fingerprint)
	if err != nil {
		return Entry{}, err
	}
	l.requests[requestID] = state
	l.caps[capKey] = cap
	return entry, nil
}

// Recover refunds all reservations left active by an interrupted process.
// Callers must fence node sessions before invoking it.
func (l *Ledger) Recover(at time.Time) (int, error) {
	if at.IsZero() {
		return 0, ErrInvalidOperation
	}
	l.mu.Lock()
	ids := make([]string, 0, len(l.requests))
	for id := range l.requests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type orphan struct{ request, attempt string }
	var orphans []orphan
	var accepted []string
	for _, id := range ids {
		state := l.requests[id]
		if state.Status == RequestSettled || state.Status == RequestRefunded || state.Status == RequestCancelled {
			continue
		}
		if state.Status == RequestAccepted {
			accepted = append(accepted, id)
			continue
		}
		for attemptID, attempt := range state.Attempts {
			if attempt.Reservation.Status == AttemptReserved {
				orphans = append(orphans, orphan{id, attemptID})
			}
		}
	}
	l.mu.Unlock()
	recovered := 0
	for _, o := range orphans {
		if _, err := l.FailAttempt(o.request, o.attempt, false, at); err != nil {
			if errors.Is(err, ErrAlreadyTerminal) {
				continue
			}
			return recovered, err
		}
		recovered++
	}
	for _, requestID := range accepted {
		if _, err := l.abandonAccepted(requestID, at); err != nil {
			if errors.Is(err, ErrAlreadyTerminal) {
				continue
			}
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

// AbandonRetry closes a request after an attempt was refunded for retry but no
// eligible second owner remained. The operation is durable metadata and does
// not alter balances or spend-cap counters.
func (l *Ledger) AbandonRetry(requestID string, at time.Time) (Entry, error) {
	return l.abandonAccepted(requestID, at)
}

func (l *Ledger) abandonAccepted(requestID string, at time.Time) (Entry, error) {
	if !validID(requestID) || at.IsZero() {
		return Entry{}, ErrInvalidOperation
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state := cloneRequest(l.requests[requestID])
	if state == nil {
		return Entry{}, ErrConflict
	}
	if state.Status == RequestRefunded {
		if prior, ok := l.operations["abandon:"+requestID]; ok {
			return prior, nil
		}
		return Entry{}, ErrAlreadyTerminal
	}
	if state.Status != RequestAccepted || len(state.Attempts) == 0 {
		return Entry{}, ErrAlreadyTerminal
	}
	for _, attempt := range state.Attempts {
		if attempt.Reservation.Status != AttemptRefunded || attempt.OutputTokens != 0 {
			return Entry{}, ErrConflict
		}
	}
	var attemptID string
	for id, attempt := range state.Attempts {
		if attempt.Reservation.OwnerID == state.OwnerID {
			if attemptID != "" {
				return Entry{}, ErrConflict
			}
			attemptID = id
		}
	}
	if attemptID == "" {
		return Entry{}, ErrConflict
	}
	operationID := "abandon:" + requestID
	fingerprint := hashFingerprint("abandon:" + requestID)
	if prior, ok, err := l.idempotent(operationID, fingerprint); ok || err != nil {
		return prior, err
	}
	state.Status = RequestRefunded
	entry, err := l.commit(Entry{OperationID: operationID, Kind: "request_abandoned", RequestID: requestID,
		AttemptID: attemptID, PriceVersion: state.Price.Version.String(), CreatedAt: at.UTC(), Postings: []Posting{
			posting(BucketAvailable, state.AccountID, 0), posting(BucketReserved, state.AccountID, 0),
		}}, operationID, fingerprint)
	if err != nil {
		return Entry{}, err
	}
	l.requests[requestID] = state
	return entry, nil
}

func (l *Ledger) Balance(accountID string) (Balance, error) {
	if !validID(accountID) {
		return Balance{}, ErrInvalidOperation
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	values := []string{BucketAvailable, BucketReserved, BucketOwnerPending, BucketOwnerAvailable, BucketPayoutReserved}
	amounts := make([]domain.Piconero, len(values))
	for i, bucket := range values {
		amount, err := liabilityBalance(l.balances, bucket, accountID)
		if err != nil {
			return Balance{}, err
		}
		p, err := domain.NewPiconero(amount)
		if err != nil {
			return Balance{}, err
		}
		amounts[i] = p
	}
	return Balance{Available: amounts[0], Reserved: amounts[1], OwnerPending: amounts[2], OwnerAvailable: amounts[3], PayoutReserved: amounts[4]}, nil
}

func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	copyEntries := make([]Entry, len(l.entries))
	copy(copyEntries, l.entries)
	for i := range copyEntries {
		copyEntries[i].Postings = append([]Posting(nil), copyEntries[i].Postings...)
	}
	return copyEntries
}

func (l *Ledger) idempotent(operationID, fingerprint string) (Entry, bool, error) {
	prior, ok := l.operations[operationID]
	if !ok {
		return Entry{}, false, nil
	}
	if l.opFingerprints[operationID] != fingerprint {
		return prior, true, ErrConflict
	}
	return prior, true, nil
}

func (l *Ledger) commit(entry Entry, operationID, fingerprint string) (Entry, error) {
	if _, exists := l.operations[operationID]; exists {
		return Entry{}, ErrConflict
	}
	if operationID == "" || entry.Kind == "" || len(entry.Postings) < 2 {
		return Entry{}, ErrInvalidOperation
	}
	if err := validateVerificationBudgetChange(entry); err != nil {
		return Entry{}, err
	}
	sort.Slice(entry.Postings, func(i, j int) bool {
		if entry.Postings[i].Bucket == entry.Postings[j].Bucket {
			return entry.Postings[i].AccountID < entry.Postings[j].AccountID
		}
		return entry.Postings[i].Bucket < entry.Postings[j].Bucket
	})
	var total big.Int
	newBalances := make(map[string]int64, len(entry.Postings))
	for _, p := range entry.Postings {
		if !validBucketAccount(p.Bucket, p.AccountID) {
			return Entry{}, ErrInvalidOperation
		}
		delta, err := parseSigned(p.Delta)
		if err != nil {
			return Entry{}, err
		}
		total.Add(&total, big.NewInt(delta))
		key := balanceKey(p.Bucket, p.AccountID)
		current := l.balances[key]
		if _, ok := newBalances[key]; ok {
			current = newBalances[key]
		}
		next, ok := checkedAdd(current, delta)
		if !ok || next > domain.MaxPiconero || next < -domain.MaxPiconero {
			return Entry{}, domain.ErrPiconeroOverflow
		}
		if isLiabilityBucket(p.Bucket) && next > 0 {
			return Entry{}, ErrInsufficientFunds
		}
		newBalances[key] = next
	}
	if total.Sign() != 0 {
		return Entry{}, ErrInvalidOperation
	}
	id, err := domain.NewID()
	if err != nil {
		return Entry{}, err
	}
	entry.ID = id.String()
	entry.OperationID = operationID
	entry.CreatedAt = entry.CreatedAt.UTC()
	if len(l.entries) > 0 {
		entry.PreviousHash = l.entries[len(l.entries)-1].EntryHash
	}
	entry.EntryHash, err = hashEntry(entry)
	if err != nil {
		return Entry{}, err
	}
	for key, value := range newBalances {
		l.balances[key] = value
	}
	l.entries = append(l.entries, entry)
	l.operations[operationID] = entry
	l.opFingerprints[operationID] = fingerprint
	return entry, nil
}

func posting(bucket, account string, delta int64) Posting {
	return Posting{Bucket: bucket, AccountID: account, Delta: strconv.FormatInt(delta, 10)}
}

func negativeAmount(value domain.Piconero) string {
	if value.Int64() == 0 {
		return "0"
	}
	return "-" + value.String()
}

func hashEntry(entry Entry) (string, error) {
	entry.EntryHash = ""
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func reserveFingerprint(p ReserveParams, day string, amount int64) string {
	encoded, _ := json.Marshal(struct {
		RequestID, AttemptID, ModelID, AccountID, APIKeyID, OwnerID string
		InputTokens, MaxOutputTokens, DailySpendCap, Amount         int64
		Price                                                       Price
		AcceptedDay                                                 string
	}{p.RequestID, p.AttemptID, p.ModelID, p.AccountID, p.APIKeyID, p.OwnerID, p.InputTokens, p.MaxOutputTokens, p.DailySpendCap.Int64(), amount, p.Price, day})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func attemptOperation(requestID, attemptID string, outputTokens int64, cancelled, retryAllowed bool, mode string) (string, string) {
	if retryAllowed || mode == "failure" {
		return "refund:" + requestID + ":" + attemptID, "attempt_refund"
	}
	if cancelled {
		return "cancel:" + requestID, "cancel"
	}
	return "settle:" + requestID, "settle"
}

func cloneRequest(source *requestState) *requestState {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Attempts = make(map[string]*attemptState, len(source.Attempts))
	for id, attempt := range source.Attempts {
		item := *attempt
		clone.Attempts[id] = &item
	}
	return &clone
}

func samePrice(a, b Price) bool {
	return a.Version == b.Version && a.InputRate.Int64() == b.InputRate.Int64() && a.OutputRate.Int64() == b.OutputRate.Int64() && a.OwnerShareBPS == b.OwnerShareBPS
}

func validID(value string) bool { _, err := domain.ParseID(value); return err == nil }

func validModelID(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func hashFingerprint(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func parseSigned(value string) (int64, error) {
	if value == "" || value == "-0" || strings.HasPrefix(value, "+") || (len(value) > 1 && value[0] == '0') || (len(value) > 2 && strings.HasPrefix(value, "-0")) {
		return 0, ErrInvalidOperation
	}
	for i, r := range value {
		if i == 0 && r == '-' {
			continue
		}
		if r < '0' || r > '9' {
			return 0, ErrInvalidOperation
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n > domain.MaxPiconero || n < -domain.MaxPiconero {
		return 0, ErrInvalidOperation
	}
	return n, nil
}

func liabilityBalance(balances map[string]int64, bucket, account string) (int64, error) {
	posted := balances[balanceKey(bucket, account)]
	if posted == math.MinInt64 {
		return 0, domain.ErrPiconeroOverflow
	}
	value := -posted
	if value < 0 || value > domain.MaxPiconero {
		return 0, domain.ErrInvalidPiconero
	}
	return value, nil
}

func balanceKey(bucket, account string) string { return bucket + "\x00" + account }

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}
