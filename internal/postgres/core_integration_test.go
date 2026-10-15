//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

// "Instâncias" aqui são pools e serviços independentes (conexões próprias) no mesmo processo
// de teste; não substituem o teste com três processos, que fica para o milestone distribuído.

func newTestService(t *testing.T, store usecase.Store, policy usecase.ReferencePolicy) *usecase.Service {
	t.Helper()
	svc, err := usecase.NewService(store, policy)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func (db *schemaDB) instances(t *testing.T, n int) []*usecase.Service {
	t.Helper()
	return db.instancesWith(t, n, usecase.DefaultReferencePolicy())
}

func (db *schemaDB) instancesWith(t *testing.T, n int, policy usecase.ReferencePolicy) []*usecase.Service {
	t.Helper()
	services := make([]*usecase.Service, n)
	for i := range services {
		services[i] = newTestService(t, NewStore(openPool(t, db.url), 5*time.Second), policy)
	}
	return services
}

func money(t *testing.T, amount string) domain.Money {
	t.Helper()
	m, err := domain.NewMoney(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openFunded(t *testing.T, svc *usecase.Service, amount string) domain.Wallet {
	t.Helper()
	res, err := svc.OpenWallet(context.Background(), uuid.New(), money(t, amount))
	if err != nil {
		t.Fatalf("OpenWallet: %v", err)
	}
	return res.Wallet
}

func newBet(t *testing.T, w domain.Wallet, external, key, amount string) domain.Operation {
	t.Helper()
	b, err := domain.NewOperation(domain.OperationInput{
		Kind:                  "BET",
		ExternalTransactionID: external,
		IdempotencyKey:        key,
		PlayerID:              w.PlayerID.String(),
		WalletID:              w.ID.String(),
		RoundID:               "round-1",
		GameID:                "game-1",
		Amount:                amount,
		Currency:              "BRL",
	})
	if err != nil {
		t.Fatalf("NewBet: %v", err)
	}
	return b
}

// walletState lê o saldo e a versão persistidos, direto do banco.
func (db *schemaDB) walletState(t *testing.T, id uuid.UUID) (cents, version int64) {
	t.Helper()
	err := db.pool.QueryRow(context.Background(), `SELECT balance, version FROM wallets WHERE id=$1`, id).Scan(&cents, &version)
	if err != nil {
		t.Fatalf("wallet state: %v", err)
	}
	return cents, version
}

func (db *schemaDB) betCount(t *testing.T, walletID uuid.UUID, status string) int {
	t.Helper()
	return db.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET' AND status=$2`, walletID, status)
}

func (db *schemaDB) debitCount(t *testing.T, walletID uuid.UUID) int {
	t.Helper()
	return db.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`, walletID)
}

// runConcurrently dispara todas as funções ao mesmo tempo, distribuindo-as pelas instâncias.
func runConcurrently(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

// --- abertura ---------------------------------------------------------------

func TestOpenWallet_PositiveInitialBalanceWritesOpeningLedgerAndEvents(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	player := uuid.New()

	res, err := svc.OpenWallet(ctx, player, money(t, "1000.00"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Wallet.Balance.Amount() != "1000.00" || res.Wallet.Version != 1 || res.Wallet.PlayerID != player {
		t.Fatalf("result = %+v", res)
	}

	cents, version := db.walletState(t, res.Wallet.ID)
	if cents != 100000 || version != 1 {
		t.Fatalf("wallet = %d v%d, want 100000 v1", cents, version)
	}
	var kind, status string
	var amount, resultBalance int64
	err = db.pool.QueryRow(ctx, `SELECT kind, status, amount, result_balance FROM wager_transactions WHERE wallet_id=$1`, res.Wallet.ID).
		Scan(&kind, &status, &amount, &resultBalance)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "OPENING" || status != "PROCESSED" || amount != 100000 || resultBalance != 100000 {
		t.Errorf("opening = %s %s %d %d", kind, status, amount, resultBalance)
	}
	var direction string
	var before, after, entryVersion int64
	err = db.pool.QueryRow(ctx, `SELECT direction, balance_before, balance_after, wallet_version FROM wallet_ledger_entries WHERE wallet_id=$1`, res.Wallet.ID).
		Scan(&direction, &before, &after, &entryVersion)
	if err != nil {
		t.Fatal(err)
	}
	if direction != "CREDIT" || before != 0 || after != 100000 || entryVersion != 1 {
		t.Errorf("ledger = %s %d %d v%d", direction, before, after, entryVersion)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE event_type IN ('WagerTransactionProcessed','WalletBalanceChanged')`); n != 2 {
		t.Errorf("outbox events = %d, want 2", n)
	}
}

func TestOpenWallet_ZeroBalanceCreatesOnlyTheWallet(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]

	res, err := svc.OpenWallet(context.Background(), uuid.New(), money(t, "0.00"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.Wallet.Balance.IsZero() || res.Wallet.Version != 1 {
		t.Fatalf("result = %+v", res)
	}
	for _, table := range []string{"wager_transactions", "wallet_ledger_entries", "outbox_events"} {
		if n := db.count(t, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s rows = %d, want 0", table, n)
		}
	}
}

func TestOpenWallet_RepeatedDoesNotGrantTheCreditAgain(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	player := uuid.New()

	first, err := svc.OpenWallet(ctx, player, money(t, "100.00"))
	if err != nil || !first.Created {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := svc.OpenWallet(ctx, player, money(t, "500.00"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Wallet.ID != first.Wallet.ID || again.Wallet.Balance.Amount() != "100.00" {
		t.Fatalf("second = %+v, want existing wallet unchanged", again)
	}
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='OPENING'`); n != 1 {
		t.Errorf("OPENING rows = %d, want 1", n)
	}
	if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries`); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

func TestOpenWallet_ConcurrentCreatesOneWalletAndOneCredit(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)
	player := uuid.New()
	initial := money(t, "1000.00")

	const attempts = 20
	results := make([]usecase.OpenWalletResult, attempts)
	errs := make([]error, attempts)
	runConcurrently(attempts, func(i int) {
		results[i], errs[i] = instances[i%len(instances)].OpenWallet(context.Background(), player, initial)
	})

	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if results[i].Created {
			created++
		}
		if results[i].Wallet.ID != results[0].Wallet.ID {
			t.Fatalf("attempt %d returned another wallet", i)
		}
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	cents, version := db.walletState(t, results[0].Wallet.ID)
	if cents != 100000 || version != 1 {
		t.Errorf("wallet = %d v%d, want 100000 v1", cents, version)
	}
	if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries`); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// --- BET --------------------------------------------------------------------

func TestProcess_ProcessedWritesTransactionWalletLedgerAndOutbox(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "100.00")
	bet := newBet(t, w, "tx-1", "provider-a:tx-1", "25.00")

	res, err := svc.Process(ctx, "provider-a", bet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "PROCESSED" || res.Replay || res.FailureCode != "" || res.Balance == nil || res.Balance.Amount() != "75.00" {
		t.Fatalf("result = %+v", res)
	}

	cents, version := db.walletState(t, w.ID)
	if cents != 7500 || version != 2 {
		t.Fatalf("wallet = %d v%d, want 7500 v2", cents, version)
	}
	var hashLen int
	var resultBalance int64
	var provider string
	err = db.pool.QueryRow(ctx, `SELECT octet_length(payload_hash), result_balance, provider_id FROM wager_transactions WHERE id=$1`, res.TransactionID).
		Scan(&hashLen, &resultBalance, &provider)
	if err != nil || hashLen != 32 || resultBalance != 7500 || provider != "provider-a" {
		t.Fatalf("transaction = %d %d %q, %v", hashLen, resultBalance, provider, err)
	}
	var direction string
	var before, after, entryVersion int64
	err = db.pool.QueryRow(ctx, `SELECT direction, balance_before, balance_after, wallet_version FROM wallet_ledger_entries WHERE transaction_id=$1`, res.TransactionID).
		Scan(&direction, &before, &after, &entryVersion)
	if err != nil || direction != "DEBIT" || before != 10000 || after != 7500 || entryVersion != 2 {
		t.Fatalf("ledger = %s %d %d v%d, %v", direction, before, after, entryVersion, err)
	}

	events := map[string]map[string]any{}
	rows, err := db.pool.Query(ctx, `SELECT event_type, payload FROM outbox_events WHERE transaction_id=$1`, res.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventType string
		var payload []byte
		if err := rows.Scan(&eventType, &payload); err != nil {
			t.Fatal(err)
		}
		var envelope map[string]any
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		events[eventType] = envelope
	}
	if len(events) != 2 {
		t.Fatalf("events = %v, want WagerTransactionProcessed and WalletBalanceChanged", events)
	}
	changed := events["WalletBalanceChanged"]["data"].(map[string]any)
	if changed["direction"] != "DEBIT" || changed["walletVersion"] != float64(2) ||
		changed["balanceBefore"].(map[string]any)["amount"] != "100.00" ||
		changed["balanceAfter"].(map[string]any)["amount"] != "75.00" {
		t.Errorf("WalletBalanceChanged data = %v", changed)
	}
	processed := events["WagerTransactionProcessed"]["data"].(map[string]any)
	if processed["status"] != "PROCESSED" || processed["providerId"] != "provider-a" || processed["kind"] != "BET" {
		t.Errorf("WagerTransactionProcessed data = %v", processed)
	}
}

func TestProcess_TwoBetsOf80OnBalance100(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)

	for round := 0; round < 10; round++ {
		w := openFunded(t, instances[0], "100.00")
		bets := []domain.Operation{
			newBet(t, w, fmt.Sprintf("tx-a-%d", round), fmt.Sprintf("provider-a:tx-a-%d", round), "80.00"),
			newBet(t, w, fmt.Sprintf("tx-b-%d", round), fmt.Sprintf("provider-a:tx-b-%d", round), "80.00"),
		}
		results := make([]usecase.Result, 2)
		errs := make([]error, 2)
		runConcurrently(2, func(i int) {
			results[i], errs[i] = instances[(i+1)%3].Process(context.Background(), "provider-a", bets[i])
		})

		processed, rejected := 0, 0
		for i, res := range results {
			if errs[i] != nil {
				t.Fatalf("round %d bet %d: %v", round, i, errs[i])
			}
			switch {
			case res.Status == "PROCESSED":
				processed++
			case res.Status == "REJECTED" && res.FailureCode == "INSUFFICIENT_FUNDS":
				rejected++
			}
		}
		if processed != 1 || rejected != 1 {
			t.Fatalf("round %d: processed=%d rejected=%d, want 1 and 1", round, processed, rejected)
		}
		if cents, version := db.walletState(t, w.ID); cents != 2000 || version != 2 {
			t.Fatalf("round %d: wallet = %d v%d, want 2000 v2", round, cents, version)
		}
		if n := db.debitCount(t, w.ID); n != 1 {
			t.Fatalf("round %d: debit entries = %d, want 1", round, n)
		}
		if n := db.betCount(t, w.ID, "REJECTED"); n != 1 {
			t.Fatalf("round %d: rejected rows = %d, want 1", round, n)
		}
	}
}

func TestProcess_FiftyIdenticalRequestsDebitOnce(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)
	w := openFunded(t, instances[0], "100.00")
	bet := newBet(t, w, "tx-1", "provider-a:tx-1", "25.00")

	const requests = 50
	results := make([]usecase.Result, requests)
	errs := make([]error, requests)
	runConcurrently(requests, func(i int) {
		results[i], errs[i] = instances[i%len(instances)].Process(context.Background(), "provider-a", bet)
	})

	originals := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if !res.Replay {
			originals++
		}
		if res.Status != "PROCESSED" || res.TransactionID != results[0].TransactionID ||
			res.Balance == nil || res.Balance.Amount() != "75.00" {
			t.Fatalf("request %d result = %+v, want the original result", i, res)
		}
	}
	if originals != 1 {
		t.Errorf("non-replay responses = %d, want 1", originals)
	}
	if cents, version := db.walletState(t, w.ID); cents != 7500 || version != 2 {
		t.Errorf("wallet = %d v%d, want 7500 v2", cents, version)
	}
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 1 {
		t.Errorf("BET transactions = %d, want 1", n)
	}
	if n := db.debitCount(t, w.ID); n != 1 {
		t.Errorf("debit entries = %d, want 1", n)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE transaction_id=$1`, results[0].TransactionID); n != 2 {
		t.Errorf("outbox events for the bet = %d, want 2 (processed + balance changed)", n)
	}
}

func TestProcess_IdempotencyConflictsAndReplays(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "100.00")
	first := newBet(t, w, "tx-1", "provider-a:tx-1", "25.00")
	original, err := svc.Process(ctx, "provider-a", first)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		if cents, version := db.walletState(t, w.ID); cents != 7500 || version != 2 {
			t.Fatalf("wallet = %d v%d, want 7500 v2", cents, version)
		}
		if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 1 {
			t.Fatalf("BET rows = %d, want 1", n)
		}
	}

	t.Run("same key and payload is a replay", func(t *testing.T) {
		res, err := svc.Process(ctx, "provider-a", first)
		if err != nil || !res.Replay || res.TransactionID != original.TransactionID || res.Status != "PROCESSED" {
			t.Fatalf("replay = %+v, %v", res, err)
		}
		unchanged(t)
	})
	t.Run("same key with another payload conflicts", func(t *testing.T) {
		_, err := svc.Process(ctx, "provider-a", newBet(t, w, "tx-1", "provider-a:tx-1", "26.00"))
		if !errors.Is(err, usecase.ErrIdempotencyConflict) {
			t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
		}
		unchanged(t)
	})
	t.Run("same external id with another key conflicts", func(t *testing.T) {
		_, err := svc.Process(ctx, "provider-a", newBet(t, w, "tx-1", "another-key", "25.00"))
		if !errors.Is(err, usecase.ErrIdempotencyConflict) {
			t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
		}
		unchanged(t)
	})
	t.Run("same key with another external id conflicts", func(t *testing.T) {
		_, err := svc.Process(ctx, "provider-a", newBet(t, w, "tx-other", "provider-a:tx-1", "25.00"))
		if !errors.Is(err, usecase.ErrIdempotencyConflict) {
			t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
		}
		unchanged(t)
	})
	t.Run("replay returns the historical balance after other movements", func(t *testing.T) {
		if res, err := svc.Process(ctx, "provider-a", newBet(t, w, "tx-2", "provider-a:tx-2", "50.00")); err != nil || res.Status != "PROCESSED" {
			t.Fatalf("second bet = %+v, %v", res, err)
		}
		res, err := svc.Process(ctx, "provider-a", first)
		if err != nil || !res.Replay || res.Balance == nil || res.Balance.Amount() != "75.00" {
			t.Fatalf("replay = %+v, %v; want historical balance 75.00", res, err)
		}
		if cents, _ := db.walletState(t, w.ID); cents != 2500 {
			t.Fatalf("wallet = %d, want 2500", cents)
		}
	})
	t.Run("same identifiers under another provider are independent", func(t *testing.T) {
		before, _ := db.walletState(t, w.ID)
		res, err := svc.Process(ctx, "provider-b", first)
		if err != nil || res.Replay || res.Status != "PROCESSED" || res.TransactionID == original.TransactionID {
			t.Fatalf("provider-b result = %+v, %v", res, err)
		}
		after, _ := db.walletState(t, w.ID)
		if before-after != 2500 {
			t.Fatalf("provider-b debit = %d, want 2500", before-after)
		}
	})
}

func TestProcess_RejectionIsPersistedAndReplayedAfterNewCredit(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "10.00")
	bet := newBet(t, w, "tx-1", "provider-a:tx-1", "50.00")

	rejected, err := svc.Process(ctx, "provider-a", bet)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != "REJECTED" || rejected.FailureCode != "INSUFFICIENT_FUNDS" || rejected.Balance == nil || rejected.Balance.Amount() != "10.00" {
		t.Fatalf("rejection = %+v", rejected)
	}
	if cents, version := db.walletState(t, w.ID); cents != 1000 || version != 1 {
		t.Fatalf("wallet = %d v%d, want 1000 v1", cents, version)
	}
	if n := db.debitCount(t, w.ID); n != 0 {
		t.Fatalf("debit entries = %d, want 0", n)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE transaction_id=$1 AND event_type='WagerTransactionRejected'`, rejected.TransactionID); n != 1 {
		t.Fatalf("rejection events = %d, want 1", n)
	}

	// Crédito legítimo posterior: 100.00 via WIN (mesmo mecanismo auditável do schema).
	credit := newExternalTx(wallet{id: w.ID.String(), playerID: w.PlayerID.String(), currency: "BRL", balance: 1000, version: 1}, "WIN", 10000)
	db.move(t, wallet{id: w.ID.String(), playerID: w.PlayerID.String(), currency: "BRL", balance: 1000, version: 1}, credit, "CREDIT")

	replay, err := svc.Process(ctx, "provider-a", bet)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.Status != "REJECTED" || replay.FailureCode != "INSUFFICIENT_FUNDS" ||
		replay.TransactionID != rejected.TransactionID || replay.Balance == nil || replay.Balance.Amount() != "10.00" {
		t.Fatalf("replay = %+v, want the original rejection", replay)
	}
	if cents, _ := db.walletState(t, w.ID); cents != 11000 {
		t.Fatalf("wallet = %d, want 11000 (no debit by replay)", cents)
	}
	if n := db.betCount(t, w.ID, "PROCESSED"); n != 0 {
		t.Fatalf("processed BETs = %d, want 0", n)
	}
}

func TestProcess_WalletProblemsArePersistedRejections(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "100.00")

	tests := []struct {
		name     string
		mutate   func(*domain.OperationInput)
		wantCode string
	}{
		{"unknown wallet", func(in *domain.OperationInput) { in.WalletID = uuid.NewString() }, "WALLET_NOT_FOUND"},
		{"wallet of another player", func(in *domain.OperationInput) { in.PlayerID = uuid.NewString() }, "WALLET_NOT_FOUND"},
		{"currency differs from the wallet", func(in *domain.OperationInput) { in.Currency = "USD" }, "CURRENCY_MISMATCH"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := domain.OperationInput{
				Kind:                  "BET",
				ExternalTransactionID: "tx-" + tc.wantCode + string(rune('a'+i)),
				IdempotencyKey:        "key-" + tc.wantCode + string(rune('a'+i)),
				PlayerID:              w.PlayerID.String(), WalletID: w.ID.String(),
				RoundID: "r", GameID: "g", Amount: "10.00", Currency: "BRL",
			}
			tc.mutate(&in)
			bet, err := domain.NewOperation(in)
			if err != nil {
				t.Fatal(err)
			}
			res, err := svc.Process(ctx, "provider-a", bet)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != "REJECTED" || res.FailureCode != tc.wantCode || res.Balance != nil {
				t.Fatalf("result = %+v, want REJECTED %s", res, tc.wantCode)
			}
			again, err := svc.Process(ctx, "provider-a", bet)
			if err != nil || !again.Replay || again.TransactionID != res.TransactionID || again.FailureCode != tc.wantCode {
				t.Fatalf("replay = %+v, %v", again, err)
			}
		})
	}
	if cents, version := db.walletState(t, w.ID); cents != 10000 || version != 1 {
		t.Fatalf("wallet = %d v%d, want untouched", cents, version)
	}
	if n := db.debitCount(t, w.ID); n != 0 {
		t.Fatalf("debit entries = %d, want 0", n)
	}
}

func TestProcess_SameKeyOnDifferentWalletsDebitsOnlyOnce(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)
	walletA := openFunded(t, instances[0], "150.00")
	walletB := openFunded(t, instances[0], "150.00")
	bets := []domain.Operation{
		newBet(t, walletA, "tx-1", "provider-a:tx-1", "25.00"),
		newBet(t, walletB, "tx-1", "provider-a:tx-1", "25.00"),
	}

	for round := 0; round < 5; round++ {
		bets[0].ExternalTransactionID, bets[1].ExternalTransactionID = "tx-"+string(rune('1'+round)), "tx-"+string(rune('1'+round))
		bets[0].IdempotencyKey, bets[1].IdempotencyKey = "k"+string(rune('1'+round)), "k"+string(rune('1'+round))
		results := make([]usecase.Result, 2)
		errs := make([]error, 2)
		runConcurrently(2, func(i int) {
			results[i], errs[i] = instances[i+1].Process(context.Background(), "provider-a", bets[i])
		})
		ok, conflicts := 0, 0
		for i := range results {
			switch {
			case errs[i] == nil && results[i].Status == "PROCESSED":
				ok++
			case errors.Is(errs[i], usecase.ErrIdempotencyConflict):
				conflicts++
			default:
				t.Fatalf("round %d bet %d: %+v, %v", round, i, results[i], errs[i])
			}
		}
		if ok != 1 || conflicts != 1 {
			t.Fatalf("round %d: processed=%d conflicts=%d, want 1 and 1", round, ok, conflicts)
		}
	}
	a, _ := db.walletState(t, walletA.ID)
	b, _ := db.walletState(t, walletB.ID)
	if (15000-a)+(15000-b) != 5*2500 {
		t.Fatalf("total debited = %d, want %d (one debit per round)",
			(15000-a)+(15000-b), 5*2500)
	}
}

func TestProcess_FailureLeavesNoPartialState(t *testing.T) {
	// Falhas controladas do banco (triggers criados só no banco de teste), nunca no código de produção:
	// uma imediata, no INSERT do evento, e uma diferida, que só dispara no COMMIT.
	scenarios := []struct {
		name string
		ddl  string
	}{
		{"immediate failure on outbox insert", `
			CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;
			CREATE TRIGGER test_fail BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION test_fail();`},
		{"deferred failure at commit", `
			CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;
			CREATE CONSTRAINT TRIGGER test_fail AFTER INSERT ON outbox_events DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION test_fail();`},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			db := newSchemaDB(t)
			svc := db.instances(t, 1)[0]
			ctx := context.Background()
			w := openFunded(t, svc, "100.00")
			bet := newBet(t, w, "tx-1", "provider-a:tx-1", "25.00")
			outboxBefore := db.count(t, `SELECT count(*) FROM outbox_events`)

			db.mustExec(t, sc.ddl)
			_, err := svc.Process(ctx, "provider-a", bet)
			if err == nil {
				t.Fatal("Process succeeded despite the forced failure")
			}
			if errors.Is(err, usecase.ErrCommitUnknown) {
				t.Fatalf("a server-reported failure must not be classified as unknown: %v", err)
			}

			if cents, version := db.walletState(t, w.ID); cents != 10000 || version != 1 {
				t.Errorf("wallet = %d v%d, want untouched", cents, version)
			}
			if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 0 {
				t.Errorf("BET rows = %d, want 0", n)
			}
			if n := db.debitCount(t, w.ID); n != 0 {
				t.Errorf("debit entries = %d, want 0", n)
			}
			if n := db.count(t, `SELECT count(*) FROM outbox_events`); n != outboxBefore {
				t.Errorf("outbox rows = %d, want %d", n, outboxBefore)
			}

			// A mesma requisição, depois que a falha some, é processada normalmente, uma única vez.
			db.mustExec(t, `DROP TRIGGER test_fail ON outbox_events; DROP FUNCTION test_fail()`)
			res, err := svc.Process(ctx, "provider-a", bet)
			if err != nil || res.Status != "PROCESSED" || res.Replay {
				t.Fatalf("retry = %+v, %v", res, err)
			}
			if cents, _ := db.walletState(t, w.ID); cents != 7500 {
				t.Errorf("wallet after retry = %d, want 7500", cents)
			}
		})
	}
}

func TestProcess_DifferentWalletsDoNotBlockEachOtherButSameWalletSerializes(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	locked := openFunded(t, svc, "100.00")
	free := openFunded(t, svc, "100.00")

	// Outra conexão segura o lock da carteira "locked", como uma BET em andamento faria.
	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id=$1 FOR UPDATE`, locked.ID); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		_, err := svc.Process(ctx, "provider-a", newBet(t, locked, "tx-l", "key-l", "10.00"))
		blocked <- err
	}()

	// A outra carteira não espera pelo lock.
	freeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if res, err := svc.Process(freeCtx, "provider-a", newBet(t, free, "tx-f", "key-f", "10.00")); err != nil || res.Status != "PROCESSED" {
		t.Fatalf("independent wallet = %+v, %v", res, err)
	}

	select {
	case err := <-blocked:
		t.Fatalf("bet on the locked wallet finished while the lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("bet after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bet on the locked wallet never finished")
	}
	if cents, _ := db.walletState(t, locked.ID); cents != 9000 {
		t.Errorf("locked wallet = %d, want 9000", cents)
	}
}

func TestProcess_LockTimeoutAndCancellationPersistNothing(t *testing.T) {
	db := newSchemaDB(t)
	setup := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, setup, "100.00")

	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id=$1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}

	t.Run("lock timeout", func(t *testing.T) {
		svc := newTestService(t, NewStore(openPool(t, db.url), 200*time.Millisecond), usecase.DefaultReferencePolicy())
		_, err := svc.Process(ctx, "provider-a", newBet(t, w, "tx-1", "key-1", "10.00"))
		if !errors.Is(err, usecase.ErrLockTimeout) {
			t.Fatalf("error = %v, want ErrLockTimeout", err)
		}
	})
	t.Run("context cancelled while waiting", func(t *testing.T) {
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, err := setup.Process(cctx, "provider-a", newBet(t, w, "tx-2", "key-2", "10.00"))
		if err == nil {
			t.Fatal("Process succeeded with an expired context")
		}
	})

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 0 {
		t.Errorf("BET rows = %d, want 0", n)
	}
	if cents, version := db.walletState(t, w.ID); cents != 10000 || version != 1 {
		t.Errorf("wallet = %d v%d, want untouched", cents, version)
	}
}

func TestProcess_RejectsMissingProvider(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	_, err := svc.Process(context.Background(), "", newBet(t, w, "tx-1", "key-1", "10.00"))
	if !errors.Is(err, usecase.ErrInvalidProvider) {
		t.Fatalf("error = %v, want ErrInvalidProvider", err)
	}
}

// --- outbox -----------------------------------------------------------------

func TestOutbox_MigrationUpDownUpKeepsFinancialSchema(t *testing.T) {
	dbURL := createDatabase(t)
	m := newMigrator(t, dbURL)
	if err := m.Migrate(1); err != nil {
		t.Fatalf("migrate to 1: %v", err)
	}
	db := &schemaDB{pool: openPool(t, dbURL), url: dbURL}
	snapshot := func() []string { return db.objectSnapshot(t) }
	baseline := snapshot()

	if err := m.Migrate(2); err != nil {
		t.Fatalf("up: %v", err)
	}
	withOutbox := snapshot()
	if len(withOutbox) <= len(baseline) {
		t.Fatalf("migration 2 added no objects: %d vs %d", len(withOutbox), len(baseline))
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down one step: %v", err)
	}
	after := snapshot()
	if len(after) != len(baseline) {
		t.Fatalf("objects after down = %v, want baseline %v", after, baseline)
	}
	for i := range baseline {
		if after[i] != baseline[i] {
			t.Fatalf("object %d after down = %q, want %q", i, after[i], baseline[i])
		}
	}
	if err := m.Migrate(2); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := snapshot(); len(got) != len(withOutbox) {
		t.Fatalf("objects after second up = %d, want %d", len(got), len(withOutbox))
	}
}

func TestOutbox_PayloadIsImmutableAndEventsAreUniquePerTransaction(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "100.00")
	res, err := svc.Process(ctx, "provider-a", newBet(t, w, "tx-1", "key-1", "10.00"))
	if err != nil {
		t.Fatal(err)
	}

	var eventID uuid.UUID
	err = db.pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE transaction_id=$1 AND event_type='WalletBalanceChanged'`, res.TransactionID).Scan(&eventID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.pool.Exec(ctx, `UPDATE outbox_events SET payload = '{}'::jsonb WHERE event_id=$1`, eventID)
	requirePgError(t, err, sqlstateRestrictViolation, "")
	_, err = db.pool.Exec(ctx, `UPDATE outbox_events SET event_type='WagerTransactionRejected' WHERE event_id=$1`, eventID)
	requirePgError(t, err, sqlstateRestrictViolation, "")
	if _, err := db.pool.Exec(ctx, `UPDATE outbox_events SET attempts = attempts + 1, next_attempt_at = now(), published_at = now() WHERE event_id=$1`, eventID); err != nil {
		t.Fatalf("publication state must stay updatable: %v", err)
	}
	_, err = db.pool.Exec(ctx, `
		INSERT INTO outbox_events (event_id, event_type, schema_version, aggregate_id, message_group_id, transaction_id, payload, occurred_at, next_attempt_at, created_at)
		VALUES (gen_random_uuid(), 'WalletBalanceChanged', 1, $1::uuid, $1::uuid::text, $2, '{}'::jsonb, now(), now(), now())`, w.ID, res.TransactionID)
	requirePgError(t, err, sqlstateUniqueViolation, "outbox_events_transaction_type_key")
}
