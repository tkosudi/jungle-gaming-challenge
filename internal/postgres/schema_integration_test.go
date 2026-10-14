//go:build integration

package postgres

import (
	"context"
	"errors"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cada teste cria um banco descartável no servidor do Compose e aplica as migrations
// reais com golang-migrate; o banco de desenvolvimento nunca é tocado.

const (
	sqlstateCheckViolation    = "23514"
	sqlstateUniqueViolation   = "23505"
	sqlstateForeignKey        = "23503"
	sqlstateRestrictViolation = "23001"
	sqlstateOutOfRange        = "22003"
)

type schemaDB struct {
	pool *pgxpool.Pool
	url  string
}

func createDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	adminURL := testDatabaseURL()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)

	name := "m11_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			t.Errorf("connect admin for cleanup: %v", err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func newMigrator(t *testing.T, dbURL string) *migrate.Migrate {
	t.Helper()
	dir, err := filepath.Abs("../../db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "pgx5"
	m, err := migrate.New("file://"+dir, u.String())
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func openPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := NewPool(context.Background(), dbURL, 5*time.Second)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newSchemaDB(t *testing.T) *schemaDB {
	t.Helper()
	dbURL := createDatabase(t)
	if err := newMigrator(t, dbURL).Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return &schemaDB{pool: openPool(t, dbURL), url: dbURL}
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func requirePgError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want PgError %s (%s)", err, code, constraint)
	}
	if pgErr.Code != code {
		t.Fatalf("sqlstate = %s (%s), want %s (%s): %s", pgErr.Code, pgErr.ConstraintName, code, constraint, pgErr.Message)
	}
	if constraint != "" && pgErr.ConstraintName != constraint {
		t.Fatalf("constraint = %q, want %q: %s", pgErr.ConstraintName, constraint, pgErr.Message)
	}
}

func ptr[T any](v T) *T { return &v }

// --- fixtures ---------------------------------------------------------------

type wallet struct {
	id, playerID, currency string
	balance, version       int64
}

type txSpec struct {
	id, walletID, playerID, kind, status, currency string
	amount                                         int64
	providerID, externalID, idempotencyKey         *string
	hash                                           []byte
	roundID, gameID, refExternalID, refTxID        *string
	failureCode                                    *string
	resultBalance                                  *int64
	attempts                                       int
	nextAttemptAt                                  *time.Time
}

func (s txSpec) insert(ctx context.Context, q execer) error {
	_, err := q.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, kind, status, wallet_id, player_id, amount, currency,
			provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, reference_external_transaction_id, reference_transaction_id,
			failure_code, result_balance, reference_attempts, next_attempt_at,
			created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19, now(), now())`,
		s.id, s.kind, s.status, s.walletID, s.playerID, s.amount, s.currency,
		s.providerID, s.externalID, s.idempotencyKey, s.hash,
		s.roundID, s.gameID, s.refExternalID, s.refTxID,
		s.failureCode, s.resultBalance, s.attempts, s.nextAttemptAt)
	return err
}

// newExternalTx devolve uma operação externa PROCESSED válida de provider-a.
func newExternalTx(w wallet, kind string, amount int64) txSpec {
	ext := "ext-" + uuid.NewString()
	return txSpec{
		id: uuid.NewString(), walletID: w.id, playerID: w.playerID,
		kind: kind, status: "PROCESSED", currency: w.currency, amount: amount,
		providerID: ptr("provider-a"), externalID: ptr(ext), idempotencyKey: ptr("provider-a:" + ext),
		hash:    make([]byte, 32),
		roundID: ptr("round-1"), gameID: ptr("game-1"),
		resultBalance: ptr(w.balance),
	}
}

type ledgerSpec struct {
	walletID, txID, direction string
	amount, before, after     int64
	version                   int64
}

func (e ledgerSpec) insert(ctx context.Context, q execer) error {
	_, err := q.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount, balance_before, balance_after, wallet_version, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())`,
		uuid.NewString(), e.walletID, e.txID, e.direction, e.amount, e.before, e.after, e.version)
	return err
}

func insertWallet(ctx context.Context, q execer, w wallet) error {
	_, err := q.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5, now(), now())`,
		w.id, w.playerID, w.currency, w.balance, w.version)
	return err
}

func newWallet(balance int64) wallet {
	return wallet{id: uuid.NewString(), playerID: uuid.NewString(), currency: "BRL", balance: balance, version: 1}
}

func openingTx(w wallet) txSpec {
	return txSpec{
		id: uuid.NewString(), walletID: w.id, playerID: w.playerID,
		kind: "OPENING", status: "PROCESSED", currency: w.currency, amount: w.balance,
		resultBalance: ptr(w.balance),
	}
}

// openWallet cria carteira, OPENING e lançamento de crédito no mesmo commit, como o caso de uso fará.
func (db *schemaDB) openWallet(t *testing.T, balance int64) wallet {
	t.Helper()
	ctx := context.Background()
	w := newWallet(balance)
	err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
		if err := insertWallet(ctx, tx, w); err != nil {
			return err
		}
		if balance == 0 {
			return nil
		}
		op := openingTx(w)
		if err := op.insert(ctx, tx); err != nil {
			return err
		}
		return ledgerSpec{walletID: w.id, txID: op.id, direction: "CREDIT", amount: balance, before: 0, after: balance, version: 1}.insert(ctx, tx)
	})
	if err != nil {
		t.Fatalf("openWallet: %v", err)
	}
	return w
}

// moveTx aplica carteira + transação + lançamento em um commit.
func (db *schemaDB) moveTx(w wallet, spec txSpec, direction string) (wallet, error) {
	ctx := context.Background()
	next := w
	next.version++
	if direction == "DEBIT" {
		next.balance -= spec.amount
	} else {
		next.balance += spec.amount
	}
	spec.resultBalance = ptr(next.balance)
	err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
		if err := spec.insert(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=$2, version=$3, updated_at=now() WHERE id=$1`,
			w.id, next.balance, next.version); err != nil {
			return err
		}
		return ledgerSpec{walletID: w.id, txID: spec.id, direction: direction, amount: spec.amount,
			before: w.balance, after: next.balance, version: next.version}.insert(ctx, tx)
	})
	if err != nil {
		return w, err
	}
	return next, nil
}

func (db *schemaDB) move(t *testing.T, w wallet, spec txSpec, direction string) wallet {
	t.Helper()
	next, err := db.moveTx(w, spec, direction)
	if err != nil {
		t.Fatalf("move %s %s: %v", spec.kind, direction, err)
	}
	return next
}

func (db *schemaDB) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := db.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (db *schemaDB) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// --- migrations -------------------------------------------------------------

func TestMigrations_UpDownUpLeavesNoResidue(t *testing.T) {
	dbURL := createDatabase(t)
	m := newMigrator(t, dbURL)

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}

	pool := openPool(t, dbURL)
	db := &schemaDB{pool: pool, url: dbURL}
	residue := map[string]string{
		"relations": `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname NOT LIKE 'schema_migrations%'`,
		"functions": `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public'`,
		"triggers": `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal`,
	}
	for what, query := range residue {
		if n := db.count(t, query); n != 0 {
			t.Errorf("%s left after down: %d", what, n)
		}
	}

	if err := m.Up(); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if n := db.count(t, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN ('wallets','wager_transactions','wallet_ledger_entries')`); n != 3 {
		t.Fatalf("tables after second up = %d, want 3", n)
	}
}

// --- wallets ----------------------------------------------------------------

func TestWallets_OnePerPlayerAndCurrency(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	first := db.openWallet(t, 0)

	dup := newWallet(0)
	dup.playerID = first.playerID
	requirePgError(t, insertWallet(ctx, db.pool, dup), sqlstateUniqueViolation, "wallets_player_currency_key")

	other := newWallet(0)
	other.playerID, other.currency = first.playerID, "USD"
	if err := insertWallet(ctx, db.pool, other); err != nil {
		t.Fatalf("same player, other currency must be accepted: %v", err)
	}
}

func TestWallets_RejectInvalidValues(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	tests := []struct {
		name       string
		mutate     func(*wallet)
		constraint string
	}{
		{"negative balance", func(w *wallet) { w.balance = -1 }, "wallets_balance_non_negative"},
		{"lowercase currency", func(w *wallet) { w.currency = "brl" }, "wallets_currency_format"},
		{"long currency", func(w *wallet) { w.currency = "BRLX" }, "wallets_currency_format"},
		{"version zero", func(w *wallet) { w.version = 0 }, "wallets_version_positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWallet(0)
			tc.mutate(&w)
			requirePgError(t, insertWallet(ctx, db.pool, w), sqlstateCheckViolation, tc.constraint)
		})
	}
}

func TestWallets_BalanceOverflowFailsInsteadOfWrapping(t *testing.T) {
	db := newSchemaDB(t)
	w := db.openWallet(t, math.MaxInt64)
	_, err := db.pool.Exec(context.Background(), `UPDATE wallets SET balance = balance + 1 WHERE id = $1`, w.id)
	requirePgError(t, err, sqlstateOutOfRange, "")
}

func TestWallets_IdentityIsImmutableAndRowsCannotBeRemoved(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)

	for name, sql := range map[string]string{
		"currency": `UPDATE wallets SET currency = 'USD' WHERE id = $1`,
		"player":   `UPDATE wallets SET player_id = gen_random_uuid() WHERE id = $1`,
	} {
		_, err := db.pool.Exec(ctx, sql, w.id)
		t.Run("update "+name, func(t *testing.T) { requirePgError(t, err, sqlstateRestrictViolation, "wallets_identity_immutable") })
	}
	_, err := db.pool.Exec(ctx, `DELETE FROM wallets WHERE id = $1`, w.id)
	requirePgError(t, err, sqlstateRestrictViolation, "")
	_, err = db.pool.Exec(ctx, `TRUNCATE wallets CASCADE`)
	requirePgError(t, err, sqlstateRestrictViolation, "")
	if n := db.count(t, `SELECT count(*) FROM wallets`); n != 1 {
		t.Fatalf("wallets = %d, want 1", n)
	}
}

func TestWallets_VersionFollowsBalanceChanges(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)

	tests := []struct{ name, sql string }{
		{"balance changed without version bump", `UPDATE wallets SET balance = 900 WHERE id = $1`},
		{"version skipped", `UPDATE wallets SET balance = 900, version = 3 WHERE id = $1`},
		{"version bumped without balance change", `UPDATE wallets SET version = 2 WHERE id = $1`},
		{"version decreased", `UPDATE wallets SET balance = 900, version = 0 WHERE id = $1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.pool.Exec(ctx, tc.sql, w.id)
			if err == nil {
				t.Fatal("update accepted, want rejection")
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != sqlstateCheckViolation {
				t.Fatalf("error = %v, want check violation", err)
			}
		})
	}
}

// --- wallet <-> ledger correspondence --------------------------------------

func TestLedger_CommittedMovementKeepsWalletAndLedgerAligned(t *testing.T) {
	db := newSchemaDB(t)
	w := db.openWallet(t, 100000)
	w = db.move(t, w, newExternalTx(w, "BET", 2500), "DEBIT")

	var balance, version int64
	err := db.pool.QueryRow(context.Background(), `SELECT balance, version FROM wallets WHERE id=$1`, w.id).Scan(&balance, &version)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 97500 || version != 2 {
		t.Fatalf("wallet = %d v%d, want 97500 v2", balance, version)
	}
	if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, w.id); n != 2 {
		t.Fatalf("ledger entries = %d, want 2 (opening + bet)", n)
	}
}

func TestLedger_ZeroOpeningThenFirstMovementIsVersionTwo(t *testing.T) {
	db := newSchemaDB(t)
	w := db.openWallet(t, 0)
	spec := newExternalTx(w, "WIN", 500)
	db.move(t, w, spec, "CREDIT")
}

func TestLedger_TwoMovementsInOneCommit(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	a, b := newExternalTx(w, "BET", 100), newExternalTx(w, "BET", 50)
	a.resultBalance, b.resultBalance = ptr(int64(900)), ptr(int64(850))

	err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
		for _, s := range []txSpec{a, b} {
			if err := s.insert(ctx, tx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=900, version=2 WHERE id=$1`, w.id); err != nil {
			return err
		}
		if err := (ledgerSpec{walletID: w.id, txID: a.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}).insert(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=850, version=3 WHERE id=$1`, w.id); err != nil {
			return err
		}
		return ledgerSpec{walletID: w.id, txID: b.id, direction: "DEBIT", amount: 50, before: 900, after: 850, version: 3}.insert(ctx, tx)
	})
	if err != nil {
		t.Fatalf("two movements: %v", err)
	}
}

func TestLedger_RejectsInconsistentCommits(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	other := db.openWallet(t, 1000)
	// BET já confirmada da outra carteira: o lançamento indevido chega sozinho, sem enfileirar a transação.
	otherBet := newExternalTx(other, "BET", 100)
	other = db.move(t, other, otherBet, "DEBIT")

	bet := func(after int64) txSpec {
		s := newExternalTx(w, "BET", 100)
		s.resultBalance = ptr(after)
		return s
	}
	tests := []struct {
		name       string
		run        func(tx pgx.Tx) error
		constraint string
	}{
		{
			"balance changed without ledger entry",
			func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE wallets SET balance=900, version=2 WHERE id=$1`, w.id)
				return err
			},
			"wallet_matches_ledger",
		},
		{
			"wallet created with balance and no ledger",
			func(tx pgx.Tx) error { return insertWallet(ctx, tx, newWallet(500)) },
			"wallet_matches_ledger",
		},
		{
			"entry balance_before is not the real previous balance",
			func(tx pgx.Tx) error {
				s := bet(800)
				if err := s.insert(ctx, tx); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=800, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 100, before: 900, after: 800, version: 2}.insert(ctx, tx)
			},
			"ledger_entry_consistency",
		},
		{
			"wallet balance differs from last entry balance_after",
			func(tx pgx.Tx) error {
				s := bet(900)
				if err := s.insert(ctx, tx); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=850, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}.insert(ctx, tx)
			},
			"wallet_matches_ledger",
		},
		{
			"entry for LOSS",
			func(tx pgx.Tx) error {
				s := newExternalTx(w, "LOSS", 0)
				if err := s.insert(ctx, tx); err != nil {
					return err
				}
				// amount > 0 é exigido pelo ledger; um LOSS nunca pode ter lançamento.
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=999, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 1, before: 1000, after: 999, version: 2}.insert(ctx, tx)
			},
			"ledger_entry_consistency",
		},
		{
			"entry amount differs from transaction amount",
			func(tx pgx.Tx) error {
				s := bet(901)
				if err := s.insert(ctx, tx); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=901, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 99, before: 1000, after: 901, version: 2}.insert(ctx, tx)
			},
			"ledger_entry_consistency",
		},
		{
			"entry wallet differs from transaction wallet",
			func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=900, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: otherBet.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}.insert(ctx, tx)
			},
			"ledger_entry_consistency",
		},
		{
			"BET recorded as credit",
			func(tx pgx.Tx) error {
				s := bet(1100)
				if err := s.insert(ctx, tx); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=1100, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: s.id, direction: "CREDIT", amount: 100, before: 1000, after: 1100, version: 2}.insert(ctx, tx)
			},
			"ledger_entry_consistency",
		},
		{
			"entry for a transaction that is not PROCESSED",
			func(tx pgx.Tx) error {
				s := bet(900)
				s.status, s.resultBalance, s.failureCode = "REJECTED", nil, ptr("INSUFFICIENT_FUNDS")
				if err := s.insert(ctx, tx); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=900, version=2 WHERE id=$1`, w.id); err != nil {
					return err
				}
				return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}.insert(ctx, tx)
			},
			"ledger_entry_consistency",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := inTx(ctx, db.pool, tc.run)
			requirePgError(t, err, sqlstateCheckViolation, tc.constraint)
		})
	}

	var balance int64
	if err := db.pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE id=$1`, w.id).Scan(&balance); err != nil || balance != 1000 {
		t.Fatalf("wallet after rejected commits = %d (%v), want 1000", balance, err)
	}
}

func TestLedger_RollbackDirectionIsOppositeOfReferencedMovement(t *testing.T) {
	db := newSchemaDB(t)
	w := db.openWallet(t, 1000)
	win := newExternalTx(w, "WIN", 300)
	w = db.move(t, w, win, "CREDIT")

	rollback := newExternalTx(w, "ROLLBACK", 300)
	rollback.refExternalID, rollback.refTxID = win.externalID, &win.id

	if _, err := db.moveTx(w, rollback, "CREDIT"); err == nil {
		t.Fatal("ROLLBACK(WIN) as credit accepted, want rejection")
	} else {
		requirePgError(t, err, sqlstateCheckViolation, "ledger_entry_consistency")
	}
	db.move(t, w, rollback, "DEBIT")
}

func TestLedger_NegativeBalanceCannotBeCommitted(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
		s := newExternalTx(w, "BET", 1001)
		if err := s.insert(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE wallets SET balance = balance - 1001, version = 2 WHERE id = $1`, w.id)
		return err
	})
	requirePgError(t, err, sqlstateCheckViolation, "wallets_balance_non_negative")
}

// --- ledger append-only e aritmética ---------------------------------------

func TestLedger_IsAppendOnly(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)

	for name, sql := range map[string]string{
		"update":   `UPDATE wallet_ledger_entries SET amount = amount WHERE wallet_id = $1`,
		"delete":   `DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`,
		"truncate": `TRUNCATE wallet_ledger_entries`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.pool.Exec(ctx, sql, argsFor(sql, w.id)...)
			requirePgError(t, err, sqlstateRestrictViolation, "")
		})
	}
	if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries`); n != 1 {
		t.Fatalf("ledger entries = %d, want 1", n)
	}
}

func argsFor(sql string, args ...any) []any {
	if strings.Contains(sql, "$1") {
		return args
	}
	return nil
}

func TestLedger_ArithmeticAndShapeConstraints(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	spec := newExternalTx(w, "BET", 100)
	spec.status, spec.resultBalance = "PENDING", nil
	if err := spec.insert(ctx, db.pool); err != nil {
		t.Fatal(err)
	}

	base := ledgerSpec{walletID: w.id, txID: spec.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}
	tests := []struct {
		name       string
		mutate     func(*ledgerSpec)
		constraint string
	}{
		{"debit with wrong balance_after", func(e *ledgerSpec) { e.after = 901 }, "ledger_arithmetic"},
		{"credit that subtracts", func(e *ledgerSpec) { e.direction = "CREDIT" }, "ledger_arithmetic"},
		{"zero amount", func(e *ledgerSpec) { e.amount, e.after = 0, 1000 }, "ledger_amount_positive"},
		{"negative balance_after", func(e *ledgerSpec) { e.amount, e.after = 1001, -1 }, "ledger_balances_non_negative"},
		{"unknown direction", func(e *ledgerSpec) { e.direction = "SIDEWAYS" }, ""},
		{"version zero", func(e *ledgerSpec) { e.version = 0 }, "ledger_version_positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.mutate(&e)
			requirePgError(t, e.insert(ctx, db.pool), sqlstateCheckViolation, tc.constraint)
		})
	}
}

func TestLedger_BigintOverflowInArithmeticFails(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	spec := newExternalTx(w, "WIN", 1)
	spec.status, spec.resultBalance = "PENDING", nil
	if err := spec.insert(ctx, db.pool); err != nil {
		t.Fatal(err)
	}
	e := ledgerSpec{walletID: w.id, txID: spec.id, direction: "CREDIT", amount: 1, before: math.MaxInt64, after: 0, version: 2}
	requirePgError(t, e.insert(ctx, db.pool), sqlstateOutOfRange, "")
}

func TestLedger_OneEntryPerWalletTransactionAndVersion(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	var openingID string
	if err := db.pool.QueryRow(ctx, `SELECT transaction_id FROM wallet_ledger_entries WHERE wallet_id=$1`, w.id).Scan(&openingID); err != nil {
		t.Fatal(err)
	}

	dupTx := ledgerSpec{walletID: w.id, txID: openingID, direction: "CREDIT", amount: 1000, before: 0, after: 1000, version: 5}
	requirePgError(t, dupTx.insert(ctx, db.pool), sqlstateUniqueViolation, "ledger_wallet_transaction_key")

	spec := newExternalTx(w, "WIN", 10)
	spec.status, spec.resultBalance = "PENDING", nil
	if err := spec.insert(ctx, db.pool); err != nil {
		t.Fatal(err)
	}
	dupVersion := ledgerSpec{walletID: w.id, txID: spec.id, direction: "CREDIT", amount: 10, before: 1000, after: 1010, version: 1}
	requirePgError(t, dupVersion.insert(ctx, db.pool), sqlstateUniqueViolation, "ledger_wallet_version_key")
}

// Backstop, não o mecanismo principal (que será SELECT ... FOR UPDATE no caso de uso):
// um escritor que leu a versão antiga e grava version = antiga+1 depois de outro commit é recusado.
func TestWallets_StaleWriterIsRejected(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)

	t1, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer t2.Rollback(ctx)

	spec := newExternalTx(w, "BET", 100)
	spec.resultBalance = ptr(int64(900))
	if err := spec.insert(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Exec(ctx, `UPDATE wallets SET balance=900, version=2 WHERE id=$1`, w.id); err != nil {
		t.Fatal(err)
	}

	stale := make(chan error, 1)
	go func() {
		// Calculado da leitura antiga (saldo 1000, versão 1) e visando a versão 2: bloqueia até o commit de t1.
		_, err := t2.Exec(ctx, `UPDATE wallets SET balance=850, version=2 WHERE id=$1`, w.id)
		stale <- err
	}()
	time.Sleep(300 * time.Millisecond)

	if err := (ledgerSpec{walletID: w.id, txID: spec.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}).insert(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatalf("first writer commit: %v", err)
	}

	select {
	case err := <-stale:
		requirePgError(t, err, sqlstateCheckViolation, "wallets_version_follows_balance")
	case <-time.After(5 * time.Second):
		t.Fatal("stale writer still blocked after first commit")
	}
}

// --- wager_transactions -----------------------------------------------------

func TestWagerTransactions_RejectInvalidShapes(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	ref := newExternalTx(w, "BET", 100)
	w = db.move(t, w, ref, "DEBIT")
	soon := time.Now().Add(time.Minute)

	tests := []struct {
		name       string
		mutate     func(*txSpec)
		constraint string
	}{
		{"unknown kind", func(s *txSpec) { s.kind = "TRANSFER" }, "wager_tx_kind_valid"},
		{"unknown status", func(s *txSpec) { s.status = "DONE" }, "wager_tx_state_allowed"},
		{"lowercase currency", func(s *txSpec) { s.currency = "brl" }, "wager_tx_currency_format"},
		{"LOSS with amount", func(s *txSpec) { s.kind, s.amount = "LOSS", 1 }, "wager_tx_amount_policy"},
		{"BET with zero amount", func(s *txSpec) { s.amount = 0 }, "wager_tx_amount_policy"},
		{"negative amount", func(s *txSpec) { s.amount = -5 }, "wager_tx_amount_policy"},
		{"external without provider", func(s *txSpec) { s.providerID = nil }, "wager_tx_origin_fields"},
		{"empty provider", func(s *txSpec) { s.providerID = ptr("") }, "wager_tx_origin_fields"},
		{"empty external id", func(s *txSpec) { s.externalID = ptr("") }, "wager_tx_origin_fields"},
		{"empty idempotency key", func(s *txSpec) { s.idempotencyKey = ptr("") }, "wager_tx_origin_fields"},
		{"external without payload hash", func(s *txSpec) { s.hash = nil }, "wager_tx_origin_fields"},
		{"empty round", func(s *txSpec) { s.roundID = ptr("") }, "wager_tx_origin_fields"},
		{"empty game", func(s *txSpec) { s.gameID = ptr("") }, "wager_tx_origin_fields"},
		{"payload hash is not 32 bytes", func(s *txSpec) { s.hash = make([]byte, 31) }, "wager_tx_payload_hash_size"},
		{"OPENING carrying external metadata", func(s *txSpec) { s.kind = "OPENING" }, "wager_tx_origin_fields"},
		{"REFUND without reference", func(s *txSpec) { s.kind = "REFUND" }, "wager_tx_reference_policy"},
		{"ROLLBACK without reference", func(s *txSpec) { s.kind = "ROLLBACK" }, "wager_tx_reference_policy"},
		{"BET with reference", func(s *txSpec) { s.refExternalID = ptr("x") }, "wager_tx_reference_policy"},
		{"PROCESSED without result balance", func(s *txSpec) { s.resultBalance = nil }, "wager_tx_state_fields"},
		{"PROCESSED with failure code", func(s *txSpec) { s.failureCode = ptr("X") }, "wager_tx_state_fields"},
		{"PROCESSED with next attempt", func(s *txSpec) { s.nextAttemptAt = &soon }, "wager_tx_state_fields"},
		{"PROCESSED reversal without resolved reference", func(s *txSpec) { s.kind, s.refExternalID = "REFUND", ptr("ext-x") }, "wager_tx_state_fields"},
		{"REJECTED without failure code", func(s *txSpec) { s.status, s.resultBalance = "REJECTED", nil }, "wager_tx_state_fields"},
		{"REJECTED with empty failure code", func(s *txSpec) { s.status, s.resultBalance, s.failureCode = "REJECTED", nil, ptr("") }, "wager_tx_state_fields"},
		{"FAILED without failure code", func(s *txSpec) { s.status, s.resultBalance = "FAILED", nil }, "wager_tx_state_fields"},
		{"PENDING_REFERENCE for BET", func(s *txSpec) { s.status, s.resultBalance, s.nextAttemptAt = "PENDING_REFERENCE", nil, &soon }, "wager_tx_state_fields"},
		{"PENDING_REFERENCE without next attempt", func(s *txSpec) {
			s.kind, s.status, s.resultBalance, s.refExternalID = "REFUND", "PENDING_REFERENCE", nil, ptr("ext-x")
		}, "wager_tx_state_fields"},
		{"PENDING_REFERENCE already resolved", func(s *txSpec) {
			s.kind, s.status, s.resultBalance, s.refExternalID, s.refTxID, s.nextAttemptAt = "REFUND", "PENDING_REFERENCE", nil, ptr("ext-x"), &ref.id, &soon
		}, "wager_tx_state_fields"},
		{"PENDING with result balance", func(s *txSpec) { s.status = "PENDING" }, "wager_tx_state_fields"},
		{"negative result balance", func(s *txSpec) {
			s.status, s.failureCode, s.resultBalance = "REJECTED", ptr("INSUFFICIENT_FUNDS"), ptr(int64(-1))
		}, "wager_tx_result_balance_non_negative"},
		{"negative attempts", func(s *txSpec) { s.attempts = -1 }, "wager_tx_attempts_non_negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newExternalTx(w, "BET", 100)
			tc.mutate(&s)
			requirePgError(t, s.insert(ctx, db.pool), sqlstateCheckViolation, tc.constraint)
		})
	}
}

func TestWagerTransactions_OpeningRules(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)

	t.Run("second OPENING for the same wallet", func(t *testing.T) {
		requirePgError(t, openingTx(w).insert(ctx, db.pool), sqlstateUniqueViolation, "wager_tx_one_opening_per_wallet")
	})
	t.Run("OPENING must be PROCESSED", func(t *testing.T) {
		z := db.openWallet(t, 0)
		op := openingTx(z)
		op.amount, op.status, op.resultBalance = 10, "PENDING", nil
		requirePgError(t, op.insert(ctx, db.pool), sqlstateCheckViolation, "wager_tx_opening_processed")
	})
	t.Run("OPENING with zero amount", func(t *testing.T) {
		z := db.openWallet(t, 0)
		requirePgError(t, openingTx(z).insert(ctx, db.pool), sqlstateCheckViolation, "wager_tx_amount_policy")
	})
}

func TestWagerTransactions_IdempotencyIsPerProvider(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	first := newExternalTx(w, "BET", 100)
	w = db.move(t, w, first, "DEBIT")

	sameExternal := newExternalTx(w, "BET", 100)
	sameExternal.externalID = first.externalID
	requirePgError(t, sameExternal.insert(ctx, db.pool), sqlstateUniqueViolation, "wager_tx_provider_external_key")

	sameKey := newExternalTx(w, "BET", 100)
	sameKey.idempotencyKey = first.idempotencyKey
	requirePgError(t, sameKey.insert(ctx, db.pool), sqlstateUniqueViolation, "wager_tx_provider_idempotency_key")

	otherProvider := newExternalTx(w, "BET", 100)
	otherProvider.providerID = ptr("provider-b")
	otherProvider.externalID, otherProvider.idempotencyKey = first.externalID, first.idempotencyKey
	db.move(t, w, otherProvider, "DEBIT") // mesmos ids sob outro provedor são aceitos
}

func TestWagerTransactions_ReferenceMustShareProviderAndWallet(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	otherWallet := db.openWallet(t, 1000)
	bet := newExternalTx(w, "BET", 100)
	w = db.move(t, w, bet, "DEBIT")

	refund := func(owner wallet, provider string) txSpec {
		s := newExternalTx(owner, "REFUND", 100)
		s.providerID, s.idempotencyKey = ptr(provider), ptr(provider+":"+*s.externalID)
		s.refExternalID, s.refTxID = bet.externalID, &bet.id
		return s
	}
	const fk = "wager_tx_reference_same_provider_wallet_fkey"
	requirePgError(t, refund(w, "provider-b").insert(ctx, db.pool), sqlstateForeignKey, fk)
	requirePgError(t, refund(otherWallet, "provider-a").insert(ctx, db.pool), sqlstateForeignKey, fk)
	db.move(t, w, refund(w, "provider-a"), "CREDIT")
}

func TestWagerTransactions_DEC001SingleDirectCompensation(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 100000)

	// Cada compensação PROCESSED é um movimento real (carteira + transação + lançamento).
	newBet := func(t *testing.T) txSpec {
		t.Helper()
		s := newExternalTx(w, "BET", 1000)
		w = db.move(t, w, s, "DEBIT")
		return s
	}
	compensation := func(kind string, of txSpec) txSpec {
		s := newExternalTx(w, kind, of.amount)
		s.refExternalID, s.refTxID = of.externalID, &of.id
		return s
	}
	// processed devolve o erro do commit; o direction é o do movimento que a compensação faz.
	processed := func(kind string, of txSpec, direction string) (txSpec, error) {
		s := compensation(kind, of)
		next, err := db.moveTx(w, s, direction)
		if err == nil {
			w = next
		}
		return s, err
	}
	const idx = "wager_tx_one_compensation_per_reference"

	t.Run("REFUND then ROLLBACK of the same BET", func(t *testing.T) {
		bet := newBet(t)
		if _, err := processed("REFUND", bet, "CREDIT"); err != nil {
			t.Fatal(err)
		}
		_, err := processed("ROLLBACK", bet, "CREDIT")
		requirePgError(t, err, sqlstateUniqueViolation, idx)
	})
	t.Run("ROLLBACK then REFUND of the same BET", func(t *testing.T) {
		bet := newBet(t)
		if _, err := processed("ROLLBACK", bet, "CREDIT"); err != nil {
			t.Fatal(err)
		}
		_, err := processed("REFUND", bet, "CREDIT")
		requirePgError(t, err, sqlstateUniqueViolation, idx)
	})
	t.Run("ROLLBACK(REFUND) does not free ROLLBACK(BET)", func(t *testing.T) {
		bet := newBet(t)
		refund, err := processed("REFUND", bet, "CREDIT")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := processed("ROLLBACK", refund, "DEBIT"); err != nil {
			t.Fatalf("ROLLBACK(REFUND): %v", err)
		}
		_, err = processed("ROLLBACK", bet, "CREDIT")
		requirePgError(t, err, sqlstateUniqueViolation, idx)
		_, err = processed("REFUND", bet, "CREDIT")
		requirePgError(t, err, sqlstateUniqueViolation, idx)
	})
	t.Run("rejected compensation does not consume the slot", func(t *testing.T) {
		bet := newBet(t)
		rejected := compensation("ROLLBACK", bet)
		rejected.status, rejected.resultBalance, rejected.failureCode = "REJECTED", nil, ptr("INSUFFICIENT_FUNDS")
		if err := rejected.insert(ctx, db.pool); err != nil {
			t.Fatal(err)
		}
		if _, err := processed("REFUND", bet, "CREDIT"); err != nil {
			t.Fatalf("REFUND after rejected ROLLBACK: %v", err)
		}
	})
}

func TestWagerTransactions_StateTransitionsAndImmutability(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	bet := newExternalTx(w, "BET", 100)
	w = db.move(t, w, bet, "DEBIT")
	soon := time.Now().Add(time.Minute)

	pendingRefund := func() txSpec {
		s := newExternalTx(w, "REFUND", 100)
		s.status, s.resultBalance, s.nextAttemptAt, s.refExternalID = "PENDING_REFERENCE", nil, &soon, bet.externalID
		if err := s.insert(ctx, db.pool); err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("terminal PROCESSED rejects any change", func(t *testing.T) {
		for name, sql := range map[string]string{
			"status":       `UPDATE wager_transactions SET status='REJECTED', failure_code='X', result_balance=NULL WHERE id=$1`,
			"failure code": `UPDATE wager_transactions SET failure_code='X' WHERE id=$1`,
			"result":       `UPDATE wager_transactions SET result_balance=1 WHERE id=$1`,
			"updated_at":   `UPDATE wager_transactions SET updated_at=now() WHERE id=$1`,
			"delete":       `DELETE FROM wager_transactions WHERE id=$1`,
		} {
			_, err := db.pool.Exec(ctx, sql, bet.id)
			t.Run(name, func(t *testing.T) { requirePgError(t, err, sqlstateRestrictViolation, "") })
		}
	})
	t.Run("terminal REJECTED cannot become PROCESSED", func(t *testing.T) {
		s := pendingRefund()
		db.mustExec(t, `UPDATE wager_transactions SET status='REJECTED', failure_code='REFERENCE_NOT_FOUND', next_attempt_at=NULL WHERE id=$1`, s.id)
		_, err := db.pool.Exec(ctx, `UPDATE wager_transactions SET status='PROCESSED', failure_code=NULL, result_balance=1000, reference_transaction_id=$2 WHERE id=$1`, s.id, bet.id)
		requirePgError(t, err, sqlstateRestrictViolation, "")
	})
	t.Run("PENDING_REFERENCE can be retried and then resolved with its ledger entry", func(t *testing.T) {
		s := pendingRefund()
		db.mustExec(t, `UPDATE wager_transactions SET reference_attempts = reference_attempts + 1, next_attempt_at = now() + interval '2 minutes' WHERE id=$1`, s.id)

		after := w.balance + s.amount
		err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE wager_transactions SET status='PROCESSED', reference_transaction_id=$2, result_balance=$3, next_attempt_at=NULL, updated_at=now() WHERE id=$1`, s.id, bet.id, after); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=$2, version=$3 WHERE id=$1`, w.id, after, w.version+1); err != nil {
				return err
			}
			return ledgerSpec{walletID: w.id, txID: s.id, direction: "CREDIT", amount: s.amount, before: w.balance, after: after, version: w.version + 1}.insert(ctx, tx)
		})
		if err != nil {
			t.Fatalf("resolve pending reference: %v", err)
		}
		w.balance, w.version = after, w.version+1
	})
	t.Run("identity of a pending operation is immutable", func(t *testing.T) {
		s := pendingRefund()
		for name, sql := range map[string]string{
			"amount":      `UPDATE wager_transactions SET amount = amount + 1 WHERE id=$1`,
			"kind":        `UPDATE wager_transactions SET kind = 'ROLLBACK' WHERE id=$1`,
			"wallet":      `UPDATE wager_transactions SET wallet_id = (SELECT id FROM wallets WHERE id <> wager_transactions.wallet_id LIMIT 1) WHERE id=$1`,
			"external id": `UPDATE wager_transactions SET external_transaction_id = 'other' WHERE id=$1`,
			"hash":        `UPDATE wager_transactions SET payload_hash = decode(repeat('ab', 32), 'hex') WHERE id=$1`,
			"reference":   `UPDATE wager_transactions SET reference_external_transaction_id = 'other' WHERE id=$1`,
		} {
			_, err := db.pool.Exec(ctx, sql, s.id)
			t.Run(name, func(t *testing.T) {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != sqlstateRestrictViolation {
					t.Fatalf("error = %v, want restrict violation", err)
				}
			})
		}
	})
	t.Run("cannot go back to PENDING", func(t *testing.T) {
		s := pendingRefund()
		_, err := db.pool.Exec(ctx, `UPDATE wager_transactions SET status='PENDING', next_attempt_at=NULL WHERE id=$1`, s.id)
		requirePgError(t, err, sqlstateRestrictViolation, "")
	})
	t.Run("truncate is refused", func(t *testing.T) {
		_, err := db.pool.Exec(ctx, `TRUNCATE wager_transactions CASCADE`)
		requirePgError(t, err, sqlstateRestrictViolation, "")
	})
}

// --- lacunas financeiras da revisão (M1.1) ---------------------------------

// Problema 1: o vínculo carteira <-> ledger vale nos dois sentidos. Um lançamento inserido
// sozinho, sem atualizar a carteira, não pode confirmar um histórico mais novo que a carteira.
func TestLedger_EntryWithoutWalletUpdateFailsAtCommit(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)

	err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
		s := newExternalTx(w, "BET", 100)
		s.resultBalance = ptr(int64(900))
		if err := s.insert(ctx, tx); err != nil {
			return err
		}
		return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 100, before: 1000, after: 900, version: 2}.insert(ctx, tx)
	})
	requirePgError(t, err, sqlstateCheckViolation, "wallet_matches_ledger")
	if n := db.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.id); n != 1 {
		t.Fatalf("ledger entries = %d, want 1 (opening only)", n)
	}
}

// Problemas 2 e 3: operação PROCESSED que movimenta dinheiro exige exatamente um lançamento
// cujo balance_after é o result_balance devolvido ao provedor. LOSS não movimenta.
func TestWagerTransactions_ProcessedMovementRequiresMatchingLedgerEntry(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	const constraint = "transaction_matches_ledger"

	t.Run("PROCESSED BET without ledger entry", func(t *testing.T) {
		requirePgError(t, newExternalTx(w, "BET", 100).insert(ctx, db.pool), sqlstateCheckViolation, constraint)
	})
	t.Run("PROCESSED OPENING without ledger entry", func(t *testing.T) {
		z := db.openWallet(t, 0)
		requirePgError(t, openingTx(wallet{id: z.id, playerID: z.playerID, currency: z.currency, balance: 10}).insert(ctx, db.pool),
			sqlstateCheckViolation, constraint)
	})
	t.Run("PENDING_REFERENCE resolved to PROCESSED without ledger entry", func(t *testing.T) {
		bet := newExternalTx(w, "BET", 100)
		w = db.move(t, w, bet, "DEBIT")
		soon := time.Now().Add(time.Minute)
		refund := newExternalTx(w, "REFUND", 100)
		refund.status, refund.resultBalance, refund.nextAttemptAt, refund.refExternalID = "PENDING_REFERENCE", nil, &soon, bet.externalID
		if err := refund.insert(ctx, db.pool); err != nil {
			t.Fatal(err)
		}
		_, err := db.pool.Exec(ctx, `UPDATE wager_transactions
			SET status='PROCESSED', reference_transaction_id=$2, result_balance=$3, next_attempt_at=NULL
			WHERE id=$1`, refund.id, bet.id, w.balance+100)
		requirePgError(t, err, sqlstateCheckViolation, constraint)
	})
	t.Run("result_balance differs from the entry balance_after", func(t *testing.T) {
		err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
			s := newExternalTx(w, "BET", 100)
			s.resultBalance = ptr(w.balance) // a carteira ficou em w.balance-100
			if err := s.insert(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=$2, version=$3 WHERE id=$1`, w.id, w.balance-100, w.version+1); err != nil {
				return err
			}
			return ledgerSpec{walletID: w.id, txID: s.id, direction: "DEBIT", amount: 100,
				before: w.balance, after: w.balance - 100, version: w.version + 1}.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateCheckViolation, constraint)
	})
	t.Run("LOSS is PROCESSED without ledger entry", func(t *testing.T) {
		loss := newExternalTx(w, "LOSS", 0)
		loss.resultBalance = ptr(w.balance)
		if err := loss.insert(ctx, db.pool); err != nil {
			t.Fatalf("LOSS must not need a ledger entry: %v", err)
		}
	})
}

// Problema 4: wallet_id, player_id e currency da transação concordam com a carteira.
func TestWagerTransactions_PlayerAndCurrencyMustMatchWallet(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	w := db.openWallet(t, 1000)
	const fk = "wager_tx_wallet_owner_currency_fkey"

	t.Run("another player", func(t *testing.T) {
		s := newExternalTx(w, "BET", 100)
		s.playerID = uuid.NewString()
		requirePgError(t, s.insert(ctx, db.pool), sqlstateForeignKey, fk)
	})
	t.Run("another currency", func(t *testing.T) {
		s := newExternalTx(w, "BET", 100)
		s.currency = "USD"
		requirePgError(t, s.insert(ctx, db.pool), sqlstateForeignKey, fk)
	})
	t.Run("rejection for a wallet that does not exist keeps the submitted player", func(t *testing.T) {
		s := newExternalTx(w, "BET", 100)
		s.walletID, s.playerID = "", uuid.NewString()
		s.status, s.resultBalance, s.failureCode = "REJECTED", nil, ptr("WALLET_NOT_FOUND")
		_, err := db.pool.Exec(ctx, `
			INSERT INTO wager_transactions (id, kind, status, wallet_id, player_id, amount, currency,
				provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
				failure_code, created_at, updated_at)
			VALUES ($1,$2,$3,NULL,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now(), now())`,
			s.id, s.kind, s.status, s.playerID, s.amount, s.currency,
			s.providerID, s.externalID, s.idempotencyKey, s.hash, s.roundID, s.gameID, s.failureCode)
		if err != nil {
			t.Fatalf("REJECTED without wallet must be persisted: %v", err)
		}
	})
}

// Problema 5: o lançamento da versão 1 é a OPENING, e somente ele.
func TestLedger_VersionOneEntryMustBeOpening(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	const constraint = "ledger_entry_consistency"

	t.Run("WIN as the first entry of a funded wallet", func(t *testing.T) {
		w := newWallet(500)
		err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
			if err := insertWallet(ctx, tx, w); err != nil {
				return err
			}
			win := newExternalTx(w, "WIN", 500)
			if err := win.insert(ctx, tx); err != nil {
				return err
			}
			return ledgerSpec{walletID: w.id, txID: win.id, direction: "CREDIT", amount: 500, before: 0, after: 500, version: 1}.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateCheckViolation, constraint)
	})
	t.Run("OPENING after the wallet was created with zero", func(t *testing.T) {
		z := db.openWallet(t, 0)
		err := inTx(ctx, db.pool, func(tx pgx.Tx) error {
			op := openingTx(wallet{id: z.id, playerID: z.playerID, currency: z.currency, balance: 500})
			if err := op.insert(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE wallets SET balance=500, version=2 WHERE id=$1`, z.id); err != nil {
				return err
			}
			return ledgerSpec{walletID: z.id, txID: op.id, direction: "CREDIT", amount: 500, before: 0, after: 500, version: 2}.insert(ctx, tx)
		})
		requirePgError(t, err, sqlstateCheckViolation, constraint)
	})
}
