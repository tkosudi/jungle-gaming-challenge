//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

// objectSnapshot lista tabelas, funções e triggers do schema público, para comparar estados de migration.
func (db *schemaDB) objectSnapshot(t *testing.T) []string {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), `
		SELECT 'rel:' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname NOT LIKE 'schema_migrations%'
		UNION ALL SELECT 'fn:' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'
		UNION ALL SELECT 'trg:' || tgname FROM pg_trigger WHERE NOT tgisinternal
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestReferenceConsistency_MigrationUpDownUp(t *testing.T) {
	dbURL := createDatabase(t)
	m := newMigrator(t, dbURL)
	if err := m.Migrate(2); err != nil {
		t.Fatalf("migrate to 2: %v", err)
	}
	db := &schemaDB{pool: openPool(t, dbURL), url: dbURL}
	baseline := db.objectSnapshot(t)

	if err := m.Migrate(3); err != nil {
		t.Fatalf("up: %v", err)
	}
	withTrigger := db.objectSnapshot(t)
	if len(withTrigger) != len(baseline)+2 { // função + trigger
		t.Fatalf("migration 3 added %d objects, want 2", len(withTrigger)-len(baseline))
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down one step: %v", err)
	}
	after := db.objectSnapshot(t)
	if fmt.Sprint(after) != fmt.Sprint(baseline) {
		t.Fatalf("objects after down = %v, want %v", after, baseline)
	}
	if err := m.Migrate(3); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := db.objectSnapshot(t); fmt.Sprint(got) != fmt.Sprint(withTrigger) {
		t.Fatalf("objects after second up differ from the first up")
	}
}

// Insere direto no banco uma transação PROCESSED que referencia outra; o trigger de referência
// dispara no INSERT, antes das verificações diferidas do ledger.
func TestReferenceConsistency_ProcessedReferencesAreValidatedByTheDatabase(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "200.00")
	bet := submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	win := submit(t, svc, newOp(t, w, "WIN", "win-1", "10.00", ""))
	rejected := submit(t, svc, newOp(t, w, "BET", "bet-rejected", "9999.00", ""))
	other, err := domain.NewOperation(domain.OperationInput{
		Kind: "BET", ExternalTransactionID: "bet-round-2", IdempotencyKey: "key-bet-round-2",
		PlayerID: w.PlayerID.String(), WalletID: w.ID.String(), RoundID: "round-2", GameID: "game-1", Amount: "30.00", Currency: "BRL",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRound := submit(t, svc, other)

	insert := func(kind, amount string, ref uuid.UUID, external string) error {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, `
			INSERT INTO wager_transactions (id, kind, status, wallet_id, player_id, amount, currency, provider_id,
				external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
				reference_external_transaction_id, reference_transaction_id, result_balance, created_at, updated_at)
			VALUES (gen_random_uuid(), $1, 'PROCESSED', $2, $3, $4::numeric * 100, 'BRL', 'provider-a',
				$5, 'key-' || $5, decode(repeat('ab', 32), 'hex'), 'round-1', 'game-1',
				'ref-ext', $6, 0, now(), now())`, kind, w.ID, w.PlayerID, amount, external, ref)
		return err
	}

	t.Run("a consistent reference is accepted by the trigger", func(t *testing.T) {
		for _, tc := range []struct {
			kind, amount string
			ref          uuid.UUID
		}{
			{"REFUND", "30.00", bet.TransactionID},
			{"ROLLBACK", "30.00", bet.TransactionID},
			{"ROLLBACK", "10.00", win.TransactionID},
			{"WIN", "99.00", bet.TransactionID}, // WIN não exige valor igual
		} {
			if err := insert(tc.kind, tc.amount, tc.ref, "ok-"+tc.kind); err != nil {
				t.Errorf("%s of %s rejected: %v", tc.kind, tc.ref, err)
			}
		}
	})

	rejections := []struct {
		name         string
		kind, amount string
		ref          uuid.UUID
	}{
		{"REFUND with another amount", "REFUND", "29.99", bet.TransactionID},
		{"ROLLBACK with another amount", "ROLLBACK", "9.99", win.TransactionID},
		{"REFUND of a WIN", "REFUND", "10.00", win.TransactionID},
		{"WIN of a WIN", "WIN", "10.00", win.TransactionID},
		{"REFUND of a rejected BET", "REFUND", "9999.00", rejected.TransactionID},
		{"REFUND of another round", "REFUND", "30.00", otherRound.TransactionID},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(tc.kind, tc.amount, tc.ref, "bad-"+tc.kind)
			requirePgError(t, err, sqlstateCheckViolation, "transaction_reference_consistency")
		})
	}
}

func TestReferenceConsistency_ResolvingAPendingAgainstAnInconsistentReferenceIsRefused(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instancesWith(t, 1, immediate(5))[0]
	w := openFunded(t, svc, "100.00")
	win := submit(t, svc, newOp(t, w, "WIN", "win-1", "10.00", ""))
	pending := submit(t, svc, newOp(t, w, "REFUND", "refund-1", "10.00", "bet-1"))

	_, err := db.pool.Exec(context.Background(), `
		UPDATE wager_transactions
		   SET status = 'PROCESSED', result_balance = 0, reference_transaction_id = $2, next_attempt_at = NULL
		 WHERE id = $1`, pending.TransactionID, win.TransactionID)
	requirePgError(t, err, sqlstateCheckViolation, "transaction_reference_consistency")
	if status, _, _ := db.txState(t, pending.TransactionID); status != "PENDING_REFERENCE" {
		t.Fatalf("status = %s, want PENDING_REFERENCE", status)
	}
}
