package billing

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"

	"github.com/redis/go-redis/v9"
)

// These tests execute the financial Lua script and WAITAOF against an actual
// local Redis instance. They are skipped by the normal unit suite; M3 sign-off
// runs them through make test-ledger-redis with an isolated test Redis URL.
func TestRedisJournalIntegrationRetryRecoveryAndPrecision(t *testing.T) {
	client := integrationRedisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journal := integrationJournal(t, client)
	ledger, err := NewRedisLedger(journal)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Ready() {
		t.Fatal("journal became ready before startup recovery")
	}
	if recovered, err := ledger.Recover(ctx, time.Now().UTC()); err != nil || recovered != 0 {
		t.Fatalf("initial recovery count=%d err=%v", recovered, err)
	}

	account, key, ownerOne, ownerTwo := testID(t), testID(t), testID(t), testID(t)
	creditAmount, _ := domain.NewPiconero(9_007_199_254_740_993)
	creditAt := time.Now().UTC().Add(-time.Minute)
	credit, err := ledger.CreditTest(ctx, account, "integration-credit", creditAmount, creditAt)
	if err != nil {
		t.Fatal(err)
	}
	creditAgain, err := ledger.CreditTest(ctx, account, "integration-credit", creditAmount, creditAt.Add(time.Hour))
	if err != nil || creditAgain.EntryHash != credit.EntryHash {
		t.Fatalf("credit replay changed the journal: entry=%+v err=%v", creditAgain, err)
	}

	now := time.Now().UTC()
	capAmount, _ := domain.NewPiconero(creditAmount.Int64())
	price := testPrice(t, 1_000_000, 2_000_000, 5_000)
	requestID, attemptOne, attemptTwo := testID(t), testID(t), testID(t)
	params := ReserveParams{RequestID: requestID, AttemptID: attemptOne, ModelID: defaultModelID, AccountID: account, APIKeyID: key,
		OwnerID: ownerOne, InputTokens: 2, MaxOutputTokens: 10, DailySpendCap: capAmount, Price: price, AcceptedAt: now}
	_, _, err = ledger.Reserve(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.FailAttempt(ctx, requestID, attemptOne, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	params.AttemptID, params.OwnerID = attemptTwo, ownerTwo
	_, _, err = ledger.Reserve(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Settle(ctx, requestID, attemptTwo, 3, false, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	quote, err := CalculateQuote(2, 3, 10, price, false)
	if err != nil {
		t.Fatal(err)
	}

	cancelRequest, cancelAttempt := testID(t), testID(t)
	params.RequestID, params.AttemptID, params.OwnerID = cancelRequest, cancelAttempt, ownerOne
	params.AcceptedAt = now.Add(3 * time.Second)
	if _, _, err := ledger.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Settle(ctx, cancelRequest, cancelAttempt, 1, true, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	cancelQuote, err := CalculateQuote(2, 1, 10, price, true)
	if err != nil {
		t.Fatal(err)
	}

	retryWindowRequest, retryWindowAttempt := testID(t), testID(t)
	params.RequestID, params.AttemptID, params.OwnerID = retryWindowRequest, retryWindowAttempt, ownerOne
	params.AcceptedAt = now.Add(5 * time.Second)
	if _, _, err := ledger.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.FailAttempt(ctx, retryWindowRequest, retryWindowAttempt, true, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.AbandonRetry(ctx, retryWindowRequest, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.AbandonRetry(ctx, retryWindowRequest, now.Add(8*time.Second)); err != nil {
		t.Fatalf("retry-window abandonment was not idempotent: %v", err)
	}

	// A second process view must recover a durable orphan reservation without
	// replaying request content or refunding the already committed settlement.
	orphanRequest, orphanAttempt := testID(t), testID(t)
	params.RequestID, params.AttemptID, params.OwnerID = orphanRequest, orphanAttempt, ownerOne
	params.AcceptedAt = now.Add(9 * time.Second)
	if _, _, err := ledger.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	secondJournal, err := NewRedisJournal(client, false)
	if err != nil {
		t.Fatal(err)
	}
	secondJournal.stream, secondJournal.outbox, secondJournal.headKey = journal.stream, journal.outbox, journal.headKey
	secondJournal.balancesKey, secondJournal.operationsKey = journal.balancesKey, journal.operationsKey
	secondJournal.requestsKey, secondJournal.capsKey, secondJournal.barrierKey = journal.requestsKey, journal.capsKey, journal.barrierKey
	secondJournal.usagePendingKey = journal.usagePendingKey
	secondLedger, err := NewRedisLedger(secondJournal)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := secondLedger.Recover(ctx, now.Add(time.Minute)); err != nil || recovered != 1 {
		t.Fatalf("orphan recovery count=%d err=%v", recovered, err)
	}
	if recovered, err := secondLedger.Recover(ctx, now.Add(2*time.Minute)); err != nil || recovered != 0 {
		t.Fatalf("repeated recovery count=%d err=%v", recovered, err)
	}
	checkpoint, _, err := secondLedger.Request(ctx, requestID)
	if err != nil || checkpoint.Status != RequestSettled {
		t.Fatalf("committed settlement changed during recovery: status=%s err=%v", checkpoint.Status, err)
	}
	pendingUsage, err := secondLedger.PendingUsageRequestIDs(ctx)
	containsRequest := false
	containsCancelled := false
	containsOrphan := false
	for _, pendingID := range pendingUsage {
		containsRequest = containsRequest || pendingID == requestID
		containsCancelled = containsCancelled || pendingID == cancelRequest
		containsOrphan = containsOrphan || pendingID == orphanRequest
	}
	if err != nil || !containsRequest || !containsCancelled || !containsOrphan {
		t.Fatalf("terminal usage recovery queue=%v err=%v", pendingUsage, err)
	}
	cancelled, _, err := secondLedger.Request(ctx, cancelRequest)
	if err != nil || cancelled.Status != RequestCancelled {
		t.Fatalf("cancelled request status=%s err=%v", cancelled.Status, err)
	}
	orphan, _, err := secondLedger.Request(ctx, orphanRequest)
	if err != nil || orphan.Status != RequestRefunded {
		t.Fatalf("orphan request status=%s err=%v", orphan.Status, err)
	}
	accountBalance, err := secondLedger.Balance(ctx, account)
	if err != nil || accountBalance.Available.Int64() != creditAmount.Int64()-quote.ActualCost.Int64()-cancelQuote.ActualCost.Int64() || accountBalance.Reserved.Int64() != 0 {
		t.Fatalf("recovered balance=%+v err=%v", accountBalance, err)
	}
	if err := secondJournal.Verify(ctx); err != nil {
		t.Fatalf("journal replay failed: %v", err)
	}
	if !secondJournal.Ready() {
		t.Fatal("journal is not ready after successful recovery")
	}
}

func TestRedisJournalIntegrationRejectsWrongKeyTypeBeforeMutation(t *testing.T) {
	client := integrationRedisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	journal := integrationJournal(t, client)
	if err := client.Set(ctx, journal.balancesKey, "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	journal.recovered.Store(true)
	account := testID(t)
	entry := Entry{OperationID: "integration-wrong-type", Kind: "test_credit", CreatedAt: time.Now().UTC(), Postings: []Posting{
		posting(BucketCashHot, "", 1), posting(BucketAvailable, account, -1),
	}}
	if _, err := journal.Append(ctx, entry); err == nil || !strings.Contains(err.Error(), "CORDOVA_WRONG_LEDGER_KEY_TYPE") {
		t.Fatalf("wrong key type error=%v", err)
	}
	if count, err := client.XLen(ctx, journal.stream).Result(); err != nil || count != 0 {
		t.Fatalf("wrong-type rejection mutated the journal: length=%d err=%v", count, err)
	}
	if count, err := client.HLen(ctx, journal.operationsKey).Result(); err != nil || count != 0 {
		t.Fatalf("wrong-type rejection wrote operation tombstones: count=%d err=%v", count, err)
	}
}

func integrationRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("CORDOVA_TEST_REDIS_URL"))
	if raw == "" {
		t.Skip("set CORDOVA_TEST_REDIS_URL to run Redis Lua and WAITAOF integration tests")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("invalid CORDOVA_TEST_REDIS_URL: %v", err)
	}
	if options.Network == "unix" {
		if !filepath.IsAbs(options.Addr) {
			t.Fatal("CORDOVA_TEST_REDIS_URL must use an absolute local Redis socket path")
		}
	} else {
		host, _, err := net.SplitHostPort(options.Addr)
		if err != nil || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
			t.Fatal("CORDOVA_TEST_REDIS_URL must point to local Redis; the test writes isolated temporary keys")
		}
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("local test Redis is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func integrationJournal(t *testing.T, client *redis.Client) *RedisJournal {
	t.Helper()
	id := testID(t)
	prefix := fmt.Sprintf("cv:test:m3:%s", id)
	journal, err := NewRedisJournal(client, false)
	if err != nil {
		t.Fatal(err)
	}
	journal.stream = prefix + ":ledger"
	journal.outbox = prefix + ":outbox"
	journal.headKey = prefix + ":head"
	journal.balancesKey = prefix + ":balances"
	journal.operationsKey = prefix + ":operations"
	journal.requestsKey = prefix + ":requests"
	journal.capsKey = prefix + ":caps"
	journal.barrierKey = prefix + ":barrier"
	journal.usagePendingKey = prefix + ":usage-pending"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = client.Del(ctx, journal.stream, journal.outbox, journal.headKey, journal.balancesKey,
			journal.operationsKey, journal.requestsKey, journal.capsKey, journal.barrierKey, journal.usagePendingKey).Err()
	})
	return journal
}
