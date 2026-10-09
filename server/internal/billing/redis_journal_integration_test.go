package billing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestRedisJournalIntegrationFaultInjectedCrashBoundaries(t *testing.T) {
	client := integrationRedisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	journal := integrationJournal(t, client)
	ledger, err := NewRedisLedger(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Recover(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	account, key, ownerOne, ownerTwo := testID(t), testID(t), testID(t), testID(t)
	creditAmount, _ := domain.NewPiconero(1_000_000_000)
	now := time.Now().UTC().Truncate(time.Second)
	price := testPrice(t, 1_000_000, 2_000_000, 5_000)
	cap, _ := domain.NewPiconero(1_000_000_000)

	// The credit is durable, but the worker loses the response after WAITAOF.
	creditID := "fault-boundary-credit"
	creditOp := "test-credit:" + creditID
	creditFaultFired := armJournalFault(journal, redisFaultAfterAOFBarrier, creditOp)
	if _, err := ledger.CreditTest(ctx, account, creditID, creditAmount, now); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("credit response-loss error=%v", err)
	}
	if !creditFaultFired() {
		t.Fatal("credit fault point was not reached")
	}
	journal.faultInjector = nil
	ledger = restartIntegrationLedger(t, ctx, client, journal, now.Add(time.Second))
	journal = ledger.journal
	credit, err := ledger.CreditTest(ctx, account, creditID, creditAmount, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("idempotent credit replay failed: %v", err)
	}
	if balance, err := ledger.Balance(ctx, account); err != nil || balance.Available.Int64() != creditAmount.Int64() || balance.Reserved.Int64() != 0 {
		t.Fatalf("credit replay changed balance: balance=%+v err=%v", balance, err)
	}
	if stored, err := ledger.Operation(ctx, creditOp); err != nil || stored.EntryHash != credit.EntryHash {
		t.Fatalf("credit tombstone changed after replay: stored=%+v err=%v", stored, err)
	}

	newParams := func(requestID, attemptID, ownerID string, acceptedAt time.Time) ReserveParams {
		return ReserveParams{RequestID: requestID, AttemptID: attemptID, ModelID: defaultModelID, AccountID: account, APIKeyID: key,
			OwnerID: ownerID, InputTokens: 2, MaxOutputTokens: 10, DailySpendCap: cap, Price: price, AcceptedAt: acceptedAt}
	}

	// A crash after the Lua transaction but before its durability acknowledgement
	// leaves an orphan reservation; startup recovery refunds it exactly once.
	requestID, attemptID := testID(t), testID(t)
	params := newParams(requestID, attemptID, ownerOne, now.Add(2*time.Second))
	reserveFaultFired := armJournalFault(journal, redisFaultAfterAtomicAppend, "reserve:"+requestID+":"+attemptID)
	if _, _, err := ledger.Reserve(ctx, params); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("post-append reserve error=%v", err)
	}
	if !reserveFaultFired() {
		t.Fatal("reserve fault point was not reached")
	}
	journal.faultInjector = nil
	ledger = restartIntegrationLedger(t, ctx, client, journal, now.Add(3*time.Second))
	journal = ledger.journal
	request, _, err := ledger.Request(ctx, requestID)
	if err != nil || request.Status != RequestRefunded {
		t.Fatalf("orphan reserve recovery status=%s err=%v", request.Status, err)
	}
	if balance, err := ledger.Balance(ctx, account); err != nil || balance.Available.Int64() != creditAmount.Int64() || balance.Reserved.Int64() != 0 {
		t.Fatalf("orphan reserve leaked funds: balance=%+v err=%v", balance, err)
	}

	// A lost refund response must close the abandoned retry window on restart.
	refundRequest, refundAttempt := testID(t), testID(t)
	params = newParams(refundRequest, refundAttempt, ownerOne, now.Add(4*time.Second))
	if _, _, err := ledger.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	refundFaultFired := armJournalFault(journal, redisFaultAfterAOFBarrier, "refund:"+refundRequest+":"+refundAttempt)
	if _, err := ledger.FailAttempt(ctx, refundRequest, refundAttempt, true, now.Add(5*time.Second)); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("refund response-loss error=%v", err)
	}
	if !refundFaultFired() {
		t.Fatal("refund fault point was not reached")
	}
	journal.faultInjector = nil
	ledger = restartIntegrationLedger(t, ctx, client, journal, now.Add(6*time.Second))
	journal = ledger.journal
	request, _, err = ledger.Request(ctx, refundRequest)
	if err != nil || request.Status != RequestRefunded {
		t.Fatalf("retry-window recovery status=%s err=%v", request.Status, err)
	}
	if balance, err := ledger.Balance(ctx, account); err != nil || balance.Available.Int64() != creditAmount.Int64() || balance.Reserved.Int64() != 0 {
		t.Fatalf("refund recovery leaked funds: balance=%+v err=%v", balance, err)
	}

	// A committed settlement remains terminal when the response is lost.
	settleRequest, settleAttempt := testID(t), testID(t)
	params = newParams(settleRequest, settleAttempt, ownerOne, now.Add(7*time.Second))
	reservation, _, err := ledger.Reserve(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	settleFaultFired := armJournalFault(journal, redisFaultAfterAOFBarrier, "settle:"+settleRequest)
	if _, err := ledger.Settle(ctx, settleRequest, settleAttempt, 3, false, now.Add(8*time.Second)); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("settlement response-loss error=%v", err)
	}
	if !settleFaultFired() {
		t.Fatal("settlement fault point was not reached")
	}
	journal.faultInjector = nil
	ledger = restartIntegrationLedger(t, ctx, client, journal, now.Add(9*time.Second))
	journal = ledger.journal
	request, _, err = ledger.Request(ctx, settleRequest)
	settledQuote, quoteErr := CalculateQuote(2, 3, 10, price, false)
	if err != nil || quoteErr != nil || request.Status != RequestSettled {
		t.Fatalf("settlement recovery status=%s quote_err=%v err=%v", request.Status, quoteErr, err)
	}
	if balance, err := ledger.Balance(ctx, account); err != nil || balance.Available.Int64() != creditAmount.Int64()-settledQuote.ActualCost.Int64() || balance.Reserved.Int64() != 0 || reservation.Amount == "" {
		t.Fatalf("settlement recovery balance=%+v err=%v", balance, err)
	}

	// Cancellation applies the durable client-visible prefix once.
	cancelRequest, cancelAttempt := testID(t), testID(t)
	params = newParams(cancelRequest, cancelAttempt, ownerTwo, now.Add(10*time.Second))
	if _, _, err := ledger.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	cancelFaultFired := armJournalFault(journal, redisFaultAfterAOFBarrier, "cancel:"+cancelRequest)
	if _, err := ledger.Settle(ctx, cancelRequest, cancelAttempt, 1, true, now.Add(11*time.Second)); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("cancel response-loss error=%v", err)
	}
	if !cancelFaultFired() {
		t.Fatal("cancellation fault point was not reached")
	}
	journal.faultInjector = nil
	ledger = restartIntegrationLedger(t, ctx, client, journal, now.Add(12*time.Second))
	journal = ledger.journal
	request, _, err = ledger.Request(ctx, cancelRequest)
	cancelQuote, quoteErr := CalculateQuote(2, 1, 10, price, true)
	if err != nil || quoteErr != nil || request.Status != RequestCancelled {
		t.Fatalf("cancellation recovery status=%s quote_err=%v err=%v", request.Status, quoteErr, err)
	}
	if balance, err := ledger.Balance(ctx, account); err != nil || balance.Available.Int64() != creditAmount.Int64()-settledQuote.ActualCost.Int64()-cancelQuote.ActualCost.Int64() || balance.Reserved.Int64() != 0 {
		t.Fatalf("cancellation recovery balance=%+v err=%v", balance, err)
	}

	// If the zero-value abandon record is committed before a crash, replay does
	// not append a second terminal record or change any balance.
	abandonRequest, abandonAttempt := testID(t), testID(t)
	params = newParams(abandonRequest, abandonAttempt, ownerOne, now.Add(13*time.Second))
	if _, _, err := ledger.Reserve(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.FailAttempt(ctx, abandonRequest, abandonAttempt, true, now.Add(14*time.Second)); err != nil {
		t.Fatal(err)
	}
	abandonFaultFired := armJournalFault(journal, redisFaultAfterAtomicAppend, "abandon:"+abandonRequest)
	if _, err := ledger.AbandonRetry(ctx, abandonRequest, now.Add(15*time.Second)); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("abandon post-append error=%v", err)
	}
	if !abandonFaultFired() {
		t.Fatal("abandon fault point was not reached")
	}
	journal.faultInjector = nil
	ledger = restartIntegrationLedger(t, ctx, client, journal, now.Add(16*time.Second))
	journal = ledger.journal
	request, _, err = ledger.Request(ctx, abandonRequest)
	if err != nil || request.Status != RequestRefunded {
		t.Fatalf("abandon recovery status=%s err=%v", request.Status, err)
	}
	if err := ledger.journal.Verify(ctx); err != nil {
		t.Fatalf("fault-boundary journal replay failed: %v", err)
	}
}

// TestRedisJournalIntegrationRedisProcessRestartAfterAppend owns the Redis
// process it terminates. It verifies AOF replay and ledger recovery after the
// server disappears after the atomic append response but before WAITAOF.
func TestRedisJournalIntegrationRedisProcessRestartAfterAppend(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("CORDOVA_TEST_REDIS_SERVER"))
	if binary == "" {
		t.Skip("set CORDOVA_TEST_REDIS_SERVER to an isolated redis-server binary")
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		t.Fatalf("CORDOVA_TEST_REDIS_SERVER is not an executable file: %q", binary)
	}

	dataDir := t.TempDir()
	socketID := testID(t)
	socket := filepath.Join("/tmp", "cv-redis-"+strings.ReplaceAll(socketID[:8], "-", "")+".sock")
	process := startCrashTestRedis(t, binary, dataDir, socket)
	client := crashTestRedisClient(t, socket)
	journal := integrationJournal(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ledger, err := NewRedisLedger(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Recover(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	accountID, keyID, ownerID := testID(t), testID(t), testID(t)
	creditAmount, _ := domain.NewPiconero(10_000_000)
	if _, err := ledger.CreditTest(ctx, accountID, "redis-process-crash-credit", creditAmount, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	requestID, attemptID := testID(t), testID(t)
	price := testPrice(t, 1_000_000, 1_000_000, 5_000)
	capAmount, _ := domain.NewPiconero(10_000_000)
	params := ReserveParams{RequestID: requestID, AttemptID: attemptID, ModelID: defaultModelID, AccountID: accountID,
		APIKeyID: keyID, OwnerID: ownerID, InputTokens: 1, MaxOutputTokens: 2, DailySpendCap: capAmount,
		Price: price, AcceptedAt: time.Now().UTC()}

	journal.faultInjector = func(stage redisJournalFaultStage, operationID string) error {
		if stage != redisFaultAfterAtomicAppend || operationID != "reserve:"+requestID+":"+attemptID {
			return nil
		}
		return errors.Join(errors.New("simulated server loss before WAITAOF"), process.kill())
	}
	if _, _, err := ledger.Reserve(ctx, params); !errors.Is(err, ErrJournalUncertain) {
		t.Fatalf("reserve after Redis process loss error=%v", err)
	}
	journal.faultInjector = nil
	if err := process.wait(); err != nil && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("Redis process exit=%v", err)
	}

	restarted := startCrashTestRedis(t, binary, dataDir, socket)
	newClient := crashTestRedisClient(t, socket)
	newJournal, err := NewRedisJournal(newClient, false)
	if err != nil {
		t.Fatal(err)
	}
	newJournal.stream, newJournal.outbox, newJournal.headKey = journal.stream, journal.outbox, journal.headKey
	newJournal.balancesKey, newJournal.operationsKey, newJournal.requestsKey = journal.balancesKey, journal.operationsKey, journal.requestsKey
	newJournal.capsKey, newJournal.barrierKey, newJournal.usagePendingKey = journal.capsKey, journal.barrierKey, journal.usagePendingKey
	newLedger, err := NewRedisLedger(newJournal)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := newLedger.Recover(ctx, time.Now().UTC().Add(time.Second)); err != nil || recovered != 1 {
		t.Fatalf("post-restart recovered=%d err=%v", recovered, err)
	}
	checkpoint, _, err := newLedger.Request(ctx, requestID)
	if err != nil || checkpoint.Status != RequestRefunded {
		t.Fatalf("post-restart request status=%s err=%v", checkpoint.Status, err)
	}
	balance, err := newLedger.Balance(ctx, accountID)
	if err != nil || balance.Available.Int64() != creditAmount.Int64() || balance.Reserved.Int64() != 0 {
		t.Fatalf("post-restart balance=%+v err=%v", balance, err)
	}
	if err := newJournal.Verify(ctx); err != nil {
		t.Fatalf("post-restart journal verification: %v", err)
	}
	if err := restarted.kill(); err != nil && !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("stop restarted Redis: %v", err)
	}
}

type crashTestRedisProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	killOnce sync.Once
}

func startCrashTestRedis(t *testing.T, binary, dataDir, socket string) *crashTestRedisProcess {
	t.Helper()
	cmd := exec.Command(binary, "--port", "0", "--unixsocket", socket, "--unixsocketperm", "700",
		"--dir", dataDir, "--appendonly", "yes", "--appendfsync", "always", "--save", "",
		"--daemonize", "no", "--logfile", filepath.Join(dataDir, "redis.log"))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start isolated Redis: %v", err)
	}
	process := &crashTestRedisProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		_ = process.kill()
		_ = process.wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket, MaxRetries: 0})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := client.Ping(ctx).Err()
		cancel()
		_ = client.Close()
		if err == nil {
			return process
		}
		select {
		case <-process.done:
			log, _ := os.ReadFile(filepath.Join(dataDir, "redis.log"))
			t.Fatalf("Redis exited during startup: %v: %s", process.waitErr, strings.TrimSpace(string(log)))
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("isolated Redis did not become ready; see %s", filepath.Join(dataDir, "redis.log"))
	return nil
}

func (p *crashTestRedisProcess) kill() error {
	var result error
	p.killOnce.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		result = p.cmd.Process.Kill()
	})
	return result
}

func (p *crashTestRedisProcess) wait() error {
	select {
	case <-p.done:
		return p.waitErr
	case <-time.After(5 * time.Second):
		return errors.New("Redis process did not exit")
	}
}

func crashTestRedisClient(t *testing.T, socket string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket, MaxRetries: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("isolated Redis is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func armJournalFault(journal *RedisJournal, stage redisJournalFaultStage, operationID string) func() bool {
	fired := false
	journal.faultInjector = func(actualStage redisJournalFaultStage, actualOperationID string) error {
		if !fired && actualStage == stage && actualOperationID == operationID {
			fired = true
			return errors.New("injected worker crash")
		}
		return nil
	}
	return func() bool { return fired }
}

func restartIntegrationLedger(t *testing.T, ctx context.Context, client *redis.Client, previous *RedisJournal, at time.Time) *RedisLedger {
	t.Helper()
	journal, err := NewRedisJournal(client, false)
	if err != nil {
		t.Fatal(err)
	}
	journal.stream, journal.outbox, journal.headKey = previous.stream, previous.outbox, previous.headKey
	journal.balancesKey, journal.operationsKey, journal.requestsKey = previous.balancesKey, previous.operationsKey, previous.requestsKey
	journal.capsKey, journal.barrierKey, journal.usagePendingKey = previous.capsKey, previous.barrierKey, previous.usagePendingKey
	ledger, err := NewRedisLedger(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Recover(ctx, at); err != nil {
		t.Fatalf("restart recovery failed: %v", err)
	}
	return ledger
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
