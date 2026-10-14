//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

const provider = "provider-a"

// newOp monta uma operação do jogador da carteira w; a chave de idempotência deriva do ID externo.
func newOp(t *testing.T, w domain.Wallet, kind, external, amount, reference string) domain.Operation {
	t.Helper()
	op, err := domain.NewOperation(domain.OperationInput{
		Kind: kind, ExternalTransactionID: external, IdempotencyKey: "key-" + external,
		PlayerID: w.PlayerID.String(), WalletID: w.ID.String(),
		RoundID: "round-1", GameID: "game-1", Amount: amount, Currency: "BRL",
		ReferenceExternalTransactionID: reference,
	})
	if err != nil {
		t.Fatalf("NewOperation(%s %s): %v", kind, external, err)
	}
	return op
}

// submit envia a operação e exige resposta sem erro.
func submit(t *testing.T, svc *usecase.Service, op domain.Operation) usecase.Result {
	t.Helper()
	res, err := svc.Process(context.Background(), provider, op)
	if err != nil {
		t.Fatalf("Process(%s %s): %v", op.Kind, op.ExternalTransactionID, err)
	}
	return res
}

func requireResult(t *testing.T, res usecase.Result, status domain.Status, code, balance string) {
	t.Helper()
	got := ""
	if res.Balance != nil {
		got = res.Balance.Amount()
	}
	if res.Status != status || res.FailureCode != code || got != balance {
		t.Fatalf("result = %s %q balance %q, want %s %q balance %q", res.Status, res.FailureCode, got, status, code, balance)
	}
}

func (db *schemaDB) txState(t *testing.T, id uuid.UUID) (status, failure string, attempts int) {
	t.Helper()
	err := db.pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(failure_code, ''), reference_attempts FROM wager_transactions WHERE id=$1`, id).
		Scan(&status, &failure, &attempts)
	if err != nil {
		t.Fatalf("transaction state: %v", err)
	}
	return status, failure, attempts
}

func (db *schemaDB) eventTypes(t *testing.T, txID uuid.UUID) []string {
	t.Helper()
	rows, err := db.pool.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE transaction_id=$1 ORDER BY event_type`, txID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		types = append(types, s)
	}
	sort.Strings(types)
	return types
}

func (db *schemaDB) ledgerCount(t *testing.T, walletID uuid.UUID) int {
	t.Helper()
	return db.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID)
}

func requireEvents(t *testing.T, db *schemaDB, txID uuid.UUID, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := db.eventTypes(t, txID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events of %s = %v, want %v", txID, got, want)
	}
}

func requireWallet(t *testing.T, db *schemaDB, id uuid.UUID, cents, version int64) {
	t.Helper()
	if c, v := db.walletState(t, id); c != cents || v != version {
		t.Fatalf("wallet = %d v%d, want %d v%d", c, v, cents, version)
	}
}

// --- WIN e LOSS -------------------------------------------------------------

func TestWin_WithoutReferenceCreditsAndReplaysHistoricalBalance(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")

	win := submit(t, svc, newOp(t, w, "WIN", "win-1", "40.00", ""))
	requireResult(t, win, domain.StatusProcessed, "", "140.00")
	requireWallet(t, db, w.ID, 14000, 2)
	requireEvents(t, db, win.TransactionID, "WagerTransactionProcessed", "WalletBalanceChanged")

	submit(t, svc, newOp(t, w, "BET", "bet-1", "10.00", "")) // o saldo atual passa a ser 130.00
	replay := submit(t, svc, newOp(t, w, "WIN", "win-1", "40.00", ""))
	requireResult(t, replay, domain.StatusProcessed, "", "140.00")
	if !replay.Replay || replay.TransactionID != win.TransactionID {
		t.Fatalf("replay = %+v, want replay of %s", replay, win.TransactionID)
	}
	requireWallet(t, db, w.ID, 13000, 3)
}

func TestWin_ReferencingABetUsesTheResolvedReference(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	bet := submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))

	win := submit(t, svc, newOp(t, w, "WIN", "win-1", "90.00", "bet-1"))
	requireResult(t, win, domain.StatusProcessed, "", "160.00")

	var ref uuid.UUID
	err := db.pool.QueryRow(context.Background(), `SELECT reference_transaction_id FROM wager_transactions WHERE id=$1`, win.TransactionID).Scan(&ref)
	if err != nil || ref != bet.TransactionID {
		t.Fatalf("resolved reference = %v, %v; want %s", ref, err, bet.TransactionID)
	}

	rejected := submit(t, svc, newOp(t, w, "BET", "bet-bad", "500.00", "")) // INSUFFICIENT_FUNDS
	requireResult(t, rejected, domain.StatusRejected, "INSUFFICIENT_FUNDS", "160.00")
	res := submit(t, svc, newOp(t, w, "WIN", "win-2", "10.00", "bet-bad"))
	requireResult(t, res, domain.StatusRejected, "REFERENCE_NOT_PROCESSED", "160.00")
	requireEvents(t, db, res.TransactionID, "WagerTransactionRejected")
	requireWallet(t, db, w.ID, 16000, 3)
}

func TestLoss_WritesNoLedgerAndKeepsVersion(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	entries := db.ledgerCount(t, w.ID)

	loss := submit(t, svc, newOp(t, w, "LOSS", "loss-1", "0.00", ""))
	requireResult(t, loss, domain.StatusProcessed, "", "100.00")
	requireWallet(t, db, w.ID, 10000, 1)
	if n := db.ledgerCount(t, w.ID); n != entries {
		t.Fatalf("ledger entries = %d, want %d", n, entries)
	}
	requireEvents(t, db, loss.TransactionID, "WagerTransactionProcessed")

	submit(t, svc, newOp(t, w, "BET", "bet-1", "10.00", ""))
	replay := submit(t, svc, newOp(t, w, "LOSS", "loss-1", "0.00", ""))
	requireResult(t, replay, domain.StatusProcessed, "", "100.00")
	if !replay.Replay || replay.TransactionID != loss.TransactionID {
		t.Fatalf("replay = %+v", replay)
	}

	usd, err := domain.NewOperation(domain.OperationInput{
		Kind: "LOSS", ExternalTransactionID: "loss-usd", IdempotencyKey: "key-loss-usd",
		PlayerID: w.PlayerID.String(), WalletID: w.ID.String(), RoundID: "r", GameID: "g", Amount: "0.00", Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	requireResult(t, submit(t, svc, usd), domain.StatusRejected, "CURRENCY_MISMATCH", "")
}

// --- REFUND e ROLLBACK ------------------------------------------------------

func TestRefund_ReturnsTheBetOnceAndAuditsRejections(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	submit(t, svc, newOp(t, w, "WIN", "win-1", "5.00", ""))
	requireWallet(t, db, w.ID, 7500, 3)

	refund := submit(t, svc, newOp(t, w, "REFUND", "refund-1", "30.00", "bet-1"))
	requireResult(t, refund, domain.StatusProcessed, "", "105.00")
	requireWallet(t, db, w.ID, 10500, 4)
	requireEvents(t, db, refund.TransactionID, "WagerTransactionProcessed", "WalletBalanceChanged")

	cases := []struct {
		name string
		op   domain.Operation
		code string
	}{
		{"second refund of the same bet", newOp(t, w, "REFUND", "refund-2", "30.00", "bet-1"), "REFERENCE_ALREADY_COMPENSATED"},
		{"refund of a WIN", newOp(t, w, "REFUND", "refund-3", "5.00", "win-1"), "REFERENCE_KIND_INVALID"},
		{"another amount", newOp(t, w, "REFUND", "refund-4", "29.99", "bet-1"), "AMOUNT_MISMATCH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := submit(t, svc, tc.op)
			requireResult(t, res, domain.StatusRejected, tc.code, "105.00")
			requireEvents(t, db, res.TransactionID, "WagerTransactionRejected")
			again := submit(t, svc, tc.op)
			if !again.Replay || again.TransactionID != res.TransactionID || again.FailureCode != tc.code {
				t.Fatalf("replay = %+v", again)
			}
		})
	}
	requireWallet(t, db, w.ID, 10500, 4)
}

func TestRollback_ReversesBetWinAndRefundWithTheOppositeMovement(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")

	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	requireResult(t, submit(t, svc, newOp(t, w, "ROLLBACK", "rb-bet", "30.00", "bet-1")), domain.StatusProcessed, "", "100.00")

	submit(t, svc, newOp(t, w, "WIN", "win-1", "50.00", ""))
	requireResult(t, submit(t, svc, newOp(t, w, "ROLLBACK", "rb-win", "50.00", "win-1")), domain.StatusProcessed, "", "100.00")

	submit(t, svc, newOp(t, w, "BET", "bet-2", "20.00", ""))
	submit(t, svc, newOp(t, w, "REFUND", "refund-2", "20.00", "bet-2"))
	requireResult(t, submit(t, svc, newOp(t, w, "ROLLBACK", "rb-refund", "20.00", "refund-2")), domain.StatusProcessed, "", "80.00")

	// O saldo armazenado é a soma dos créditos menos a dos débitos do ledger.
	var credits, debits int64
	err := db.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(amount) FILTER (WHERE direction='CREDIT'), 0), COALESCE(SUM(amount) FILTER (WHERE direction='DEBIT'), 0)
		  FROM wallet_ledger_entries WHERE wallet_id=$1`, w.ID).Scan(&credits, &debits)
	if err != nil {
		t.Fatal(err)
	}
	if cents, _ := db.walletState(t, w.ID); cents != credits-debits || cents != 8000 {
		t.Fatalf("balance %d, ledger credits-debits %d, want 8000", cents, credits-debits)
	}
}

func TestRollback_OfWinWithoutFundsIsAuditableAndDistinctFromBet(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	submit(t, svc, newOp(t, w, "WIN", "win-1", "50.00", ""))
	submit(t, svc, newOp(t, w, "BET", "bet-1", "140.00", "")) // saldo 10.00
	entries := db.ledgerCount(t, w.ID)

	rollback := newOp(t, w, "ROLLBACK", "rb-1", "50.00", "win-1")
	res := submit(t, svc, rollback)
	requireResult(t, res, domain.StatusRejected, "REVERSAL_INSUFFICIENT_FUNDS", "10.00")
	requireEvents(t, db, res.TransactionID, "WagerTransactionRejected")
	if n := db.ledgerCount(t, w.ID); n != entries {
		t.Fatalf("ledger entries = %d, want %d", n, entries)
	}
	requireWallet(t, db, w.ID, 1000, 3)

	submit(t, svc, newOp(t, w, "WIN", "win-2", "100.00", "")) // agora haveria saldo
	replay := submit(t, svc, rollback)
	requireResult(t, replay, domain.StatusRejected, "REVERSAL_INSUFFICIENT_FUNDS", "10.00")
	if !replay.Replay || replay.TransactionID != res.TransactionID {
		t.Fatalf("replay = %+v, want the original rejection", replay)
	}
}

func TestDEC001_ExclusivityIsHistorical(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	submit(t, svc, newOp(t, w, "REFUND", "refund-1", "30.00", "bet-1"))
	requireResult(t, submit(t, svc, newOp(t, w, "ROLLBACK", "rb-refund", "30.00", "refund-1")), domain.StatusProcessed, "", "70.00")

	// ROLLBACK(REFUND) não libera a BET para outra compensação direta.
	requireResult(t, submit(t, svc, newOp(t, w, "ROLLBACK", "rb-bet", "30.00", "bet-1")), domain.StatusRejected, "REFERENCE_ALREADY_COMPENSATED", "70.00")
	requireResult(t, submit(t, svc, newOp(t, w, "REFUND", "refund-2", "30.00", "bet-1")), domain.StatusRejected, "REFERENCE_ALREADY_COMPENSATED", "70.00")
	// O REFUND, por sua vez, só admite uma reversão.
	requireResult(t, submit(t, svc, newOp(t, w, "ROLLBACK", "rb-refund-2", "30.00", "refund-1")), domain.StatusRejected, "REFERENCE_ALREADY_COMPENSATED", "70.00")

	requireWallet(t, db, w.ID, 7000, 4)
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK')`, w.ID); n != 2 {
		t.Fatalf("processed compensations = %d, want 2 (REFUND and ROLLBACK(REFUND))", n)
	}
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND status='REJECTED'`, w.ID); n != 3 {
		t.Fatalf("rejected rows = %d, want 3 (all preserved for audit)", n)
	}
}

func TestRefundVersusRollbackOfTheSameBetConcurrently(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)
	for round := 0; round < 10; round++ {
		w := openFunded(t, instances[0], "100.00")
		id := fmt.Sprintf("-%d", round)
		submit(t, instances[0], newOp(t, w, "BET", "bet"+id, "30.00", ""))
		ops := []domain.Operation{
			newOp(t, w, "REFUND", "refund"+id, "30.00", "bet"+id),
			newOp(t, w, "ROLLBACK", "rollback"+id, "30.00", "bet"+id),
		}
		results := make([]usecase.Result, 2)
		errs := make([]error, 2)
		runConcurrently(2, func(i int) {
			results[i], errs[i] = instances[(i+1)%3].Process(context.Background(), provider, ops[i])
		})
		processed, rejected := 0, 0
		for i, res := range results {
			if errs[i] != nil {
				t.Fatalf("round %d: %v", round, errs[i])
			}
			switch {
			case res.Status == domain.StatusProcessed:
				processed++
			case res.Status == domain.StatusRejected && res.FailureCode == "REFERENCE_ALREADY_COMPENSATED":
				rejected++
			}
		}
		if processed != 1 || rejected != 1 {
			t.Fatalf("round %d: processed=%d rejected=%d, want 1 and 1 (%+v)", round, processed, rejected, results)
		}
		requireWallet(t, db, w.ID, 10000, 3)
		if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='CREDIT'`, w.ID); n != 2 {
			t.Fatalf("round %d: credit entries = %d, want opening + one compensation", round, n)
		}
	}
}

func TestOperations_SameKeyWithAnotherKindIsAConflict(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	submit(t, svc, newOp(t, w, "BET", "tx-1", "30.00", ""))

	_, err := svc.Process(context.Background(), provider, newOp(t, w, "WIN", "tx-1", "30.00", ""))
	if !errors.Is(err, usecase.ErrIdempotencyConflict) {
		t.Fatalf("same key and external id with another kind: %v, want ErrIdempotencyConflict", err)
	}
	requireWallet(t, db, w.ID, 7000, 2)
}
