//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

const consumer = "wager-transactions-consumer"

// message monta a mensagem da inbox; o hash representa o conteúdo (aqui, derivado de um rótulo).
func message(id, content string) usecase.InboxMessage {
	return usecase.InboxMessage{ConsumerName: consumer, MessageID: id, PayloadHash: sha256.Sum256([]byte(content)), ReceivedAt: time.Now()}
}

func processMessage(t *testing.T, svc *usecase.Service, msg usecase.InboxMessage, op domain.Operation) usecase.MessageResult {
	t.Helper()
	res, err := svc.ProcessMessage(context.Background(), msg, provider, op)
	if err != nil {
		t.Fatalf("ProcessMessage(%s): %v", msg.MessageID, err)
	}
	return res
}

func (db *schemaDB) inboxCount(t *testing.T) int {
	t.Helper()
	return db.count(t, `SELECT count(*) FROM inbox_messages`)
}

func TestProcessMessage_CompletesTheInboxInTheSameCommitAsTheOperation(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")

	res := processMessage(t, svc, message("msg-1", "bet-1"), newOp(t, w, "BET", "bet-1", "30.00", ""))
	if res.Duplicate || res.Replay || res.Status != domain.StatusProcessed || res.Balance.Amount() != "70.00" {
		t.Fatalf("result = %+v", res)
	}
	var txID uuid.UUID
	var outcome string
	err := db.pool.QueryRow(context.Background(),
		`SELECT transaction_id, outcome FROM inbox_messages WHERE consumer_name=$1 AND message_id='msg-1'`, consumer).Scan(&txID, &outcome)
	if err != nil || txID != res.TransactionID || outcome != "PROCESSED" {
		t.Fatalf("inbox = %s %s, %v", txID, outcome, err)
	}
	requireWallet(t, db, w.ID, 7000, 2)

	t.Run("redelivery with the same content is a duplicate without effects", func(t *testing.T) {
		again := processMessage(t, svc, message("msg-1", "bet-1"), newOp(t, w, "BET", "bet-1", "30.00", ""))
		if !again.Duplicate || again.TransactionID != res.TransactionID || again.Status != domain.StatusProcessed {
			t.Fatalf("duplicate = %+v", again)
		}
		requireWallet(t, db, w.ID, 7000, 2)
		if n := db.inboxCount(t); n != 1 {
			t.Fatalf("inbox rows = %d, want 1", n)
		}
	})
	t.Run("same message id with another content is a permanent conflict", func(t *testing.T) {
		_, err := svc.ProcessMessage(context.Background(), message("msg-1", "bet-2"), provider, newOp(t, w, "BET", "bet-2", "10.00", ""))
		if !errors.Is(err, usecase.ErrMessageConflict) {
			t.Fatalf("error = %v, want ErrMessageConflict", err)
		}
		requireWallet(t, db, w.ID, 7000, 2)
		if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id='bet-2'`); n != 0 {
			t.Fatalf("the conflicting operation was persisted")
		}
	})
	t.Run("another message for an operation already applied by HTTP is a replay", func(t *testing.T) {
		viaHTTP := submit(t, svc, newOp(t, w, "BET", "bet-3", "5.00", ""))
		viaSQS := processMessage(t, svc, message("msg-3", "bet-3"), newOp(t, w, "BET", "bet-3", "5.00", ""))
		if viaSQS.Duplicate || !viaSQS.Replay || viaSQS.TransactionID != viaHTTP.TransactionID {
			t.Fatalf("sqs after http = %+v, want replay of %s", viaSQS, viaHTTP.TransactionID)
		}
		requireWallet(t, db, w.ID, 6500, 3)
	})
	t.Run("rejections and pending references are valid completions", func(t *testing.T) {
		rejected := processMessage(t, svc, message("msg-4", "bet-big"), newOp(t, w, "BET", "bet-big", "999.00", ""))
		pending := processMessage(t, svc, message("msg-5", "rb"), newOp(t, w, "ROLLBACK", "rb", "1.00", "bet-later"))
		if rejected.Status != domain.StatusRejected || pending.Status != domain.StatusPendingReference {
			t.Fatalf("rejected = %+v, pending = %+v", rejected, pending)
		}
		if n := db.count(t, `SELECT count(*) FROM inbox_messages WHERE outcome IN ('REJECTED','PENDING_REFERENCE')`); n != 2 {
			t.Fatalf("inbox completions = %d, want 2", n)
		}
	})
}

func TestProcessMessage_FiftyEquivalentMessagesApplyOneOperation(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)
	w := openFunded(t, instances[0], "100.00")
	op := newOp(t, w, "BET", "bet-1", "25.00", "")

	for _, tc := range []struct {
		name string
		id   func(i int) string
	}{
		{"same message redelivered", func(int) string { return "msg-same" }},
		{"distinct messages for the same operation", func(i int) string { return fmt.Sprintf("msg-%d", i) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := make([]usecase.MessageResult, 50)
			errs := make([]error, 50)
			runConcurrently(50, func(i int) {
				results[i], errs[i] = instances[i%3].ProcessMessage(context.Background(), message(tc.id(i), "bet-1"), provider, op)
			})
			for i, err := range errs {
				if err != nil {
					t.Fatalf("message %d: %v", i, err)
				}
				if results[i].TransactionID != results[0].TransactionID {
					t.Fatalf("message %d resolved to another transaction", i)
				}
			}
		})
	}
	requireWallet(t, db, w.ID, 7500, 2)
	if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID); n != 1 {
		t.Fatalf("debits = %d, want 1", n)
	}
	if n := db.inboxCount(t); n != 51 {
		t.Fatalf("inbox rows = %d, want 1 + 50", n)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE event_type='WalletBalanceChanged'`); n != 2 {
		t.Fatalf("balance events = %d, want opening + one debit", n)
	}
}

func TestProcessMessage_ConcurrentConflictingContentForOneMessageID(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 2)
	w := openFunded(t, instances[0], "100.00")
	ops := []domain.Operation{newOp(t, w, "BET", "bet-a", "10.00", ""), newOp(t, w, "BET", "bet-b", "20.00", "")}

	for round := 0; round < 5; round++ {
		id := fmt.Sprintf("msg-%d", round)
		errs := make([]error, 2)
		runConcurrently(2, func(i int) {
			op := ops[i]
			op.ExternalTransactionID += fmt.Sprintf("-%d", round)
			op.IdempotencyKey += fmt.Sprintf("-%d", round)
			_, errs[i] = instances[i].ProcessMessage(context.Background(), message(id, fmt.Sprintf("%d-%d", round, i)), provider, op)
		})
		ok, conflicts := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, usecase.ErrMessageConflict):
				conflicts++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if ok != 1 || conflicts != 1 {
			t.Fatalf("round %d: ok=%d conflicts=%d", round, ok, conflicts)
		}
	}
	// Só a operação vencedora de cada rodada deixou efeito.
	if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 5 {
		t.Fatalf("bets = %d, want 5", n)
	}
	if n := db.inboxCount(t); n != 5 {
		t.Fatalf("inbox rows = %d, want 5", n)
	}
}

func TestProcessMessage_FailureLeavesNoInboxNorFinancialState(t *testing.T) {
	scenarios := []struct{ name, ddl string }{
		{"immediate failure on the inbox insert", `
			CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;
			CREATE TRIGGER test_fail BEFORE INSERT ON inbox_messages FOR EACH ROW EXECUTE FUNCTION test_fail();`},
		{"deferred failure at commit", `
			CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;
			CREATE CONSTRAINT TRIGGER test_fail AFTER INSERT ON inbox_messages DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION test_fail();`},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			db := newSchemaDB(t)
			svc := db.instances(t, 1)[0]
			w := openFunded(t, svc, "100.00")
			outbox := db.count(t, `SELECT count(*) FROM outbox_events`)
			op := newOp(t, w, "BET", "bet-1", "30.00", "")

			db.mustExec(t, sc.ddl)
			if _, err := svc.ProcessMessage(context.Background(), message("msg-1", "bet-1"), provider, op); err == nil {
				t.Fatal("ProcessMessage succeeded despite the forced failure")
			}
			requireWallet(t, db, w.ID, 10000, 1)
			for table, want := range map[string]int{"inbox_messages": 0, "outbox_events": outbox} {
				if n := db.count(t, `SELECT count(*) FROM `+table); n != want {
					t.Fatalf("%s rows = %d, want %d", table, n, want)
				}
			}
			if n := db.count(t, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 0 {
				t.Fatalf("bet rows = %d, want 0", n)
			}

			db.mustExec(t, `DROP TRIGGER test_fail ON inbox_messages; DROP FUNCTION test_fail()`)
			res := processMessage(t, svc, message("msg-1", "bet-1"), op)
			if res.Duplicate || res.Status != domain.StatusProcessed {
				t.Fatalf("redelivery = %+v", res)
			}
			requireWallet(t, db, w.ID, 7000, 2)
		})
	}
}

func TestInbox_IsAppendOnly(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	processMessage(t, svc, message("msg-1", "bet-1"), newOp(t, w, "BET", "bet-1", "1.00", ""))
	ctx := context.Background()
	_, err := db.pool.Exec(ctx, `UPDATE inbox_messages SET outcome='REJECTED'`)
	requirePgError(t, err, sqlstateRestrictViolation, "")
	_, err = db.pool.Exec(ctx, `DELETE FROM inbox_messages`)
	requirePgError(t, err, sqlstateRestrictViolation, "")
}

func TestInbox_MigrationUpDownUp(t *testing.T) {
	dbURL := createDatabase(t)
	m := newMigrator(t, dbURL)
	if err := m.Migrate(3); err != nil {
		t.Fatalf("migrate to 3: %v", err)
	}
	db := &schemaDB{pool: openPool(t, dbURL), url: dbURL}
	baseline := db.objectSnapshot(t)
	if err := m.Migrate(4); err != nil {
		t.Fatalf("up: %v", err)
	}
	withInbox := db.objectSnapshot(t)
	if len(withInbox) <= len(baseline) {
		t.Fatal("migration 4 added no objects")
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := db.objectSnapshot(t); fmt.Sprint(got) != fmt.Sprint(baseline) {
		t.Fatalf("objects after down = %v, want %v", got, baseline)
	}
	if err := m.Migrate(4); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := db.objectSnapshot(t); fmt.Sprint(got) != fmt.Sprint(withInbox) {
		t.Fatal("objects after second up differ")
	}
}
