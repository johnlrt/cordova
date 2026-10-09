package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"cordova/server/internal/domain"
)

var ErrInvalidSnapshot = errors.New("ledger recovery snapshot is invalid")

type AttemptCheckpoint struct {
	Reservation  Reservation `json:"reservation"`
	OutputTokens int64       `json:"output_tokens"`
}

type RequestCheckpoint struct {
	AccountID       string                       `json:"account_id"`
	APIKeyID        string                       `json:"api_key_id"`
	ModelID         string                       `json:"model_id"`
	OwnerID         string                       `json:"owner_id"`
	InputTokens     int64                        `json:"input_tokens"`
	MaxOutputTokens int64                        `json:"max_output_tokens"`
	DailySpendCap   domain.Piconero              `json:"daily_spend_cap_piconero"`
	AcceptedDay     string                       `json:"accepted_day"`
	AcceptedAt      time.Time                    `json:"accepted_at"`
	Price           Price                        `json:"price"`
	Status          RequestStatus                `json:"status"`
	Attempts        map[string]AttemptCheckpoint `json:"attempts"`
}

type CapCheckpoint struct {
	Limit    domain.Piconero `json:"limit_piconero"`
	Spent    domain.Piconero `json:"spent_piconero"`
	Reserved domain.Piconero `json:"reserved_piconero"`
}

// LedgerSnapshot is a metadata-only checkpoint for restart/replay testing. A
// production Redis store must write the checkpoint and its stream operation
// atomically and still retain the append-only operation tombstones.
type LedgerSnapshot struct {
	Version      int                          `json:"version"`
	Entries      []Entry                      `json:"entries"`
	Requests     map[string]RequestCheckpoint `json:"requests"`
	Caps         map[string]CapCheckpoint     `json:"caps"`
	Fingerprints map[string]string            `json:"operation_fingerprints"`
	Checksum     string                       `json:"checksum"`
}

func (l *Ledger) Snapshot() LedgerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := LedgerSnapshot{Version: 1, Entries: copyLedgerEntries(l.entries),
		Requests:     make(map[string]RequestCheckpoint, len(l.requests)),
		Caps:         make(map[string]CapCheckpoint, len(l.caps)),
		Fingerprints: make(map[string]string, len(l.opFingerprints))}
	for requestID, state := range l.requests {
		dailyCap, _ := domain.NewPiconero(state.DailySpendCap)
		request := RequestCheckpoint{AccountID: state.AccountID, APIKeyID: state.APIKeyID, ModelID: state.ModelID, OwnerID: state.OwnerID,
			InputTokens: state.InputTokens, MaxOutputTokens: state.MaxOutputTokens, DailySpendCap: dailyCap,
			AcceptedDay: state.AcceptedDay, AcceptedAt: state.AcceptedAt, Price: state.Price, Status: state.Status,
			Attempts: make(map[string]AttemptCheckpoint, len(state.Attempts))}
		for attemptID, attempt := range state.Attempts {
			request.Attempts[attemptID] = AttemptCheckpoint{Reservation: attempt.Reservation, OutputTokens: attempt.OutputTokens}
		}
		snapshot.Requests[requestID] = request
	}
	for key, cap := range l.caps {
		limit, _ := domain.NewPiconero(cap.Limit)
		spent, _ := domain.NewPiconero(cap.Spent)
		reserved, _ := domain.NewPiconero(cap.Reserved)
		snapshot.Caps[key] = CapCheckpoint{Limit: limit, Spent: spent, Reserved: reserved}
	}
	for operationID, fingerprint := range l.opFingerprints {
		snapshot.Fingerprints[operationID] = fingerprint
	}
	snapshot.Checksum = checksumSnapshot(snapshot)
	return snapshot
}

// Restore verifies the journal hash chain, balanced postings, snapshot
// checksum, terminal request states, and cap totals before making a ledger
// usable again. The caller must fence old node sessions before this step.
func Restore(snapshot LedgerSnapshot) (*Ledger, error) {
	if snapshot.Version != 1 || snapshot.Checksum == "" || snapshot.Checksum != checksumSnapshot(snapshot) {
		return nil, ErrInvalidSnapshot
	}
	l := NewLedger()
	previous := ""
	for _, entry := range snapshot.Entries {
		if !domainID(entry.ID) || entry.OperationID == "" || entry.Kind == "" || entry.CreatedAt.IsZero() ||
			entry.PreviousHash != previous || len(entry.Postings) < 2 {
			return nil, ErrInvalidSnapshot
		}
		if _, exists := l.operations[entry.OperationID]; exists {
			return nil, ErrInvalidSnapshot
		}
		computed, err := hashEntry(entry)
		if err != nil || computed != entry.EntryHash {
			return nil, ErrInvalidSnapshot
		}
		var postingTotal big.Int
		for _, item := range entry.Postings {
			if !validBucketAccount(item.Bucket, item.AccountID) {
				return nil, ErrInvalidSnapshot
			}
			delta, err := parseSigned(item.Delta)
			if err != nil {
				return nil, ErrInvalidSnapshot
			}
			postingTotal.Add(&postingTotal, big.NewInt(delta))
			key := balanceKey(item.Bucket, item.AccountID)
			next, ok := checkedAdd(l.balances[key], delta)
			if !ok || next > domain.MaxPiconero || next < -domain.MaxPiconero || isLiabilityBucket(item.Bucket) && next > 0 {
				return nil, ErrInvalidSnapshot
			}
			l.balances[key] = next
		}
		if postingTotal.Sign() != 0 {
			return nil, ErrInvalidSnapshot
		}
		fingerprint := snapshot.Fingerprints[entry.OperationID]
		decodedFingerprint, fingerprintErr := hex.DecodeString(fingerprint)
		if fingerprintErr != nil || len(decodedFingerprint) != sha256.Size || hex.EncodeToString(decodedFingerprint) != fingerprint {
			return nil, ErrInvalidSnapshot
		}
		l.entries = append(l.entries, cloneEntry(entry))
		l.operations[entry.OperationID] = cloneEntry(entry)
		l.opFingerprints[entry.OperationID] = fingerprint
		previous = entry.EntryHash
	}
	if len(snapshot.Fingerprints) != len(l.operations) {
		return nil, ErrInvalidSnapshot
	}
	for requestID, checkpoint := range snapshot.Requests {
		state, err := restoreRequestCheckpoint(requestID, checkpoint)
		if err != nil {
			return nil, err
		}
		for attemptID, attempt := range state.Attempts {
			if l.operations["reserve:"+requestID+":"+attemptID].ID == "" {
				return nil, ErrInvalidSnapshot
			}
			switch attempt.Reservation.Status {
			case AttemptReserved:
				if l.operations["refund:"+requestID+":"+attemptID].ID != "" || l.operations["settle:"+requestID].ID != "" || l.operations["cancel:"+requestID].ID != "" {
					return nil, ErrInvalidSnapshot
				}
			case AttemptRefunded:
				if l.operations["refund:"+requestID+":"+attemptID].ID == "" {
					return nil, ErrInvalidSnapshot
				}
			case AttemptSettled:
				if l.operations["settle:"+requestID].ID == "" {
					return nil, ErrInvalidSnapshot
				}
			case AttemptCancelled:
				if l.operations["cancel:"+requestID].ID == "" {
					return nil, ErrInvalidSnapshot
				}
			}
		}
		abandon, hasAbandon := l.operations["abandon:"+requestID]
		if hasAbandon && (checkpoint.Status != RequestRefunded || abandon.Kind != "request_abandoned" || abandon.RequestID != requestID) {
			return nil, ErrInvalidSnapshot
		}
		l.requests[requestID] = state
	}
	for key, cap := range snapshot.Caps {
		if cap.Spent.Int64() > cap.Limit.Int64() || cap.Reserved.Int64() > cap.Limit.Int64()-cap.Spent.Int64() {
			return nil, ErrInvalidSnapshot
		}
		l.caps[key] = capState{Limit: cap.Limit.Int64(), Spent: cap.Spent.Int64(), Reserved: cap.Reserved.Int64()}
	}
	if !capsMatchRequests(l.requests, l.caps) {
		return nil, ErrInvalidSnapshot
	}
	return l, nil
}

func restoreRequestCheckpoint(requestID string, checkpoint RequestCheckpoint) (*requestState, error) {
	if !domainID(requestID) || !domainID(checkpoint.AccountID) || !domainID(checkpoint.APIKeyID) || !validModelID(checkpoint.ModelID) || !domainID(checkpoint.OwnerID) ||
		checkpoint.InputTokens < 0 || checkpoint.InputTokens > maxRequestInputTokens || checkpoint.MaxOutputTokens < 1 ||
		checkpoint.MaxOutputTokens > maxRequestOutputTokens || !validAcceptedDay(checkpoint.AcceptedDay) || checkpoint.AcceptedAt.IsZero() ||
		checkpoint.AcceptedAt.UTC().Format("2006-01-02") != checkpoint.AcceptedDay ||
		!checkpoint.Price.Version.Valid() || checkpoint.Price.OwnerShareBPS > 10_000 || len(checkpoint.Attempts) == 0 || len(checkpoint.Attempts) > maxAttempts {
		return nil, ErrInvalidSnapshot
	}
	switch checkpoint.Status {
	case RequestAccepted, RequestRunning, RequestSettled, RequestRefunded, RequestCancelled:
	default:
		return nil, ErrInvalidSnapshot
	}
	state := &requestState{AccountID: checkpoint.AccountID, APIKeyID: checkpoint.APIKeyID, ModelID: checkpoint.ModelID, OwnerID: checkpoint.OwnerID,
		InputTokens: checkpoint.InputTokens, MaxOutputTokens: checkpoint.MaxOutputTokens, DailySpendCap: checkpoint.DailySpendCap.Int64(),
		AcceptedDay: checkpoint.AcceptedDay, AcceptedAt: checkpoint.AcceptedAt.UTC(), Price: checkpoint.Price, Status: checkpoint.Status,
		Attempts: make(map[string]*attemptState, len(checkpoint.Attempts))}
	active, terminal, settled, cancelled, refunded := 0, 0, 0, 0, 0
	owners := make(map[string]bool, len(checkpoint.Attempts))
	ownerMatches := 0
	for attemptID, attempt := range checkpoint.Attempts {
		reservation := attempt.Reservation
		if _, err := domain.ParsePiconero(reservation.Amount); err != nil || !domainID(attemptID) ||
			reservation.RequestID != requestID || reservation.AttemptID != attemptID || reservation.AccountID != checkpoint.AccountID ||
			reservation.APIKeyID != checkpoint.APIKeyID || reservation.ModelID != checkpoint.ModelID || reservation.InputTokens != checkpoint.InputTokens ||
			reservation.MaxOutputTokens != checkpoint.MaxOutputTokens || reservation.AcceptedDay != checkpoint.AcceptedDay ||
			reservation.DailySpendCap != checkpoint.DailySpendCap.String() || !domainID(reservation.OwnerID) ||
			!samePrice(reservation.Price, checkpoint.Price) || attempt.OutputTokens < 0 || attempt.OutputTokens > checkpoint.MaxOutputTokens {
			return nil, ErrInvalidSnapshot
		}
		if owners[reservation.OwnerID] {
			return nil, ErrInvalidSnapshot
		}
		owners[reservation.OwnerID] = true
		if reservation.OwnerID == checkpoint.OwnerID {
			ownerMatches++
		}
		switch reservation.Status {
		case AttemptReserved:
			active++
			if attempt.OutputTokens != 0 {
				return nil, ErrInvalidSnapshot
			}
		case AttemptRefunded:
			refunded++
			if attempt.OutputTokens != 0 {
				return nil, ErrInvalidSnapshot
			}
		case AttemptSettled, AttemptCancelled:
			terminal++
			if reservation.Status == AttemptSettled {
				settled++
			} else {
				cancelled++
			}
		default:
			return nil, ErrInvalidSnapshot
		}
		state.Attempts[attemptID] = &attemptState{Reservation: reservation, OutputTokens: attempt.OutputTokens}
	}
	if ownerMatches != 1 || active > 1 || terminal > 1 || active > 0 && terminal > 0 ||
		checkpoint.Status == RequestRunning && active != 1 ||
		checkpoint.Status == RequestAccepted && (active != 0 || terminal != 0) ||
		checkpoint.Status == RequestSettled && (active != 0 || settled != 1 || cancelled != 0) ||
		checkpoint.Status == RequestCancelled && (active != 0 || cancelled != 1 || settled != 0) ||
		checkpoint.Status == RequestRefunded && (active != 0 || terminal != 0 || refunded != len(checkpoint.Attempts)) {
		return nil, ErrInvalidSnapshot
	}
	return state, nil
}

func capsMatchRequests(requests map[string]*requestState, caps map[string]capState) bool {
	expected := make(map[string]capState)
	for _, request := range requests {
		key := request.AccountID + ":" + request.APIKeyID + ":" + request.AcceptedDay
		cap := expected[key]
		limit := request.DailySpendCap
		if existing, ok := expected[key]; !ok || limit < existing.Limit {
			cap.Limit = limit
		}
		for _, attempt := range request.Attempts {
			amount, err := domain.ParsePiconero(attempt.Reservation.Amount)
			if err != nil {
				return false
			}
			switch attempt.Reservation.Status {
			case AttemptReserved:
				var ok bool
				cap.Reserved, ok = checkedAdd(cap.Reserved, amount.Int64())
				if !ok {
					return false
				}
			case AttemptSettled:
				quote, err := CalculateQuote(request.InputTokens, attempt.OutputTokens, request.MaxOutputTokens, request.Price, false)
				if err != nil {
					return false
				}
				var ok bool
				cap.Spent, ok = checkedAdd(cap.Spent, quote.ActualCost.Int64())
				if !ok {
					return false
				}
			case AttemptCancelled:
				quote, err := CalculateQuote(request.InputTokens, attempt.OutputTokens, request.MaxOutputTokens, request.Price, true)
				if err != nil {
					return false
				}
				var ok bool
				cap.Spent, ok = checkedAdd(cap.Spent, quote.ActualCost.Int64())
				if !ok {
					return false
				}
			}
		}
		expected[key] = cap
	}
	for key, got := range expected {
		actual, ok := caps[key]
		if !ok || actual.Limit != got.Limit || actual.Reserved != got.Reserved || actual.Spent != got.Spent || actual.Spent > actual.Limit ||
			actual.Reserved > actual.Limit-actual.Spent {
			return false
		}
	}
	for key, actual := range caps {
		if expected[key].Reserved != actual.Reserved || expected[key].Spent != actual.Spent {
			return false
		}
	}
	return true
}

func checksumSnapshot(snapshot LedgerSnapshot) string {
	snapshot.Checksum = ""
	data, _ := json.Marshal(snapshot)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func copyLedgerEntries(entries []Entry) []Entry {
	copyOf := make([]Entry, len(entries))
	copy(copyOf, entries)
	for i := range copyOf {
		copyOf[i] = cloneEntry(copyOf[i])
	}
	return copyOf
}

func cloneEntry(entry Entry) Entry {
	entry.Postings = append([]Posting(nil), entry.Postings...)
	return entry
}

func domainID(value string) bool {
	_, err := domain.ParseID(value)
	return err == nil
}

func validAcceptedDay(value string) bool {
	parsed, err := time.Parse("2006-01-02", value)
	return err == nil && parsed.Format("2006-01-02") == value
}

func validBucketAccount(bucket, account string) bool {
	switch bucket {
	case BucketAvailable, BucketReserved, BucketOwnerPending, BucketOwnerAvailable, BucketPayoutReserved:
		return domainID(account)
	case BucketCashHot, BucketEquity, BucketNetworkFee:
		return account == ""
	default:
		return false
	}
}

func isLiabilityBucket(bucket string) bool {
	switch bucket {
	case BucketAvailable, BucketReserved, BucketOwnerPending, BucketOwnerAvailable, BucketPayoutReserved:
		return true
	default:
		return false
	}
}
