//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

// immediate agenda as tentativas sem espera, para que cada passo do worker ache a pendência vencida.
func immediate(maxAttempts int) usecase.ReferencePolicy {
	return usecase.ReferencePolicy{MaxAttempts: maxAttempts}
}

func step(t *testing.T, svc *usecase.Service) usecase.PendingStep {
	t.Helper()
	s, err := svc.ResolveNextPendingReference(context.Background())
	if err != nil {
		t.Fatalf("ResolveNextPendingReference: %v", err)
	}
	return s
}

func requireStep(t *testing.T, s usecase.PendingStep, id uuid.UUID, status domain.Status, code string) {
	t.Helper()
	if !s.Found || s.TransactionID != id || s.Status != status || s.FailureCode != code {
		t.Fatalf("step = %+v, want %s %s %q", s, id, status, code)
	}
}

func (db *schemaDB) pendingCount(t *testing.T) int {
	t.Helper()
	return db.count(t, `SELECT count(*) FROM wager_transactions WHERE status='PENDING_REFERENCE'`)
}

// drain executa passos até não restar pendência, com limite para não mascarar um laço infinito.
func (db *schemaDB) drain(t *testing.T, svc *usecase.Service) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if db.pendingCount(t) == 0 {
			return
		}
		if !step(t, svc).Found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatalf("pending references left after 200 steps: %d", db.pendingCount(t))
}

func TestPending_ArrivesBeforeTheReferenceAndResolvesAfterIt(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(5))[0]
	w := openFunded(t, svc, "100.00")
	rollback := newOp(t, w, "ROLLBACK", "rb-1", "30.00", "bet-1")

	pending := submit(t, svc, rollback)
	requireResult(t, pending, domain.StatusPendingReference, "", "")
	requireWallet(t, db, w.ID, 10000, 1)
	if n := db.ledgerCount(t, w.ID); n != 1 {
		t.Fatalf("ledger entries = %d, want only the opening", n)
	}
	requireEvents(t, db, pending.TransactionID, "WagerTransactionPendingReference")

	replay := submit(t, svc, rollback)
	requireResult(t, replay, domain.StatusPendingReference, "", "")
	if !replay.Replay || replay.TransactionID != pending.TransactionID {
		t.Fatalf("pending replay = %+v", replay)
	}
	requireEvents(t, db, pending.TransactionID, "WagerTransactionPendingReference")

	// Sem a referência a tentativa só é registrada e reagendada.
	s := step(t, svc)
	requireStep(t, s, pending.TransactionID, domain.StatusPendingReference, "")
	if _, _, attempts := db.txState(t, pending.TransactionID); attempts != 1 || s.Attempts != 1 {
		t.Fatalf("attempts = %d (step %d), want 1", attempts, s.Attempts)
	}
	requireWallet(t, db, w.ID, 10000, 1)

	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	requireStep(t, step(t, svc), pending.TransactionID, domain.StatusProcessed, "")
	requireWallet(t, db, w.ID, 10000, 3)
	requireEvents(t, db, pending.TransactionID, "WagerTransactionPendingReference", "WagerTransactionProcessed", "WalletBalanceChanged")

	submit(t, svc, newOp(t, w, "BET", "bet-2", "10.00", "")) // o saldo atual passa a ser 90.00
	again := submit(t, svc, rollback)
	requireResult(t, again, domain.StatusProcessed, "", "100.00")
	if !again.Replay || again.TransactionID != pending.TransactionID {
		t.Fatalf("replay after resolution = %+v", again)
	}
	if step(t, svc).Found {
		t.Fatal("a resolved pending was selected again")
	}
}

func TestPending_ExpiresAfterTheLastAttemptAndStaysRejected(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(3))[0]
	w := openFunded(t, svc, "100.00")
	rollback := newOp(t, w, "ROLLBACK", "rb-1", "30.00", "bet-1")
	pending := submit(t, svc, rollback)

	requireStep(t, step(t, svc), pending.TransactionID, domain.StatusPendingReference, "")
	requireStep(t, step(t, svc), pending.TransactionID, domain.StatusPendingReference, "")
	requireStep(t, step(t, svc), pending.TransactionID, domain.StatusRejected, "REFERENCE_NOT_FOUND")
	requireEvents(t, db, pending.TransactionID, "WagerTransactionPendingReference", "WagerTransactionRejected")

	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", "")) // a referência chega tarde demais
	late := submit(t, svc, rollback)
	requireResult(t, late, domain.StatusRejected, "REFERENCE_NOT_FOUND", "100.00")
	if !late.Replay || late.TransactionID != pending.TransactionID {
		t.Fatalf("replay = %+v", late)
	}
	requireWallet(t, db, w.ID, 7000, 2)
	if step(t, svc).Found {
		t.Fatal("an expired pending was selected again")
	}
}

func TestPending_ReferenceArrivingBeforeTheLastAttemptResolvesInsteadOfExpiring(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(2))[0]
	w := openFunded(t, svc, "100.00")
	pending := submit(t, svc, newOp(t, w, "REFUND", "refund-1", "30.00", "bet-1"))

	requireStep(t, step(t, svc), pending.TransactionID, domain.StatusPendingReference, "")
	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	// Esta é a última tentativa: a referência é consultada antes de qualquer expiração.
	requireStep(t, step(t, svc), pending.TransactionID, domain.StatusProcessed, "")
	requireWallet(t, db, w.ID, 10000, 3)
}

func TestPending_BackoffDoublesUpToTheCapAndHoldsUntilDue(t *testing.T) {
	db := newSchemaDB(t)
	policy := usecase.ReferencePolicy{InitialBackoff: 100 * time.Second, MaxBackoff: 300 * time.Second, MaxAttempts: 10}
	svc := db.instancesWith(t, 1, policy)[0]
	w := openFunded(t, svc, "100.00")
	pending := submit(t, svc, newOp(t, w, "ROLLBACK", "rb-1", "30.00", "bet-1"))

	delay := func() float64 {
		t.Helper()
		var seconds float64
		err := db.pool.QueryRow(context.Background(),
			`SELECT EXTRACT(EPOCH FROM (next_attempt_at - now())) FROM wager_transactions WHERE id=$1`, pending.TransactionID).Scan(&seconds)
		if err != nil {
			t.Fatal(err)
		}
		return seconds
	}
	makeDue := func() {
		t.Helper()
		db.mustExec(t, fmt.Sprintf(`UPDATE wager_transactions SET next_attempt_at = now() WHERE id='%s'`, pending.TransactionID))
	}

	if d := delay(); d < 90 || d > 100 {
		t.Fatalf("first delay = %.1fs, want about 100s", d)
	}
	if step(t, svc).Found {
		t.Fatal("a pending that is not due was selected")
	}
	for i, want := range []float64{200, 300, 300} {
		makeDue()
		requireStep(t, step(t, svc), pending.TransactionID, domain.StatusPendingReference, "")
		if d := delay(); d < want-10 || d > want {
			t.Fatalf("delay after attempt %d = %.1fs, want about %.0fs", i+1, d, want)
		}
	}
}

func TestPending_PendingReferenceNeverExpiresWhileItsReferenceIsStillPending(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(2))[0]
	w := openFunded(t, svc, "100.00")
	win := submit(t, svc, newOp(t, w, "WIN", "win-1", "20.00", "bet-1"))          // BET ausente
	rollback := submit(t, svc, newOp(t, w, "ROLLBACK", "rb-1", "20.00", "win-1")) // referencia a WIN pendente
	requireResult(t, rollback, domain.StatusPendingReference, "", "")
	db.mustExec(t, fmt.Sprintf(`UPDATE wager_transactions SET next_attempt_at = now() + interval '1 hour' WHERE id='%s'`, win.TransactionID))

	for i := 1; i <= 4; i++ { // mais tentativas que MaxAttempts: a referência existe, só está pendente
		s := step(t, svc)
		requireStep(t, s, rollback.TransactionID, domain.StatusPendingReference, "")
		if s.Attempts != i {
			t.Fatalf("attempts = %d, want %d", s.Attempts, i)
		}
	}

	// Quando a WIN expira, a reversão fica rejeitada porque a referência não teve sucesso.
	db.mustExec(t, fmt.Sprintf(`UPDATE wager_transactions SET next_attempt_at = now() WHERE id='%s'`, win.TransactionID))
	db.drain(t, svc)
	if status, code, _ := db.txState(t, win.TransactionID); status != "REJECTED" || code != "REFERENCE_NOT_FOUND" {
		t.Fatalf("WIN = %s %s, want REJECTED REFERENCE_NOT_FOUND", status, code)
	}
	if status, code, _ := db.txState(t, rollback.TransactionID); status != "REJECTED" || code != "REFERENCE_NOT_PROCESSED" {
		t.Fatalf("ROLLBACK = %s %s, want REJECTED REFERENCE_NOT_PROCESSED", status, code)
	}
	requireWallet(t, db, w.ID, 10000, 1)
}

func TestPending_ChainResolvesWhenTheBetArrives(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(10))[0]
	w := openFunded(t, svc, "100.00")
	win := submit(t, svc, newOp(t, w, "WIN", "win-1", "20.00", "bet-1"))
	rollback := submit(t, svc, newOp(t, w, "ROLLBACK", "rb-1", "20.00", "win-1"))

	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	db.drain(t, svc)

	for _, id := range []uuid.UUID{win.TransactionID, rollback.TransactionID} {
		if status, code, _ := db.txState(t, id); status != "PROCESSED" || code != "" {
			t.Fatalf("transaction %s = %s %s, want PROCESSED", id, status, code)
		}
	}
	requireWallet(t, db, w.ID, 7000, 4) // 100 - 30 + 20 - 20
	requireEvents(t, db, rollback.TransactionID, "WagerTransactionPendingReference", "WagerTransactionProcessed", "WalletBalanceChanged")
}

func TestPending_ConcurrentWorkersResolveOnePendingOnce(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instancesWith(t, 3, immediate(5))
	w := openFunded(t, instances[0], "100.00")
	pending := submit(t, instances[0], newOp(t, w, "ROLLBACK", "rb-1", "30.00", "bet-1"))
	submit(t, instances[0], newOp(t, w, "BET", "bet-1", "30.00", ""))

	const workers = 8
	steps := make([]usecase.PendingStep, workers)
	errs := make([]error, workers)
	runConcurrently(workers, func(i int) {
		steps[i], errs[i] = instances[i%len(instances)].ResolveNextPendingReference(context.Background())
	})
	resolved := 0
	for i, s := range steps {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if s.Found {
			resolved++
			requireStep(t, s, pending.TransactionID, domain.StatusProcessed, "")
		}
	}
	if resolved != 1 {
		t.Fatalf("workers that resolved the pending = %d, want 1", resolved)
	}
	requireWallet(t, db, w.ID, 10000, 3)
	requireEvents(t, db, pending.TransactionID, "WagerTransactionPendingReference", "WagerTransactionProcessed", "WalletBalanceChanged")
}

func TestPending_ManyWalletsDrainedByIndependentWorkersEachResolvedOnce(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instancesWith(t, 3, immediate(5))
	const wallets = 12
	var ws []domain.Wallet
	for i := 0; i < wallets; i++ {
		w := openFunded(t, instances[0], "100.00")
		ws = append(ws, w)
		submit(t, instances[0], newOp(t, w, "REFUND", fmt.Sprintf("refund-%d", i), "30.00", fmt.Sprintf("bet-%d", i)))
		submit(t, instances[0], newOp(t, w, "BET", fmt.Sprintf("bet-%d", i), "30.00", ""))
	}

	runConcurrently(len(instances), func(i int) {
		for n := 0; n < 200 && db.count(t, `SELECT count(*) FROM wager_transactions WHERE status='PENDING_REFERENCE'`) > 0; n++ {
			if _, err := instances[i].ResolveNextPendingReference(context.Background()); err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
		}
	})
	db.drain(t, instances[0]) // eventual passo que um worker perdeu por SKIP LOCKED

	for _, w := range ws {
		requireWallet(t, db, w.ID, 10000, 3)
	}
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='REFUND' AND status='PROCESSED'`); n != wallets {
		t.Fatalf("processed refunds = %d, want %d", n, wallets)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE event_type='WagerTransactionProcessed' AND payload->'data'->>'kind'='REFUND'`); n != wallets {
		t.Fatalf("REFUND processed events = %d, want %d", n, wallets)
	}
}

func TestPending_WorkerVersusSynchronousCompensationOfTheSameBet(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instancesWith(t, 3, immediate(5))
	for round := 0; round < 10; round++ {
		id := fmt.Sprintf("-%d", round)
		w := openFunded(t, instances[0], "100.00")
		submit(t, instances[0], newOp(t, w, "ROLLBACK", "rb"+id, "30.00", "bet"+id))
		submit(t, instances[0], newOp(t, w, "BET", "bet"+id, "30.00", ""))

		var refund usecase.Result
		var refundErr, stepErr error
		runConcurrently(2, func(i int) {
			if i == 0 {
				refund, refundErr = instances[1].Process(context.Background(), provider, newOp(t, w, "REFUND", "refund"+id, "30.00", "bet"+id))
				return
			}
			_, stepErr = instances[2].ResolveNextPendingReference(context.Background())
		})
		if refundErr != nil || stepErr != nil {
			t.Fatalf("round %d: refund %v, step %v", round, refundErr, stepErr)
		}
		db.drain(t, instances[2]) // se o worker pulou a carteira ocupada, resolve agora

		processed := db.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK')`, w.ID)
		rejected := db.count(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND status='REJECTED' AND failure_code='REFERENCE_ALREADY_COMPENSATED'`, w.ID)
		if processed != 1 || rejected != 1 {
			t.Fatalf("round %d: processed=%d rejected=%d, want 1 and 1 (refund %+v)", round, processed, rejected, refund)
		}
		requireWallet(t, db, w.ID, 10000, 3)
	}
}

func TestPending_WorkerDoesNotWaitForABusyWallet(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(5))[0]
	busy := openFunded(t, svc, "100.00")
	free := openFunded(t, svc, "100.00")
	first := submit(t, svc, newOp(t, busy, "ROLLBACK", "rb-busy", "30.00", "bet-busy"))
	second := submit(t, svc, newOp(t, free, "ROLLBACK", "rb-free", "30.00", "bet-free"))

	ctx := context.Background()
	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id=$1 FOR UPDATE`, busy.ID); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	s := step(t, svc)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("step waited %v for a busy wallet", elapsed)
	}
	requireStep(t, s, second.TransactionID, domain.StatusPendingReference, "")

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := step(t, svc); !got.Found || got.TransactionID != first.TransactionID {
		t.Fatalf("after release: %+v, want the pending of the freed wallet", got)
	}
}

func TestPending_SurvivesTheInstanceThatCreatedItAndFailuresDoNotBecomeFailed(t *testing.T) {
	scenarios := []struct {
		name string
		ddl  string
	}{
		{"immediate failure", `
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
			// A instância A cria a pendência e é encerrada; só o banco guarda o estado.
			poolA := openPool(t, db.url)
			a := newTestService(t, NewStore(poolA, 5*time.Second), immediate(5))
			w := openFunded(t, a, "100.00")
			pending := submit(t, a, newOp(t, w, "ROLLBACK", "rb-1", "30.00", "bet-1"))
			poolA.Close()

			b := db.instancesWith(t, 1, immediate(5))[0]
			submit(t, b, newOp(t, w, "BET", "bet-1", "30.00", ""))
			outbox := db.count(t, `SELECT count(*) FROM outbox_events`)

			db.mustExec(t, sc.ddl)
			_, err := b.ResolveNextPendingReference(context.Background())
			if err == nil {
				t.Fatal("step succeeded despite the forced failure")
			}
			if errors.Is(err, usecase.ErrCommitUnknown) {
				t.Fatalf("a server-reported failure must not be classified as unknown: %v", err)
			}
			status, _, attempts := db.txState(t, pending.TransactionID)
			if status != "PENDING_REFERENCE" || attempts != 0 {
				t.Fatalf("after the failure: %s attempts %d, want PENDING_REFERENCE untouched", status, attempts)
			}
			requireWallet(t, db, w.ID, 7000, 2)
			if n := db.count(t, `SELECT count(*) FROM outbox_events`); n != outbox {
				t.Fatalf("outbox rows = %d, want %d", n, outbox)
			}

			db.mustExec(t, `DROP TRIGGER test_fail ON outbox_events; DROP FUNCTION test_fail()`)
			requireStep(t, step(t, b), pending.TransactionID, domain.StatusProcessed, "")
			requireWallet(t, db, w.ID, 10000, 3)
		})
	}
}

func TestPending_ProviderCannotResolveAgainstAnotherProvidersTransaction(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(1))[0]
	w := openFunded(t, svc, "100.00")
	submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", "")) // provider-a

	rollback := newOp(t, w, "ROLLBACK", "rb-1", "30.00", "bet-1")
	res, err := svc.Process(context.Background(), "provider-b", rollback)
	if err != nil {
		t.Fatal(err)
	}
	requireResult(t, res, domain.StatusPendingReference, "", "") // a BET do provedor-a é invisível para o b
	requireStep(t, step(t, svc), res.TransactionID, domain.StatusRejected, "REFERENCE_NOT_FOUND")
	requireWallet(t, db, w.ID, 7000, 2)
}
