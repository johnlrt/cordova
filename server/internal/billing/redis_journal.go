package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cordova/server/internal/domain"

	"github.com/redis/go-redis/v9"
)

var (
	ErrJournalCircuitOpen = errors.New("financial journal is unavailable pending reconciliation")
	ErrJournalUncertain   = errors.New("financial journal durability is uncertain")
	ErrJournalConflict    = errors.New("financial journal changed concurrently")
	ErrRequestNotFound    = errors.New("request accounting record was not found")
)

const (
	defaultJournalStream = "cv:v1:ledger"
	defaultOutboxStream  = "cv:v1:outbox:financial"
	defaultHeadKey       = "cv:v1:ledger:head"
	defaultBalancesKey   = "cv:v1:balances"
	defaultOperationsKey = "cv:v1:ledger:operations"
	defaultRequestsKey   = "cv:v1:requests"
	defaultCapsKey       = "cv:v1:spend-caps"
	defaultBarrierKey    = "cv:v1:ledger:barrier"
)

// DefaultUsagePendingKey is the Redis set populated atomically by terminal
// request appends until their account usage projection reaches the outbox.
const DefaultUsagePendingKey = "cv:v1:usage:pending"

// RedisJournal is the atomic Redis write boundary for append-only ledger
// entries. Each append uses one dedicated connection for EVAL and WAITAOF.
// It does not decide request policy; callers calculate and validate postings.
type RedisJournal struct {
	client          *redis.Client
	requireReplica  bool
	uncertain       atomic.Bool
	recovered       atomic.Bool
	recoveryMu      sync.Mutex
	write           *redis.Script
	stream          string
	outbox          string
	headKey         string
	balancesKey     string
	operationsKey   string
	requestsKey     string
	capsKey         string
	barrierKey      string
	usagePendingKey string
	faultInjector   func(redisJournalFaultStage, string) error
}

type redisJournalFaultStage string

const (
	redisFaultAfterAtomicAppend redisJournalFaultStage = "after_atomic_append"
	redisFaultAfterAOFBarrier   redisJournalFaultStage = "after_aof_barrier"
)

func NewRedisJournal(client *redis.Client, requireReplica bool) (*RedisJournal, error) {
	if client == nil {
		return nil, errors.New("Redis client is required")
	}
	return &RedisJournal{client: client, requireReplica: requireReplica, write: redis.NewScript(appendLedgerScript),
		stream: defaultJournalStream, outbox: defaultOutboxStream, headKey: defaultHeadKey,
		balancesKey: defaultBalancesKey, operationsKey: defaultOperationsKey, requestsKey: defaultRequestsKey,
		capsKey: defaultCapsKey, barrierKey: defaultBarrierKey, usagePendingKey: DefaultUsagePendingKey}, nil
}

func (j *RedisJournal) Ready() bool { return j.recovered.Load() && !j.uncertain.Load() }

// ClearCircuitAfterReconciliation is an operator action. Call it only after
// reading the operation tombstone, journal head, balances and WAITAOF state.
func (j *RedisJournal) ClearCircuitAfterReconciliation() { j.uncertain.Store(false) }

func (j *RedisJournal) Append(ctx context.Context, entry Entry) (Entry, error) {
	if entry.RequestID != "" {
		return Entry{}, ErrInvalidOperation
	}
	return j.append(ctx, entry, "", 0, RequestCheckpoint{}, CapAdjustment{}, false)
}

// CapAdjustment applies acceptance-day spend-counter deltas atomically with a
// request state transition and its balanced journal entry.
type CapAdjustment struct {
	Key           string `json:"key"`
	Limit         string `json:"limit_piconero"`
	SpentDelta    string `json:"spent_delta_piconero"`
	ReservedDelta string `json:"reserved_delta_piconero"`
}

// AppendRequest atomically journals a financial operation, updates the
// metadata-only request checkpoint and applies its spend-cap counters. The
// expected revision fences concurrent terminal operations.
func (j *RedisJournal) AppendRequest(ctx context.Context, entry Entry, requestID string, expectedRevision uint64,
	request RequestCheckpoint, capAdjustment CapAdjustment) (Entry, error) {
	return j.appendRequest(ctx, entry, requestID, expectedRevision, request, capAdjustment, false)
}

func (j *RedisJournal) appendRequest(ctx context.Context, entry Entry, requestID string, expectedRevision uint64,
	request RequestCheckpoint, capAdjustment CapAdjustment, recovering bool) (Entry, error) {
	if !validID(requestID) || entry.RequestID != requestID || requestID == "" ||
		request.AccountID == "" || request.APIKeyID == "" || capAdjustment.Key == "" {
		return Entry{}, ErrInvalidOperation
	}
	if _, err := restoreRequestCheckpoint(requestID, request); err != nil {
		return Entry{}, err
	}
	limit, err := domain.ParsePiconero(capAdjustment.Limit)
	if err != nil {
		return Entry{}, err
	}
	if limit.Int64() != request.DailySpendCap.Int64() {
		return Entry{}, ErrInvalidOperation
	}
	if !validID(request.AccountID) || !validID(request.APIKeyID) || capAdjustment.Key != request.AccountID+":"+request.APIKeyID+":"+request.AcceptedDay {
		return Entry{}, ErrInvalidOperation
	}
	if err := validateRequestTransition(entry, requestID, request, capAdjustment); err != nil {
		return Entry{}, err
	}
	return j.append(ctx, entry, requestID, expectedRevision, request, capAdjustment, recovering)
}

func (j *RedisJournal) append(ctx context.Context, entry Entry, requestID string, expectedRevision uint64,
	request RequestCheckpoint, capAdjustment CapAdjustment, recovering bool) (Entry, error) {
	if (!j.recovered.Load() || j.uncertain.Load()) && !recovering {
		return Entry{}, ErrJournalCircuitOpen
	}
	if err := validateJournalEntry(entry); err != nil {
		return Entry{}, err
	}
	if entry.ID == "" {
		id, err := domain.NewID()
		if err != nil {
			return Entry{}, err
		}
		entry.ID = id.String()
	}
	entry.Postings = append([]Posting(nil), entry.Postings...)
	sort.Slice(entry.Postings, func(i, k int) bool {
		if entry.Postings[i].Bucket == entry.Postings[k].Bucket {
			return entry.Postings[i].AccountID < entry.Postings[k].AccountID
		}
		return entry.Postings[i].Bucket < entry.Postings[k].Bucket
	})
	entry.CreatedAt = entry.CreatedAt.UTC()
	requestJSON := ""
	if requestID != "" {
		data, err := json.Marshal(request)
		if err != nil {
			return Entry{}, err
		}
		requestJSON = string(data)
	}
	fingerprint, err := transactionFingerprint(entry, requestJSON, capAdjustment)
	if err != nil {
		return Entry{}, err
	}
	conn := j.client.Conn()
	defer conn.Close()

	for attempt := 0; attempt < 4; attempt++ {
		previous, err := conn.Get(ctx, j.headKey).Result()
		if err != nil && err != redis.Nil {
			return Entry{}, j.markUncertain(err)
		}
		if err == redis.Nil {
			previous = ""
		}
		if previous != "" {
			decoded, decodeErr := hex.DecodeString(previous)
			if decodeErr != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != previous {
				return Entry{}, j.markUncertain(ErrInvalidOperation)
			}
		}
		entry.PreviousHash = previous
		entry.EntryHash, err = hashEntry(entry)
		if err != nil {
			return Entry{}, err
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return Entry{}, err
		}
		outbox, err := json.Marshal(financialOutboxEvent{Version: 1, EventID: entry.ID, EventType: "ledger.updated",
			OccurredAt: entry.CreatedAt.Format(time.RFC3339Nano), RequestID: optional(entry.RequestID), AttemptID: optional(entry.AttemptID),
			OperationID: entry.OperationID, Kind: entry.Kind, EntryHash: entry.EntryHash})
		if err != nil {
			return Entry{}, err
		}
		hasRequest := "0"
		requestRevision := "0"
		capKey, capLimit, spentDelta, reservedDelta := "", "0", "0", "0"
		if requestID != "" {
			hasRequest = "1"
			requestRevision = strconv.FormatUint(expectedRevision, 10)
			capKey, capLimit = capAdjustment.Key, capAdjustment.Limit
			spentDelta, reservedDelta = capAdjustment.SpentDelta, capAdjustment.ReservedDelta
		}
		args := []any{entry.OperationID, previous, entry.EntryHash, fingerprint, string(encoded), string(outbox),
			hasRequest, requestID, requestRevision, requestJSON, capKey, capLimit, spentDelta, reservedDelta}
		args = append(args, postingsAsArgs(entry.Postings)...)
		result, err := j.write.Run(ctx, conn, []string{j.headKey, j.balancesKey, j.operationsKey, j.stream, j.outbox,
			j.requestsKey, j.capsKey, j.usagePendingKey}, args...).Slice()
		if err != nil {
			return Entry{}, j.markUncertain(err)
		}
		code, payload, err := parseAppendResult(result)
		if err != nil {
			return Entry{}, j.markUncertain(err)
		}
		switch code {
		case 1: // new append
		case 0: // idempotent replay; the operation tombstone is authoritative
			var prior Entry
			if json.Unmarshal([]byte(payload), &prior) != nil || prior.OperationID != entry.OperationID {
				return Entry{}, j.markUncertain(ErrInvalidOperation)
			}
			entry = prior
			if err := conn.Set(ctx, j.barrierKey, entry.OperationID+":"+fingerprint, 0).Err(); err != nil {
				return Entry{}, j.markUncertain(err)
			}
		case -1:
			return Entry{}, ErrConflict
		case -2:
			continue
		case -3:
			return Entry{}, ErrConflict
		case -4:
			return Entry{}, ErrSpendCapExceeded
		case -5:
			return Entry{}, ErrInsufficientFunds
		default:
			return Entry{}, j.markUncertain(ErrInvalidOperation)
		}
		if err := j.injectFault(redisFaultAfterAtomicAppend, entry.OperationID); err != nil {
			return Entry{}, j.markUncertain(err)
		}
		if err := j.waitAOF(ctx, conn); err != nil {
			return Entry{}, j.markUncertain(err)
		}
		if err := j.injectFault(redisFaultAfterAOFBarrier, entry.OperationID); err != nil {
			return Entry{}, j.markUncertain(err)
		}
		return entry, nil
	}
	return Entry{}, ErrJournalConflict
}

func (j *RedisJournal) injectFault(stage redisJournalFaultStage, operationID string) error {
	if j.faultInjector == nil {
		return nil
	}
	return j.faultInjector(stage, operationID)
}

func (j *RedisJournal) waitAOF(ctx context.Context, conn *redis.Conn) error {
	local, replica := int64(1), int64(0)
	if j.requireReplica {
		replica = 1
	}
	result, err := conn.Do(ctx, "WAITAOF", local, replica, 3000).Int64Slice()
	if err != nil {
		return err
	}
	if len(result) != 2 || result[0] < local || result[1] < replica {
		return ErrJournalUncertain
	}
	return nil
}

func (j *RedisJournal) markUncertain(err error) error {
	j.uncertain.Store(true)
	return fmt.Errorf("%w: %v", ErrJournalUncertain, err)
}

// Request returns metadata-only idempotency state. Callers must not persist or
// return prompt text from request records; no request content is stored here.
func (j *RedisJournal) Request(ctx context.Context, requestID string) (RequestCheckpoint, uint64, error) {
	if !validID(requestID) {
		return RequestCheckpoint{}, 0, ErrInvalidOperation
	}
	encoded, err := j.client.HGet(ctx, j.requestsKey, "data:"+requestID).Result()
	if err == redis.Nil {
		return RequestCheckpoint{}, 0, ErrRequestNotFound
	}
	if err != nil {
		return RequestCheckpoint{}, 0, j.markUncertain(err)
	}
	var request RequestCheckpoint
	if json.Unmarshal([]byte(encoded), &request) != nil {
		return RequestCheckpoint{}, 0, j.markUncertain(ErrInvalidSnapshot)
	}
	if _, err := restoreRequestCheckpoint(requestID, request); err != nil {
		return RequestCheckpoint{}, 0, j.markUncertain(err)
	}
	revisionText, err := j.client.HGet(ctx, j.requestsKey, "rev:"+requestID).Result()
	if err != nil {
		return RequestCheckpoint{}, 0, j.markUncertain(err)
	}
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || strconv.FormatUint(revision, 10) != revisionText || revision == 0 {
		return RequestCheckpoint{}, 0, j.markUncertain(ErrInvalidSnapshot)
	}
	return request, revision, nil
}

// PendingUsageRequestIDs returns terminal ledger requests whose content-free
// usage projection has not yet been acknowledged by the account outbox.
func (j *RedisJournal) PendingUsageRequestIDs(ctx context.Context) ([]string, error) {
	ids, err := j.client.SMembers(ctx, j.usagePendingKey).Result()
	if err != nil {
		return nil, j.markUncertain(err)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !validID(id) {
			return nil, j.markUncertain(ErrInvalidSnapshot)
		}
	}
	return ids, nil
}

type storedRequest struct {
	id         string
	revision   uint64
	checkpoint RequestCheckpoint
}

func (j *RedisJournal) scanRequests(ctx context.Context) ([]storedRequest, error) {
	var cursor uint64
	var requests []storedRequest
	for {
		values, next, err := j.client.HScan(ctx, j.requestsKey, cursor, "data:*", 256).Result()
		if err != nil {
			return nil, j.markUncertain(err)
		}
		if len(values)%2 != 0 {
			return nil, j.markUncertain(ErrInvalidSnapshot)
		}
		for index := 0; index < len(values); index += 2 {
			field, encoded := values[index], values[index+1]
			requestID := strings.TrimPrefix(field, "data:")
			if field == requestID || !validID(requestID) {
				return nil, j.markUncertain(ErrInvalidSnapshot)
			}
			var checkpoint RequestCheckpoint
			if json.Unmarshal([]byte(encoded), &checkpoint) != nil {
				return nil, j.markUncertain(ErrInvalidSnapshot)
			}
			if _, err := restoreRequestCheckpoint(requestID, checkpoint); err != nil {
				return nil, j.markUncertain(err)
			}
			if err := j.verifyRequestTombstones(ctx, requestID, checkpoint); err != nil {
				return nil, j.markUncertain(err)
			}
			revisionText, err := j.client.HGet(ctx, j.requestsKey, "rev:"+requestID).Result()
			if err != nil {
				return nil, j.markUncertain(err)
			}
			revision, err := strconv.ParseUint(revisionText, 10, 64)
			if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != revisionText {
				return nil, j.markUncertain(ErrInvalidSnapshot)
			}
			requests = append(requests, storedRequest{id: requestID, revision: revision, checkpoint: checkpoint})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	sort.Slice(requests, func(i, k int) bool { return requests[i].id < requests[k].id })
	return requests, nil
}

func (j *RedisJournal) verifyRequestTombstones(ctx context.Context, requestID string, request RequestCheckpoint) error {
	for attemptID, attempt := range request.Attempts {
		if err := j.verifyOperationRecord(ctx, "reserve:"+requestID+":"+attemptID, requestID, attemptID, "reserve"); err != nil {
			return err
		}
		switch attempt.Reservation.Status {
		case AttemptReserved:
			for _, operation := range []string{"refund:" + requestID + ":" + attemptID, "settle:" + requestID, "cancel:" + requestID} {
				if err := j.requireOperationAbsent(ctx, operation); err != nil {
					return err
				}
			}
		case AttemptRefunded:
			if err := j.verifyOperationRecord(ctx, "refund:"+requestID+":"+attemptID, requestID, attemptID, "attempt_refund"); err != nil {
				return err
			}
		case AttemptSettled:
			if err := j.verifyOperationRecord(ctx, "settle:"+requestID, requestID, attemptID, "settle"); err != nil {
				return err
			}
		case AttemptCancelled:
			if err := j.verifyOperationRecord(ctx, "cancel:"+requestID, requestID, attemptID, "cancel"); err != nil {
				return err
			}
		default:
			return ErrInvalidSnapshot
		}
	}
	switch request.Status {
	case RequestAccepted, RequestRunning:
		for _, operation := range []string{"settle:" + requestID, "cancel:" + requestID, "abandon:" + requestID} {
			if err := j.requireOperationAbsent(ctx, operation); err != nil {
				return err
			}
		}
	case RequestSettled:
		for _, operation := range []string{"cancel:" + requestID, "abandon:" + requestID} {
			if err := j.requireOperationAbsent(ctx, operation); err != nil {
				return err
			}
		}
	case RequestCancelled:
		for _, operation := range []string{"settle:" + requestID, "abandon:" + requestID} {
			if err := j.requireOperationAbsent(ctx, operation); err != nil {
				return err
			}
		}
	case RequestRefunded:
		for _, operation := range []string{"settle:" + requestID, "cancel:" + requestID} {
			if err := j.requireOperationAbsent(ctx, operation); err != nil {
				return err
			}
		}
	default:
		return ErrInvalidSnapshot
	}
	if request.Status == RequestRefunded {
		allRefunded := true
		for _, attempt := range request.Attempts {
			allRefunded = allRefunded && attempt.Reservation.Status == AttemptRefunded
		}
		if allRefunded {
			if err := j.requireOperationAbsent(ctx, "abandon:"+requestID); err != nil {
				attemptID, ownerErr := ownerAttemptID(request)
				if ownerErr != nil {
					return ownerErr
				}
				if verifyErr := j.verifyOperationRecord(ctx, "abandon:"+requestID, requestID, attemptID, "request_abandoned"); verifyErr != nil {
					return verifyErr
				}
			}
		}
	}
	return nil
}

func ownerAttemptID(request RequestCheckpoint) (string, error) {
	matched := ""
	for attemptID, attempt := range request.Attempts {
		if attempt.Reservation.OwnerID == request.OwnerID {
			if matched != "" {
				return "", ErrInvalidSnapshot
			}
			matched = attemptID
		}
	}
	if matched == "" {
		return "", ErrInvalidSnapshot
	}
	return matched, nil
}

func (j *RedisJournal) verifyOperationRecord(ctx context.Context, operationID, requestID, attemptID, kind string) error {
	encoded, err := j.client.HGet(ctx, j.operationsKey, "entry:"+operationID).Result()
	if err != nil {
		return err
	}
	var entry Entry
	if json.Unmarshal([]byte(encoded), &entry) != nil || entry.OperationID != operationID || entry.RequestID != requestID ||
		entry.AttemptID != attemptID || entry.Kind != kind {
		return ErrInvalidSnapshot
	}
	return nil
}

func (j *RedisJournal) requireOperationAbsent(ctx context.Context, operationID string) error {
	_, err := j.client.HGet(ctx, j.operationsKey, "entry:"+operationID).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrInvalidSnapshot
}

// Verify replays the full append-only stream and checks its hash chain,
// balanced postings, balances, operation tombstones, head and outbox count.
// It is intentionally a startup/recovery gate; large deployments may later
// add signed checkpoints without changing this verification contract.
func (j *RedisJournal) Verify(ctx context.Context) error {
	previous := ""
	balances := make(map[string]int64)
	operations := make(map[string]string)
	start := "-"
	var entryCount int64
	for {
		page, err := j.client.XRangeN(ctx, j.stream, start, "+", 512).Result()
		if err != nil {
			return j.markUncertain(err)
		}
		if len(page) == 0 {
			break
		}
		for _, record := range page {
			encoded, ok := record.Values["entry"].(string)
			if !ok {
				if raw, rawOK := record.Values["entry"].([]byte); rawOK {
					encoded, ok = string(raw), true
				}
			}
			if !ok {
				return j.markUncertain(ErrInvalidSnapshot)
			}
			var entry Entry
			if json.Unmarshal([]byte(encoded), &entry) != nil || validateJournalEntry(entry) != nil ||
				entry.PreviousHash != previous || operations[entry.OperationID] != "" {
				return j.markUncertain(ErrInvalidSnapshot)
			}
			hash, err := hashEntry(entry)
			if err != nil || hash != entry.EntryHash {
				return j.markUncertain(ErrInvalidSnapshot)
			}
			var total big.Int
			for _, posting := range entry.Postings {
				delta, err := parseSigned(posting.Delta)
				if err != nil {
					return j.markUncertain(ErrInvalidSnapshot)
				}
				total.Add(&total, big.NewInt(delta))
				key := posting.Bucket + ":" + posting.AccountID
				next, ok := checkedAdd(balances[key], delta)
				if !ok || next > domain.MaxPiconero || next < -domain.MaxPiconero ||
					isLiabilityBucket(posting.Bucket) && next > 0 {
					return j.markUncertain(ErrInvalidSnapshot)
				}
				balances[key] = next
			}
			if total.Sign() != 0 {
				return j.markUncertain(ErrInvalidSnapshot)
			}
			operations[entry.OperationID] = encoded
			previous = entry.EntryHash
			entryCount++
		}
		start = "(" + page[len(page)-1].ID
		if len(page) < 512 {
			break
		}
	}
	storedHead, err := j.client.Get(ctx, j.headKey).Result()
	if err == redis.Nil {
		storedHead = ""
	} else if err != nil {
		return j.markUncertain(err)
	}
	if storedHead != previous {
		return j.markUncertain(ErrInvalidSnapshot)
	}
	if got, err := j.client.HLen(ctx, j.operationsKey).Result(); err != nil || got != entryCount*2 {
		if err != nil {
			return j.markUncertain(err)
		}
		return j.markUncertain(ErrInvalidSnapshot)
	}
	for operation, encoded := range operations {
		stored, err := j.client.HGet(ctx, j.operationsKey, "entry:"+operation).Result()
		fingerprint, fingerprintErr := j.client.HGet(ctx, j.operationsKey, "fp:"+operation).Result()
		if err != nil || fingerprintErr != nil || stored != encoded || len(fingerprint) != sha256.Size*2 {
			return j.markUncertain(ErrInvalidSnapshot)
		}
		if _, err := hex.DecodeString(fingerprint); err != nil {
			return j.markUncertain(ErrInvalidSnapshot)
		}
	}
	if outboxLength, err := j.client.XLen(ctx, j.outbox).Result(); err != nil || outboxLength != entryCount {
		if err != nil {
			return j.markUncertain(err)
		}
		return j.markUncertain(ErrInvalidSnapshot)
	}
	storedBalances, err := scanHash(ctx, j.client, j.balancesKey)
	if err != nil {
		return j.markUncertain(err)
	}
	if len(storedBalances) != len(balances) {
		return j.markUncertain(ErrInvalidSnapshot)
	}
	for key, expected := range balances {
		if storedBalances[key] != strconv.FormatInt(expected, 10) {
			return j.markUncertain(ErrInvalidSnapshot)
		}
	}
	return nil
}

func (j *RedisJournal) Recover(ctx context.Context, at time.Time) (int, error) {
	j.recoveryMu.Lock()
	defer j.recoveryMu.Unlock()
	j.recovered.Store(false)
	if at.IsZero() {
		return 0, ErrInvalidOperation
	}
	if err := j.Verify(ctx); err != nil {
		return 0, err
	}
	requests, err := j.scanRequests(ctx)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, stored := range requests {
		checkpoint := stored.checkpoint
		if checkpoint.Status == RequestRunning {
			var activeID string
			for attemptID, attempt := range checkpoint.Attempts {
				if attempt.Reservation.Status == AttemptReserved {
					if activeID != "" {
						return recovered, j.markUncertain(ErrInvalidSnapshot)
					}
					activeID = attemptID
				}
			}
			if activeID == "" {
				return recovered, j.markUncertain(ErrInvalidSnapshot)
			}
			attempt := checkpoint.Attempts[activeID]
			amount, err := domain.ParsePiconero(attempt.Reservation.Amount)
			if err != nil {
				return recovered, j.markUncertain(err)
			}
			attempt.Reservation.Status = AttemptRefunded
			checkpoint.Attempts[activeID] = attempt
			checkpoint.Status = RequestRefunded
			entry := Entry{OperationID: "refund:" + stored.id + ":" + activeID, Kind: "attempt_refund", RequestID: stored.id,
				AttemptID: activeID, PriceVersion: checkpoint.Price.Version.String(), CreatedAt: at.UTC(), Postings: []Posting{
					posting(BucketReserved, checkpoint.AccountID, amount.Int64()), posting(BucketAvailable, checkpoint.AccountID, -amount.Int64()),
				}}
			cap := CapAdjustment{Key: checkpoint.AccountID + ":" + checkpoint.APIKeyID + ":" + checkpoint.AcceptedDay,
				Limit: checkpoint.DailySpendCap.String(), SpentDelta: "0", ReservedDelta: "-" + amount.String()}
			if _, err := j.appendRequest(ctx, entry, stored.id, stored.revision, checkpoint, cap, true); err != nil {
				return recovered, err
			}
			recovered++
		} else if checkpoint.Status == RequestAccepted {
			for _, attempt := range checkpoint.Attempts {
				if attempt.Reservation.Status != AttemptRefunded || attempt.OutputTokens != 0 {
					return recovered, j.markUncertain(ErrInvalidSnapshot)
				}
			}
			activeID, ownerErr := ownerAttemptID(checkpoint)
			if ownerErr != nil {
				return recovered, j.markUncertain(ownerErr)
			}
			checkpoint.Status = RequestRefunded
			entry := Entry{OperationID: "abandon:" + stored.id, Kind: "request_abandoned", RequestID: stored.id,
				AttemptID: activeID, PriceVersion: checkpoint.Price.Version.String(), CreatedAt: at.UTC(), Postings: []Posting{
					posting(BucketAvailable, checkpoint.AccountID, 0), posting(BucketReserved, checkpoint.AccountID, 0),
				}}
			cap := CapAdjustment{Key: checkpoint.AccountID + ":" + checkpoint.APIKeyID + ":" + checkpoint.AcceptedDay,
				Limit: checkpoint.DailySpendCap.String(), SpentDelta: "0", ReservedDelta: "0"}
			if _, err := j.appendRequest(ctx, entry, stored.id, stored.revision, checkpoint, cap, true); err != nil {
				return recovered, err
			}
			recovered++
		}
	}
	requests, err = j.scanRequests(ctx)
	if err != nil {
		return recovered, err
	}
	if err := j.verifyCaps(ctx, requests); err != nil {
		return recovered, j.markUncertain(err)
	}
	if err := j.Verify(ctx); err != nil {
		return recovered, err
	}
	j.recovered.Store(true)
	return recovered, nil
}

func (j *RedisJournal) verifyCaps(ctx context.Context, requests []storedRequest) error {
	expected := make(map[string]capState)
	for _, stored := range requests {
		request := stored.checkpoint
		key := request.AccountID + ":" + request.APIKeyID + ":" + request.AcceptedDay
		cap := expected[key]
		limit := request.DailySpendCap.Int64()
		_, hasCap := expected[key]
		if !hasCap || limit < cap.Limit {
			cap.Limit = limit
		}
		for _, attempt := range request.Attempts {
			amount, err := domain.ParsePiconero(attempt.Reservation.Amount)
			if err != nil {
				return ErrInvalidSnapshot
			}
			switch attempt.Reservation.Status {
			case AttemptReserved:
				var ok bool
				cap.Reserved, ok = checkedAdd(cap.Reserved, amount.Int64())
				if !ok {
					return ErrInvalidSnapshot
				}
			case AttemptSettled, AttemptCancelled:
				quote, err := CalculateQuote(request.InputTokens, attempt.OutputTokens, request.MaxOutputTokens, request.Price,
					attempt.Reservation.Status == AttemptCancelled)
				if err != nil {
					return ErrInvalidSnapshot
				}
				var ok bool
				cap.Spent, ok = checkedAdd(cap.Spent, quote.ActualCost.Int64())
				if !ok {
					return ErrInvalidSnapshot
				}
			}
		}
		expected[key] = cap
	}
	for key, cap := range expected {
		limit, err := j.client.HGet(ctx, j.capsKey, "limit:"+key).Result()
		if err != nil {
			return err
		}
		spent, err := j.client.HGet(ctx, j.capsKey, "spent:"+key).Result()
		if err != nil {
			return err
		}
		reserved, err := j.client.HGet(ctx, j.capsKey, "reserved:"+key).Result()
		if err != nil || limit != strconv.FormatInt(cap.Limit, 10) || spent != strconv.FormatInt(cap.Spent, 10) ||
			reserved != strconv.FormatInt(cap.Reserved, 10) || cap.Spent > cap.Limit || cap.Reserved > cap.Limit-cap.Spent {
			return ErrInvalidSnapshot
		}
	}
	stored, err := scanHash(ctx, j.client, j.capsKey)
	if err != nil || len(stored) != len(expected)*3 {
		return ErrInvalidSnapshot
	}
	for key := range expected {
		if _, ok := stored["limit:"+key]; !ok {
			return ErrInvalidSnapshot
		}
		if _, ok := stored["spent:"+key]; !ok {
			return ErrInvalidSnapshot
		}
		if _, ok := stored["reserved:"+key]; !ok {
			return ErrInvalidSnapshot
		}
	}
	return nil
}

func scanHash(ctx context.Context, client *redis.Client, key string) (map[string]string, error) {
	values := make(map[string]string)
	var cursor uint64
	for {
		page, next, err := client.HScan(ctx, key, cursor, "*", 512).Result()
		if err != nil {
			return nil, err
		}
		if len(page)%2 != 0 {
			return nil, ErrInvalidSnapshot
		}
		for index := 0; index < len(page); index += 2 {
			if _, exists := values[page[index]]; exists {
				return nil, ErrInvalidSnapshot
			}
			values[page[index]] = page[index+1]
		}
		cursor = next
		if cursor == 0 {
			return values, nil
		}
	}
}

type financialOutboxEvent struct {
	Version     int     `json:"v"`
	EventID     string  `json:"event_id"`
	EventType   string  `json:"event_type"`
	OccurredAt  string  `json:"occurred_at"`
	RequestID   *string `json:"request_id"`
	AttemptID   *string `json:"attempt_id"`
	OperationID string  `json:"operation_id"`
	Kind        string  `json:"kind"`
	EntryHash   string  `json:"entry_hash"`
}

func validateJournalEntry(entry Entry) error {
	if entry.OperationID == "" || len(entry.OperationID) > 160 || entry.Kind == "" || len(entry.Kind) > 64 ||
		entry.CreatedAt.IsZero() || len(entry.Postings) < 2 || len(entry.Postings) > 64 {
		return ErrInvalidOperation
	}
	if entry.ID != "" && !validID(entry.ID) || entry.RequestID != "" && !validID(entry.RequestID) ||
		entry.AttemptID != "" && !validID(entry.AttemptID) || entry.PriceVersion != "" && !validID(entry.PriceVersion) {
		return ErrInvalidOperation
	}
	var total big.Int
	for _, item := range entry.Postings {
		if !validBucketAccount(item.Bucket, item.AccountID) {
			return ErrInvalidOperation
		}
		delta, err := parseSigned(item.Delta)
		if err != nil {
			return err
		}
		total.Add(&total, big.NewInt(delta))
	}
	if total.Sign() != 0 {
		return ErrInvalidOperation
	}
	return nil
}

func journalFingerprint(entry Entry) (string, error) {
	entry.ID = ""
	entry.CreatedAt = time.Time{}
	entry.PreviousHash = ""
	entry.EntryHash = ""
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func transactionFingerprint(entry Entry, requestJSON string, capAdjustment CapAdjustment) (string, error) {
	base, err := journalFingerprint(entry)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Entry       string        `json:"entry_fingerprint"`
		RequestJSON string        `json:"request_json"`
		Cap         CapAdjustment `json:"cap_adjustment"`
	}{base, requestJSON, capAdjustment})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateRequestTransition(entry Entry, requestID string, request RequestCheckpoint, capAdjustment CapAdjustment) error {
	attempt, ok := request.Attempts[entry.AttemptID]
	if !ok || entry.AttemptID == "" || entry.PriceVersion != request.Price.Version.String() ||
		attempt.Reservation.OwnerID != request.OwnerID || attempt.Reservation.DailySpendCap != request.DailySpendCap.String() {
		return ErrInvalidOperation
	}
	reserved, err := domain.ParsePiconero(attempt.Reservation.Amount)
	if err != nil {
		return err
	}
	spentDelta, err := parseSigned(capAdjustment.SpentDelta)
	if err != nil {
		return err
	}
	reservedDelta, err := parseSigned(capAdjustment.ReservedDelta)
	if err != nil {
		return err
	}
	var expectedSpent, expectedReserved int64
	var expected []Posting
	switch entry.Kind {
	case "reserve":
		if request.Status != RequestRunning || attempt.Reservation.Status != AttemptReserved || attempt.OutputTokens != 0 || entry.OperationID != "reserve:"+requestID+":"+entry.AttemptID {
			return ErrInvalidOperation
		}
		expectedReserved = reserved.Int64()
		expected = []Posting{posting(BucketAvailable, request.AccountID, reserved.Int64()), posting(BucketReserved, request.AccountID, -reserved.Int64())}
	case "attempt_refund":
		if (request.Status != RequestAccepted && request.Status != RequestRefunded) || attempt.Reservation.Status != AttemptRefunded || attempt.OutputTokens != 0 || entry.OperationID != "refund:"+requestID+":"+entry.AttemptID {
			return ErrInvalidOperation
		}
		expectedReserved = -reserved.Int64()
		expected = []Posting{posting(BucketReserved, request.AccountID, reserved.Int64()), posting(BucketAvailable, request.AccountID, -reserved.Int64())}
	case "settle", "cancel":
		cancelled := entry.Kind == "cancel"
		wantStatus := RequestSettled
		wantAttemptStatus := AttemptSettled
		operationID := "settle:" + requestID
		if cancelled {
			wantStatus, wantAttemptStatus, operationID = RequestCancelled, AttemptCancelled, "cancel:"+requestID
		}
		if request.Status != wantStatus || attempt.Reservation.Status != wantAttemptStatus || entry.OperationID != operationID ||
			attempt.OutputTokens > request.MaxOutputTokens {
			return ErrInvalidOperation
		}
		quote, quoteErr := CalculateQuote(request.InputTokens, attempt.OutputTokens, request.MaxOutputTokens, request.Price, cancelled)
		if quoteErr != nil || quote.Reservation.Int64() != reserved.Int64() {
			return ErrInvalidOperation
		}
		expectedSpent = quote.ActualCost.Int64()
		expectedReserved = -reserved.Int64()
		expected = []Posting{posting(BucketReserved, request.AccountID, reserved.Int64()),
			posting(BucketAvailable, request.AccountID, -quote.Refund.Int64()),
			posting(BucketOwnerPending, attempt.Reservation.OwnerID, -quote.OwnerCredit.Int64()),
			posting(BucketEquity, "", -quote.PlatformTake.Int64())}
	case "request_abandoned":
		if request.Status != RequestRefunded || attempt.Reservation.Status != AttemptRefunded || attempt.OutputTokens != 0 ||
			entry.OperationID != "abandon:"+requestID {
			return ErrInvalidOperation
		}
		for _, item := range request.Attempts {
			if item.Reservation.Status != AttemptRefunded {
				return ErrInvalidOperation
			}
		}
		expected = []Posting{posting(BucketAvailable, request.AccountID, 0), posting(BucketReserved, request.AccountID, 0)}
	default:
		return ErrInvalidOperation
	}
	if spentDelta != expectedSpent || reservedDelta != expectedReserved || !samePostings(entry.Postings, expected) {
		return ErrInvalidOperation
	}
	return nil
}

func samePostings(got, want []Posting) bool {
	if len(got) != len(want) {
		return false
	}
	actual := append([]Posting(nil), got...)
	expected := append([]Posting(nil), want...)
	sort.Slice(actual, func(i, j int) bool {
		if actual[i].Bucket == actual[j].Bucket {
			return actual[i].AccountID < actual[j].AccountID
		}
		return actual[i].Bucket < actual[j].Bucket
	})
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Bucket == expected[j].Bucket {
			return expected[i].AccountID < expected[j].AccountID
		}
		return expected[i].Bucket < expected[j].Bucket
	})
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func postingsAsArgs(postings []Posting) []any {
	args := make([]any, 0, len(postings)*3)
	for _, item := range postings {
		args = append(args, item.Bucket, item.AccountID, item.Delta)
	}
	return args
}

func parseAppendResult(value []any) (int64, string, error) {
	if len(value) != 2 {
		return 0, "", ErrInvalidOperation
	}
	var code int64
	switch n := value[0].(type) {
	case int64:
		code = n
	case int:
		code = int64(n)
	default:
		return 0, "", ErrInvalidOperation
	}
	payload, ok := value[1].(string)
	if !ok {
		if bytesValue, bytesOK := value[1].([]byte); bytesOK {
			payload = string(bytesValue)
		} else {
			return 0, "", ErrInvalidOperation
		}
	}
	return code, payload, nil
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

const appendLedgerScript = `
local function keytype(key)
  return redis.call('TYPE', key).ok
end
local expected = {'string','hash','hash','stream','stream','hash','hash','set'}
for i=1,8 do
  local t=keytype(KEYS[i])
  if t~='none' and t~=expected[i] then return redis.error_reply('CORDOVA_WRONG_LEDGER_KEY_TYPE') end
end
local operation=ARGV[1]
local expectedHead=ARGV[2]
local newHash=ARGV[3]
local fingerprint=ARGV[4]
local record=ARGV[5]
local event=ARGV[6]
local hasRequest=ARGV[7]
local requestID=ARGV[8]
local expectedRevision=ARGV[9]
local requestJSON=ARGV[10]
local capKey=ARGV[11]
local capLimit=ARGV[12]
local spentDelta=ARGV[13]
local reservedDelta=ARGV[14]
if operation=='' or #operation>160 or #newHash~=64 or string.match(newHash,'^[0-9a-f]+$')==nil or
   #fingerprint~=64 or string.match(fingerprint,'^[0-9a-f]+$')==nil or (#ARGV-14)%3~=0 or #ARGV<20 then
  return redis.error_reply('CORDOVA_INVALID_LEDGER_ARGUMENT')
end
local recordOK,recordObject=pcall(cjson.decode,record)
local eventOK,eventObject=pcall(cjson.decode,event)
if not recordOK or not eventOK then
  return redis.error_reply('CORDOVA_INVALID_LEDGER_JSON')
end
if recordObject.operation_id~=operation or recordObject.entry_hash~=newHash or recordObject.previous_hash~=expectedHead or
   eventObject.operation_id~=operation or eventObject.entry_hash~=newHash then
  return redis.error_reply('CORDOVA_INVALID_LEDGER_RECORD')
end
local priorFingerprint=redis.call('HGET',KEYS[3],'fp:'..operation)
if priorFingerprint then
  local prior=redis.call('HGET',KEYS[3],'entry:'..operation)
  if priorFingerprint~=fingerprint or not prior then return {-1,'idempotency conflict'} end
  return {0,prior}
end
local currentHead=redis.call('GET',KEYS[1]) or ''
if currentHead~=expectedHead then return {-2,'ledger head changed'} end

local function validSigned(value)
  if type(value)~='string' or value=='' or value=='-0' or string.sub(value,1,1)=='+' then return false end
  local digits=value
  if string.sub(value,1,1)=='-' then digits=string.sub(value,2) end
  if digits=='' or (#digits>1 and string.sub(digits,1,1)=='0') or string.match(digits,'^%d+$')==nil then return false end
  if #digits>19 or (#digits==19 and digits>'9000000000000000000') then return false end
  return true
end
local function parts(value)
  if string.sub(value,1,1)=='-' then return -1,string.sub(value,2) end
  return 1,value
end
local function cmpAbs(a,b)
  if #a<#b then return -1 end
  if #a>#b then return 1 end
  if a<b then return -1 end
  if a>b then return 1 end
  return 0
end
local function addAbs(a,b)
  local i,j,carry=#a,#b,0
  local out={}
  while i>0 or j>0 or carry>0 do
    local x,y=0,0
    if i>0 then x=string.byte(a,i)-48; i=i-1 end
    if j>0 then y=string.byte(b,j)-48; j=j-1 end
    local total=x+y+carry
    out[#out+1]=string.char(48+(total%10))
    carry=math.floor(total/10)
  end
  local reversed={}
  for k=#out,1,-1 do reversed[#reversed+1]=out[k] end
  return table.concat(reversed)
end
local function subAbs(a,b)
  local i,j,borrow=#a,#b,0
  local out={}
  while i>0 do
    local x=string.byte(a,i)-48-borrow
    local y=0
    if j>0 then y=string.byte(b,j)-48; j=j-1 end
    if x<y then x=x+10; borrow=1 else borrow=0 end
    out[#out+1]=string.char(48+x-y)
    i=i-1
  end
  while #out>1 and out[#out]=='0' do out[#out]=nil end
  local reversed={}
  for k=#out,1,-1 do reversed[#reversed+1]=out[k] end
  return table.concat(reversed)
end
local function addSigned(a,b)
  local sa,aa=parts(a); local sb,bb=parts(b)
  local sign,absolute
  if sa==sb then sign=sa; absolute=addAbs(aa,bb)
  else
    local cmp=cmpAbs(aa,bb)
    if cmp==0 then return '0' end
    if cmp>0 then sign=sa; absolute=subAbs(aa,bb) else sign=sb; absolute=subAbs(bb,aa) end
  end
  if absolute=='0' or sign>0 then return absolute end
  return '-'..absolute
end
local function validUnsigned(value)
  return type(value)=='string' and value~='' and string.match(value,'^%d+$')~=nil and
    (#value==1 or string.sub(value,1,1)~='0') and #value<=19 and
    (#value<19 or value<='9000000000000000000')
end
local function validCounter(value)
  return type(value)=='string' and value~='' and string.match(value,'^%d+$')~=nil and
    (#value==1 or string.sub(value,1,1)~='0') and #value<=20 and
    (#value<20 or value<='18446744073709551615')
end
local function mulSmall(value,multiplier)
  local out={}
  local carry=0
  for i=#value,1,-1 do
    local product=(string.byte(value,i)-48)*multiplier+carry
    out[#out+1]=string.char(48+(product%10))
    carry=math.floor(product/10)
  end
  local reversed={}
  for i=#out,1,-1 do reversed[#reversed+1]=out[i] end
  return tostring(carry)..table.concat(reversed)
end
local function divSmall(value,divisor)
  local out={}
  local remainder=0
  for i=1,#value do
    local current=remainder*10+string.byte(value,i)-48
    out[#out+1]=string.char(48+math.floor(current/divisor))
    remainder=current%divisor
  end
  local quotient=table.concat(out)
  quotient=string.gsub(quotient,'^0+','')
  if quotient=='' then quotient='0' end
  return quotient,remainder
end
local function ceilCost(tokens,rate)
  if type(tokens)~='number' or tokens<0 or tokens>1000000 or tokens~=math.floor(tokens) or not validUnsigned(rate) then
    return nil
  end
  local quotient,remainder=divSmall(mulSmall(rate,tokens),1000000)
  if remainder>0 then quotient=addSigned(quotient,'1') end
  if not validUnsigned(quotient) then return nil end
  return quotient
end
local function liability(bucket)
  return bucket=='user.available' or bucket=='user.reserved' or bucket=='owner.pending' or bucket=='owner.available' or bucket=='owner.payout_reserved'
end
local updates={}
local sum='0'
local actualPostings={}
for i=15,#ARGV,3 do
  local bucket,account,delta=ARGV[i],ARGV[i+1],ARGV[i+2]
  if bucket=='' or #bucket>64 or #account>64 or not validSigned(delta) then return redis.error_reply('CORDOVA_INVALID_POSTING') end
  if liability(bucket) and account=='' then return redis.error_reply('CORDOVA_INVALID_ACCOUNT') end
  sum=addSigned(sum,delta)
  local field=bucket..':'..account
  if actualPostings[field] then return redis.error_reply('CORDOVA_DUPLICATE_POSTING') end
  actualPostings[field]=delta
  local value=updates[field]
  if not value then value=redis.call('HGET',KEYS[2],field) or '0' end
  if not validSigned(value) then return redis.error_reply('CORDOVA_INVALID_BALANCE') end
  local nextValue=addSigned(value,delta)
  if not validSigned(nextValue) or liability(bucket) and string.sub(nextValue,1,1)~='-' and nextValue~='0' then
    if bucket=='user.available' and recordObject.kind=='reserve' and string.sub(nextValue,1,1)~='-' and nextValue~='0' then
      return {-5,'insufficient funds'}
    end
    return redis.error_reply('CORDOVA_BALANCE_OUT_OF_RANGE')
  end
  updates[field]=nextValue
end
if sum~='0' then return redis.error_reply('CORDOVA_UNBALANCED_POSTINGS') end

local requestField=nil
local revisionField=nil
local newRevision=nil
local nextSpent=nil
local nextReserved=nil
local effectiveLimit=nil
local terminalRequest=false
if hasRequest=='0' then
  if requestID~='' or expectedRevision~='0' or requestJSON~='' or capKey~='' or capLimit~='0' or spentDelta~='0' or reservedDelta~='0' then
    return redis.error_reply('CORDOVA_INVALID_EMPTY_REQUEST_STATE')
  end
elseif hasRequest=='1' then
  if #requestID~=36 or string.match(requestID,'^[0-9a-f]+%-[0-9a-f]+%-[0-9a-f]+%-[0-9a-f]+%-[0-9a-f]+$')==nil or
     string.sub(requestID,15,15)~='4' or not validCounter(expectedRevision) or #capKey>256 or
     not validUnsigned(capLimit) or not validSigned(spentDelta) or not validSigned(reservedDelta) then
    return redis.error_reply('CORDOVA_INVALID_REQUEST_ARGUMENT')
  end
  local requestOK,requestObject=pcall(cjson.decode,requestJSON)
  if not requestOK or type(requestObject)~='table' or requestObject.account_id==nil or requestObject.api_key_id==nil or
     type(requestObject.model_id)~='string' or #requestObject.model_id<1 or #requestObject.model_id>64 or
     requestObject.accepted_day==nil or requestObject.accepted_at==nil or requestObject.daily_spend_cap_piconero~=capLimit or
     type(requestObject.input_tokens)~='number' or requestObject.input_tokens<0 or requestObject.input_tokens>1000000 or
     requestObject.input_tokens~=math.floor(requestObject.input_tokens) or type(requestObject.max_output_tokens)~='number' or
     requestObject.max_output_tokens<1 or requestObject.max_output_tokens>2048 or requestObject.max_output_tokens~=math.floor(requestObject.max_output_tokens) or
     type(requestObject.price)~='table' or type(requestObject.price.owner_share_bps)~='number' or
     requestObject.price.owner_share_bps<0 or requestObject.price.owner_share_bps>10000 or
     recordObject.request_id~=requestID or recordObject.attempt_id==nil then
    return redis.error_reply('CORDOVA_INVALID_REQUEST_JSON')
  end
  requestField='data:'..requestID
  revisionField='rev:'..requestID
  local currentRevision=redis.call('HGET',KEYS[6],revisionField) or '0'
  local currentRequestJSON=redis.call('HGET',KEYS[6],requestField)
  if not validCounter(currentRevision) or currentRevision~=expectedRevision or
     (currentRequestJSON and currentRevision=='0') or (not currentRequestJSON and currentRevision~='0') then
    return {-3,'request revision changed'}
  end
  newRevision=addSigned(currentRevision,'1')
  if not validCounter(newRevision) then return redis.error_reply('CORDOVA_REQUEST_REVISION_OVERFLOW') end
  local oldRequest=nil
  if currentRequestJSON then
    local oldOK,oldObject=pcall(cjson.decode,currentRequestJSON)
    if not oldOK or type(oldObject)~='table' or oldObject.account_id~=requestObject.account_id or
       oldObject.api_key_id~=requestObject.api_key_id or oldObject.input_tokens~=requestObject.input_tokens or
       oldObject.model_id~=requestObject.model_id or
       oldObject.max_output_tokens~=requestObject.max_output_tokens or
       oldObject.daily_spend_cap_piconero~=requestObject.daily_spend_cap_piconero or
       oldObject.accepted_day~=requestObject.accepted_day or oldObject.accepted_at~=requestObject.accepted_at or
       type(oldObject.price)~='table' or type(requestObject.price)~='table' or
       oldObject.price.version~=requestObject.price.version or oldObject.price.input_rate_piconero_per_million~=requestObject.price.input_rate_piconero_per_million or
       oldObject.price.output_rate_piconero_per_million~=requestObject.price.output_rate_piconero_per_million or
       oldObject.price.owner_share_bps~=requestObject.price.owner_share_bps then
      return redis.error_reply('CORDOVA_REQUEST_IMMUTABLE_FIELDS_CHANGED')
    end
    oldRequest=oldObject
  end
  if type(requestObject.attempts)~='table' then return redis.error_reply('CORDOVA_INVALID_REQUEST_ATTEMPTS') end
  local activeCount,refundedCount,settledCount,cancelledCount=0,0,0,0
  local selected=nil
  local attemptCount=0
  for attemptID,item in pairs(requestObject.attempts) do
    attemptCount=attemptCount+1
    if type(item)~='table' or type(item.reservation)~='table' or not validUnsigned(item.reservation.reserved_piconero) or
       item.reservation.request_id~=requestID or item.reservation.attempt_id~=attemptID or
       item.reservation.account_id~=requestObject.account_id or item.reservation.api_key_id~=requestObject.api_key_id or
       item.reservation.model_id~=requestObject.model_id or
       item.reservation.daily_spend_cap_piconero~=requestObject.daily_spend_cap_piconero or
       item.reservation.accepted_day~=requestObject.accepted_day or item.reservation.price.version~=requestObject.price.version or
       item.reservation.price.input_rate_piconero_per_million~=requestObject.price.input_rate_piconero_per_million or
       item.reservation.price.output_rate_piconero_per_million~=requestObject.price.output_rate_piconero_per_million or
       item.reservation.price.owner_share_bps~=requestObject.price.owner_share_bps or
       type(item.output_tokens)~='number' or item.output_tokens<0 or item.output_tokens>requestObject.max_output_tokens or
       item.output_tokens~=math.floor(item.output_tokens) then
      return redis.error_reply('CORDOVA_INVALID_REQUEST_ATTEMPT')
    end
    local status=item.reservation.status
    if status=='reserved' then activeCount=activeCount+1
    elseif status=='refunded' then refundedCount=refundedCount+1
    elseif status=='settled' then settledCount=settledCount+1
    elseif status=='cancelled' then cancelledCount=cancelledCount+1
    else return redis.error_reply('CORDOVA_INVALID_ATTEMPT_STATUS') end
    if attemptID==recordObject.attempt_id then selected=item end
  end
  if attemptCount<1 or attemptCount>2 or not selected then return redis.error_reply('CORDOVA_INVALID_REQUEST_ATTEMPTS') end
  if selected.reservation.owner_id~=requestObject.owner_id then return redis.error_reply('CORDOVA_INVALID_REQUEST_OWNER') end
  local amount=selected.reservation.reserved_piconero
  local inputCost=ceilCost(requestObject.input_tokens,requestObject.price.input_rate_piconero_per_million)
  local maxOutputCost=ceilCost(requestObject.max_output_tokens,requestObject.price.output_rate_piconero_per_million)
  if not inputCost or not maxOutputCost or not validUnsigned(requestObject.price.input_rate_piconero_per_million) or
     not validUnsigned(requestObject.price.output_rate_piconero_per_million) then
    return redis.error_reply('CORDOVA_INVALID_PRICE')
  end
  local expectedReservation=addSigned(inputCost,maxOutputCost)
  if not validUnsigned(expectedReservation) or amount~=expectedReservation then return redis.error_reply('CORDOVA_RESERVATION_AMOUNT_MISMATCH') end
  local entryKind=recordObject.kind
  local expectedPostings={}
  local expectedPostingCount=0
  local function expectPosting(bucket,account,delta)
    expectedPostings[bucket..':'..account]=delta
    expectedPostingCount=expectedPostingCount+1
  end
  if requestObject.status=='running' then
    if entryKind~='reserve' or (oldRequest and oldRequest.status~='accepted') or activeCount~=1 or settledCount~=0 or cancelledCount~=0 or
       selected.reservation.status~='reserved' or recordObject.operation_id~='reserve:'..requestID..':'..recordObject.attempt_id then
      return redis.error_reply('CORDOVA_INVALID_REQUEST_TRANSITION')
    end
    if spentDelta~='0' or reservedDelta~=amount then return redis.error_reply('CORDOVA_CAP_POSTING_MISMATCH') end
    expectPosting('user.available',requestObject.account_id,amount)
    expectPosting('user.reserved',requestObject.account_id,'-'..amount)
  elseif requestObject.status=='accepted' or requestObject.status=='refunded' and entryKind=='attempt_refund' then
    if entryKind~='attempt_refund' or not oldRequest or oldRequest.status~='running' or activeCount~=0 or settledCount~=0 or cancelledCount~=0 or
       selected.reservation.status~='refunded' or selected.output_tokens~=0 or
       recordObject.operation_id~='refund:'..requestID..':'..recordObject.attempt_id then
      return redis.error_reply('CORDOVA_INVALID_REQUEST_TRANSITION')
    end
    if spentDelta~='0' or reservedDelta~='-'..amount then return redis.error_reply('CORDOVA_CAP_POSTING_MISMATCH') end
    expectPosting('user.available',requestObject.account_id,'-'..amount)
    expectPosting('user.reserved',requestObject.account_id,amount)
  elseif requestObject.status=='settled' or requestObject.status=='cancelled' then
    local isCancel=requestObject.status=='cancelled'
    if not oldRequest or oldRequest.status~='running' or activeCount~=0 or settledCount+cancelledCount~=1 or
       (isCancel and (selected.reservation.status~='cancelled' or recordObject.kind~='cancel' or recordObject.operation_id~='cancel:'..requestID)) or
       (not isCancel and (selected.reservation.status~='settled' or recordObject.kind~='settle' or recordObject.operation_id~='settle:'..requestID)) then
      return redis.error_reply('CORDOVA_INVALID_REQUEST_TRANSITION')
    end
    local outputCost=ceilCost(selected.output_tokens,requestObject.price.output_rate_piconero_per_million)
    if not outputCost then return redis.error_reply('CORDOVA_INVALID_OUTPUT_COST') end
    local actual=addSigned(inputCost,outputCost)
    if isCancel and selected.output_tokens==0 then actual='0' end
    if not validUnsigned(actual) or cmpAbs(actual,amount)>0 then return redis.error_reply('CORDOVA_SETTLEMENT_AMOUNT_MISMATCH') end
    local refund=addSigned(amount,'-'..actual)
    local ownerProduct=mulSmall(actual,requestObject.price.owner_share_bps)
    local ownerCredit=divSmall(ownerProduct,10000)
    local platformTake=addSigned(actual,'-'..ownerCredit)
    local availablePosting=actualPostings['user.available:'..requestObject.account_id]
    local ownerPosting=actualPostings['owner.pending:'..selected.reservation.owner_id]
    local equityPosting=actualPostings['equity:']
    if actualPostings['user.reserved:'..requestObject.account_id]~=amount or availablePosting~='-'..refund or
       ownerPosting~='-'..ownerCredit or equityPosting~='-'..platformTake then
      return redis.error_reply('CORDOVA_SETTLEMENT_POSTING_MISMATCH')
    end
    if not validUnsigned(actual) or spentDelta~=actual or reservedDelta~='-'..amount then return redis.error_reply('CORDOVA_CAP_POSTING_MISMATCH') end
    expectPosting('user.reserved',requestObject.account_id,amount)
    expectPosting('user.available',requestObject.account_id,'-'..refund)
    expectPosting('owner.pending',selected.reservation.owner_id,'-'..ownerCredit)
    expectPosting('equity','', '-'..platformTake)
  elseif requestObject.status=='refunded' and entryKind=='request_abandoned' then
    if not oldRequest or oldRequest.status~='accepted' or activeCount~=0 or settledCount~=0 or cancelledCount~=0 or refundedCount~=attemptCount or
       recordObject.operation_id~='abandon:'..requestID or spentDelta~='0' or reservedDelta~='0' then
      return redis.error_reply('CORDOVA_INVALID_REQUEST_TRANSITION')
    end
    expectPosting('user.available',requestObject.account_id,'0')
    expectPosting('user.reserved',requestObject.account_id,'0')
  else
    return redis.error_reply('CORDOVA_INVALID_REQUEST_STATUS')
  end
  local actualPostingCount=0
  for field,delta in pairs(actualPostings) do
    actualPostingCount=actualPostingCount+1
    if expectedPostings[field]~=delta then return redis.error_reply('CORDOVA_REQUEST_POSTING_MISMATCH') end
  end
  if actualPostingCount~=expectedPostingCount then return redis.error_reply('CORDOVA_REQUEST_POSTING_COUNT_MISMATCH') end
  terminalRequest=requestObject.status=='settled' or requestObject.status=='cancelled' or requestObject.status=='refunded'
  local requestPrefix=requestObject.account_id..':'..requestObject.api_key_id..':'..requestObject.accepted_day
  if capKey~=requestPrefix then return redis.error_reply('CORDOVA_INVALID_CAP_KEY') end
  local limitField='limit:'..capKey
  local spentField='spent:'..capKey
  local reservedField='reserved:'..capKey
  local storedLimit=redis.call('HGET',KEYS[7],limitField)
  local spent=redis.call('HGET',KEYS[7],spentField) or '0'
  local reserved=redis.call('HGET',KEYS[7],reservedField) or '0'
  if storedLimit and not validUnsigned(storedLimit) or not validUnsigned(spent) or not validUnsigned(reserved) then
    return redis.error_reply('CORDOVA_INVALID_CAP_STATE')
  end
  effectiveLimit=capLimit
  if storedLimit and cmpAbs(storedLimit,effectiveLimit)<0 then effectiveLimit=storedLimit end
  nextSpent=addSigned(spent,spentDelta)
  nextReserved=addSigned(reserved,reservedDelta)
  if not validUnsigned(nextSpent) or not validUnsigned(nextReserved) or
     cmpAbs(nextSpent,effectiveLimit)>0 or cmpAbs(nextReserved,addSigned(effectiveLimit,'-'..nextSpent))>0 then
    return {-4,'spend cap exceeded'}
  end
else
  return redis.error_reply('CORDOVA_INVALID_REQUEST_FLAG')
end

for field,value in pairs(updates) do redis.call('HSET',KEYS[2],field,value) end
redis.call('XADD',KEYS[4],'*','entry',record)
redis.call('HSET',KEYS[3],'fp:'..operation,fingerprint,'entry:'..operation,record)
redis.call('SET',KEYS[1],newHash)
redis.call('XADD',KEYS[5],'*','event',event)
if hasRequest=='1' then
  redis.call('HSET',KEYS[6],requestField,requestJSON,revisionField,newRevision)
  redis.call('HSET',KEYS[7],'limit:'..capKey,effectiveLimit,'spent:'..capKey,nextSpent,'reserved:'..capKey,nextReserved)
  if terminalRequest then
    redis.call('SADD',KEYS[8],requestID)
  end
end
return {1,record}
`
