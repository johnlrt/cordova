package billing

import (
	"encoding/json"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"cordova/server/internal/domain"
)

func TestCalculateQuoteUsesIntegerRoundingAndZeroOutputCancellation(t *testing.T) {
	price := testPrice(t, 1, 2, 3_333)
	quote, err := CalculateQuote(1, 1, 2, price, false)
	if err != nil {
		t.Fatal(err)
	}
	if quote.InputCost.Int64() != 1 || quote.OutputCost.Int64() != 1 || quote.Reservation.Int64() != 2 || quote.ActualCost.Int64() != 2 {
		t.Fatalf("unexpected rounded quote: %+v", quote)
	}
	if quote.OwnerCredit.Int64() != 0 || quote.PlatformTake.Int64() != 2 || quote.Refund.Int64() != 0 {
		t.Fatalf("unexpected share/refund: %+v", quote)
	}
	cancelled, err := CalculateQuote(17, 0, 10, price, true)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.ActualCost.Int64() != 0 || cancelled.OwnerCredit.Int64() != 0 || cancelled.Refund.Int64() != cancelled.Reservation.Int64() {
		t.Fatalf("empty cancellation must refund the full reservation: %+v", cancelled)
	}
}

func TestLedgerRetrySettlementAndIdempotency(t *testing.T) {
	l := NewLedger()
	now := time.Date(2026, 10, 9, 23, 59, 0, 0, time.UTC)
	account, key, ownerOne, ownerTwo := testID(t), testID(t), testID(t), testID(t)
	request, attemptOne, attemptTwo := testID(t), testID(t), testID(t)
	amount, _ := domain.NewPiconero(1_000)
	if _, err := l.CreditTest(account, "credit-1", amount, now); err != nil {
		t.Fatal(err)
	}
	price := testPrice(t, 1_000_000, 2_000_000, 1_000)
	capAmount, _ := domain.NewPiconero(500)
	p := ReserveParams{RequestID: request, AttemptID: attemptOne, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: ownerOne,
		InputTokens: 5, MaxOutputTokens: 10, DailySpendCap: capAmount, Price: price, AcceptedAt: now}
	reserved, reserveEntry, err := l.Reserve(p)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Amount != "25" {
		t.Fatalf("reservation = %s, want 25", reserved.Amount)
	}
	replayed, repeated, err := l.Reserve(p)
	if err != nil || replayed != reserved || repeated.EntryHash != reserveEntry.EntryHash {
		t.Fatalf("idempotent reserve: reservation=%+v entry=%+v err=%v", replayed, repeated, err)
	}
	bal, err := l.Balance(account)
	if err != nil || bal.Available.Int64() != 975 || bal.Reserved.Int64() != 25 {
		t.Fatalf("after reserve balance=%+v err=%v", bal, err)
	}
	refund, err := l.FailAttempt(request, attemptOne, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	refundAgain, err := l.FailAttempt(request, attemptOne, true, now.Add(time.Minute))
	if err != nil || refundAgain.EntryHash != refund.EntryHash {
		t.Fatalf("idempotent attempt refund: %v", err)
	}
	p.AttemptID = attemptTwo
	if _, _, err = l.Reserve(p); !errors.Is(err, ErrRetryUnavailable) {
		t.Fatalf("same owner retry error=%v", err)
	}
	p.OwnerID = ownerTwo
	p.AcceptedAt = now.Add(2 * time.Minute) // Retry is charged to the original acceptance day.
	reservation, _, err := l.Reserve(p)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.Amount != "25" {
		t.Fatalf("retry reservation = %s", reservation.Amount)
	}
	settlement, err := l.Settle(request, attemptTwo, 3, false, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	settlementAgain, err := l.Settle(request, attemptTwo, 3, false, now.Add(time.Hour))
	if err != nil || settlementAgain.EntryHash != settlement.EntryHash {
		t.Fatalf("idempotent settlement: %v", err)
	}
	if _, err = l.Settle(request, attemptTwo, 4, false, now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("different settlement replay error=%v", err)
	}
	bal, err = l.Balance(account)
	if err != nil || bal.Available.Int64() != 989 || bal.Reserved.Int64() != 0 {
		t.Fatalf("after settle balance=%+v err=%v", bal, err)
	}
	ownerBalance, err := l.Balance(ownerTwo)
	if err != nil || ownerBalance.OwnerPending.Int64() != 1 {
		t.Fatalf("owner balance=%+v err=%v", ownerBalance, err)
	}
	entries := l.Entries()
	assertJournal(t, entries)
	if len(entries) != 5 {
		t.Fatalf("ledger entries=%d, want credit, reserve, refund, reserve, settle", len(entries))
	}
}

func TestLedgerConcurrentSpendCapAndSettlementRace(t *testing.T) {
	l := NewLedger()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	account, key, owner := testID(t), testID(t), testID(t)
	credit, _ := domain.NewPiconero(100)
	if _, err := l.CreditTest(account, "credit", credit, now); err != nil {
		t.Fatal(err)
	}
	capAmount, _ := domain.NewPiconero(50)
	price := testPrice(t, 0, 1_000_000, 10_000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	var request, attempt string
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := ReserveParams{RequestID: testID(t), AttemptID: testID(t), ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: owner,
				InputTokens: 0, MaxOutputTokens: 30, DailySpendCap: capAmount, Price: price, AcceptedAt: now}
			r, _, err := l.Reserve(p)
			if err == nil {
				mu.Lock()
				success++
				request = r.RequestID
				attempt = r.AttemptID
				mu.Unlock()
			} else if !errors.Is(err, ErrSpendCapExceeded) {
				t.Errorf("reserve error=%v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("successful reservations=%d, want 1", success)
	}
	var settleErr, failErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, settleErr = l.Settle(request, attempt, 3, false, now.Add(time.Second)) }()
	go func() { defer wg.Done(); _, failErr = l.FailAttempt(request, attempt, false, now.Add(time.Second)) }()
	wg.Wait()
	if (settleErr == nil) == (failErr == nil) {
		t.Fatalf("exactly one terminal operation must win; settle=%v fail=%v", settleErr, failErr)
	}
	if settleErr != nil && !errors.Is(settleErr, ErrAlreadyTerminal) {
		t.Fatalf("unexpected settle race result: %v", settleErr)
	}
	if failErr != nil && !errors.Is(failErr, ErrAlreadyTerminal) {
		t.Fatalf("unexpected refund race result: %v", failErr)
	}
	assertJournal(t, l.Entries())
	bal, err := l.Balance(account)
	if err != nil || bal.Reserved.Int64() != 0 || bal.Available.Int64() < 70 || bal.Available.Int64() > 100 {
		t.Fatalf("terminal balances=%+v err=%v", bal, err)
	}
}

func TestLedgerRecoveryRefundsOrphanAndJournalConservesMoney(t *testing.T) {
	l := NewLedger()
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	account, key, owner, request, attempt := testID(t), testID(t), testID(t), testID(t), testID(t)
	credit, _ := domain.NewPiconero(1_000)
	if _, err := l.CreditTest(account, "credit", credit, now); err != nil {
		t.Fatal(err)
	}
	capAmount, _ := domain.NewPiconero(1_000)
	p := ReserveParams{RequestID: request, AttemptID: attempt, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: owner,
		InputTokens: 10, MaxOutputTokens: 20, DailySpendCap: capAmount, Price: testPrice(t, 1_000_000, 1_000_000, 5_000), AcceptedAt: now}
	if _, _, err := l.Reserve(p); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(l.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var saved LedgerSnapshot
	if err := json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	l, err = Restore(saved)
	if err != nil {
		t.Fatalf("restore before orphan recovery: %v", err)
	}
	recovered, err := l.Recover(now.Add(time.Minute))
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	if recovered, err = l.Recover(now.Add(2 * time.Minute)); err != nil || recovered != 0 {
		t.Fatalf("second recovery recovered=%d err=%v", recovered, err)
	}
	encoded, err = json.Marshal(l.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	if l, err = Restore(saved); err != nil {
		t.Fatalf("restore after orphan recovery: %v", err)
	}
	bal, err := l.Balance(account)
	if err != nil || bal.Available.Int64() != 1_000 || bal.Reserved.Int64() != 0 {
		t.Fatalf("recovered balance=%+v err=%v", bal, err)
	}
	assertJournal(t, l.Entries())
	saved.Entries[0].Postings[0].Delta = "999"
	if _, err := Restore(saved); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("tampered snapshot error=%v", err)
	}
}

func TestRecoveryAbandonsRetryWindowAndPreservesCommittedSettlement(t *testing.T) {
	ledger := NewLedger()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	account, key, owner, request, attempt := testID(t), testID(t), testID(t), testID(t), testID(t)
	credit, _ := domain.NewPiconero(500)
	capAmount, _ := domain.NewPiconero(500)
	if _, err := ledger.CreditTest(account, "credit-abandon", credit, now); err != nil {
		t.Fatal(err)
	}
	params := ReserveParams{RequestID: request, AttemptID: attempt, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: owner,
		InputTokens: 1, MaxOutputTokens: 10, DailySpendCap: capAmount, Price: testPrice(t, 1_000_000, 1_000_000, 5_000), AcceptedAt: now}
	if _, _, err := ledger.Reserve(params); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.FailAttempt(request, attempt, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	ledger, err := Restore(ledger.Snapshot())
	if err != nil {
		t.Fatalf("restore failed retry window: %v", err)
	}
	recovered, err := ledger.Recover(now.Add(2 * time.Second))
	if err != nil || recovered != 1 {
		t.Fatalf("recovery did not close the retry window: recovered=%d err=%v", recovered, err)
	}
	checkpoint := ledger.Snapshot().Requests[request]
	if checkpoint.Status != RequestRefunded {
		t.Fatalf("retry-window request remained %q after restart", checkpoint.Status)
	}
	if recovered, err = ledger.Recover(now.Add(3 * time.Second)); err != nil || recovered != 0 {
		t.Fatalf("recovery was not idempotent: recovered=%d err=%v", recovered, err)
	}
	if _, err := Restore(ledger.Snapshot()); err != nil {
		t.Fatalf("restore after abandonment failed: %v", err)
	}

	settledRequest, settledAttempt := testID(t), testID(t)
	params.RequestID, params.AttemptID = settledRequest, settledAttempt
	if _, _, err := ledger.Reserve(params); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Settle(settledRequest, settledAttempt, 2, false, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	ledger, err = Restore(ledger.Snapshot())
	if err != nil {
		t.Fatalf("restore committed settlement failed: %v", err)
	}
	if recovered, err := ledger.Recover(now.Add(5 * time.Second)); err != nil || recovered != 0 {
		t.Fatalf("recovery changed a committed settlement: recovered=%d err=%v", recovered, err)
	}
	if ledger.Snapshot().Requests[settledRequest].Status != RequestSettled {
		t.Fatal("durably settled request was not preserved")
	}
}

func TestCancellationChargesOnlyVerifiedPrefixAndRetryCanLoseFundsRace(t *testing.T) {
	ledger := NewLedger()
	now := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	account, key, owner1, owner2, owner3 := testID(t), testID(t), testID(t), testID(t), testID(t)
	request, attempt1, attempt2, otherRequest, otherAttempt := testID(t), testID(t), testID(t), testID(t), testID(t)
	credit, _ := domain.NewPiconero(30)
	capAmount, _ := domain.NewPiconero(100)
	if _, err := ledger.CreditTest(account, "credit-cancel-race", credit, now); err != nil {
		t.Fatal(err)
	}
	price := testPrice(t, 0, 1_000_000, 10_000)
	params := ReserveParams{RequestID: request, AttemptID: attempt1, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: owner1,
		InputTokens: 0, MaxOutputTokens: 15, DailySpendCap: capAmount, Price: price, AcceptedAt: now}
	if _, _, err := ledger.Reserve(params); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.FailAttempt(request, attempt1, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	other := ReserveParams{RequestID: otherRequest, AttemptID: otherAttempt, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: owner3,
		InputTokens: 0, MaxOutputTokens: 20, DailySpendCap: capAmount, Price: price, AcceptedAt: now.Add(2 * time.Second)}
	if _, _, err := ledger.Reserve(other); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Settle(otherRequest, otherAttempt, 20, false, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	params.AttemptID, params.OwnerID, params.AcceptedAt = attempt2, owner2, now.Add(time.Minute)
	if _, _, err := ledger.Reserve(params); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("retry did not observe funds consumed after refund: %v", err)
	}
	if err := func() error { _, err := Restore(ledger.Snapshot()); return err }(); err != nil {
		t.Fatalf("failed retry corrupted cap or ledger state: %v", err)
	}

	cancelRequest, cancelAttempt := testID(t), testID(t)
	if _, _, err := ledger.Reserve(ReserveParams{RequestID: cancelRequest, AttemptID: cancelAttempt, ModelID: defaultModelID, AccountID: account, APIKeyID: key,
		OwnerID: owner1, InputTokens: 0, MaxOutputTokens: 10, DailySpendCap: capAmount, Price: price, AcceptedAt: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Settle(cancelRequest, cancelAttempt, 3, true, now.Add(2*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
	bal, err := ledger.Balance(account)
	if err != nil || bal.Available.Int64() != 7 || bal.Reserved.Int64() != 0 {
		t.Fatalf("cancel did not release the unused reservation after charging verified prefix: balance=%+v err=%v", bal, err)
	}
	quote, err := CalculateQuote(0, 3, 10, price, true)
	if err != nil || quote.ActualCost.Int64() != 3 {
		t.Fatalf("unexpected verified-prefix quote: %+v err=%v", quote, err)
	}
}

func TestSuccessfulEmptyOutputStillSettlesInputCost(t *testing.T) {
	l := NewLedger()
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	account, key, owner, request, attempt := testID(t), testID(t), testID(t), testID(t), testID(t)
	credit, _ := domain.NewPiconero(20)
	if _, err := l.CreditTest(account, "credit-empty", credit, now); err != nil {
		t.Fatal(err)
	}
	capAmount, _ := domain.NewPiconero(20)
	p := ReserveParams{RequestID: request, AttemptID: attempt, ModelID: defaultModelID, AccountID: account, APIKeyID: key, OwnerID: owner,
		InputTokens: 1, MaxOutputTokens: 10, DailySpendCap: capAmount, Price: testPrice(t, 1_000_000, 1_000_000, 10_000), AcceptedAt: now}
	if _, _, err := l.Reserve(p); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(request, attempt, 0, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	bal, err := l.Balance(account)
	if err != nil || bal.Available.Int64() != 19 || bal.Reserved.Int64() != 0 {
		t.Fatalf("empty successful response balance=%+v err=%v", bal, err)
	}
}

func TestCostArithmeticDoesNotUseFloat(t *testing.T) {
	price := testPrice(t, 1_000, 1_000, 10_000)
	quote, err := CalculateQuote(9_007_199_254_740_993, 1, 1, price, false)
	if err != nil {
		t.Fatal(err)
	}
	expected := new(big.Int).Mul(big.NewInt(9_007_199_254_740_993), big.NewInt(1_000))
	expected.Add(expected, big.NewInt(999_999))
	expected.Quo(expected, big.NewInt(1_000_000))
	if quote.InputCost.String() != expected.String() {
		t.Fatalf("input cost=%s want=%s", quote.InputCost.String(), expected.String())
	}
}

func testPrice(t *testing.T, inputRate, outputRate int64, share uint32) Price {
	t.Helper()
	version, _ := domain.ParseID("00000000-0000-4000-8000-000000000001")
	input, _ := domain.NewPiconero(inputRate)
	output, _ := domain.NewPiconero(outputRate)
	return Price{Version: version, InputRate: input, OutputRate: output, OwnerShareBPS: share}
}

func testID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func assertJournal(t *testing.T, entries []Entry) {
	t.Helper()
	previous := ""
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.PreviousHash != previous {
			t.Fatalf("broken previous hash at %s", entry.ID)
		}
		if seen[entry.OperationID] {
			t.Fatalf("duplicate operation %s", entry.OperationID)
		}
		seen[entry.OperationID] = true
		hash, err := hashEntry(entry)
		if err != nil || hash != entry.EntryHash {
			t.Fatalf("invalid hash for %s: %v", entry.ID, err)
		}
		sum := new(big.Int)
		for _, posting := range entry.Postings {
			delta, err := parseSigned(posting.Delta)
			if err != nil {
				t.Fatal(err)
			}
			sum.Add(sum, big.NewInt(delta))
		}
		if sum.Sign() != 0 {
			t.Fatalf("unbalanced entry %s: %s", entry.ID, sum)
		}
		previous = entry.EntryHash
	}
}
