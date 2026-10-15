package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tkosudi/jungle-gaming-challenge/db/sqlc"
	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

const (
	sqlstateLockNotAvailable = "55P03"
	rollbackTimeout          = 5 * time.Second
)

// Store implementa usecase.Store sobre um pool pgx.
type Store struct {
	pool        *pgxpool.Pool
	lockTimeout time.Duration
}

// NewStore cria o store. lockTimeout limita a espera por locks de linha em cada transação
// (lock_timeout), para que uma carteira presa não segure conexões indefinidamente.
func NewStore(pool *pgxpool.Pool, lockTimeout time.Duration) *Store {
	return &Store{pool: pool, lockTimeout: lockTimeout}
}

// InTx executa fn numa transação READ COMMITTED. O locking pessimista por carteira (SELECT ...
// FOR UPDATE, feito por fn) é o que serializa escritores; READ COMMITTED basta porque cada
// statement vê o commit anterior da carteira depois de obter o lock.
func (s *Store) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	return s.run(ctx, pgx.ReadWrite, fn)
}

// InReadOnlyTx executa fn numa transação READ COMMITTED READ ONLY: o PostgreSQL recusa
// qualquer escrita (25006), o que garante no banco que a operação só lê.
func (s *Store) InReadOnlyTx(ctx context.Context, fn func(usecase.Tx) error) error {
	return s.run(ctx, pgx.ReadOnly, fn)
}

func (s *Store) run(ctx context.Context, mode pgx.TxAccessMode, fn func(usecase.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: mode})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", mapError(err))
	}
	// Idempotente depois do commit; no erro, desfaz mesmo com ctx cancelado.
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, strconv.FormatInt(s.lockTimeout.Milliseconds(), 10)+"ms"); err != nil {
		return fmt.Errorf("set lock timeout: %w", mapError(err))
	}
	if err := fn(&pgTx{q: sqlc.New(tx)}); err != nil {
		return mapError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// O servidor respondeu: a transação foi recusada (ex.: trigger diferido) e desfeita.
			return fmt.Errorf("commit: %w", mapError(err))
		}
		// Falha de conexão ou contexto durante o COMMIT: o resultado é desconhecido.
		return fmt.Errorf("%w: %v", usecase.ErrCommitUnknown, err)
	}
	return nil
}

// mapError classifica as falhas que o chamador pode tratar: lock_timeout e indisponibilidade do
// banco (conexão recusada ou perdida, servidor encerrando ou sem conexões livres). Nos dois casos a
// transação não foi confirmada. Falhas durante o COMMIT são tratadas à parte em InTx.
func mapError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == sqlstateLockNotAvailable:
			return fmt.Errorf("%w: %v", usecase.ErrLockTimeout, err)
		case strings.HasPrefix(pgErr.Code, "08"), pgErr.Code == "53300", strings.HasPrefix(pgErr.Code, "57P0"):
			return fmt.Errorf("%w: %v", usecase.ErrUnavailable, err)
		}
		return err
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	if errors.As(err, &connectErr) || errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", usecase.ErrUnavailable, err)
	}
	return err
}

type pgTx struct{ q *sqlc.Queries }

func (t *pgTx) InsertWalletIfAbsent(ctx context.Context, id, playerID uuid.UUID, balance domain.Money, at time.Time) (bool, error) {
	_, err := t.q.InsertWalletIfAbsent(ctx, sqlc.InsertWalletIfAbsentParams{
		ID: id, PlayerID: playerID, Currency: string(balance.Currency()), Balance: balance.Cents(), CreatedAt: at,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (t *pgTx) WalletByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency domain.Currency) (domain.Wallet, error) {
	row, err := t.q.GetWalletByPlayerAndCurrency(ctx, sqlc.GetWalletByPlayerAndCurrencyParams{PlayerID: playerID, Currency: string(currency)})
	if err != nil {
		return domain.Wallet{}, err
	}
	return toWallet(row.ID, row.PlayerID, row.Currency, row.Balance, row.Version)
}

func (t *pgTx) LockWallet(ctx context.Context, id uuid.UUID) (domain.Wallet, error) {
	row, err := t.q.LockWallet(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, usecase.ErrWalletNotFound
	}
	if err != nil {
		return domain.Wallet{}, err
	}
	return toWallet(row.ID, row.PlayerID, row.Currency, row.Balance, row.Version)
}

func (t *pgTx) UpdateWallet(ctx context.Context, w domain.Wallet, at time.Time) error {
	return t.q.UpdateWalletBalance(ctx, sqlc.UpdateWalletBalanceParams{
		ID: w.ID, Balance: w.Balance.Cents(), Version: w.Version, UpdatedAt: at,
	})
}

func (t *pgTx) InsertOpeningTransaction(ctx context.Context, id uuid.UUID, w domain.Wallet, at time.Time) error {
	return t.q.InsertOpeningTransaction(ctx, sqlc.InsertOpeningTransactionParams{
		ID: id, WalletID: &w.ID, PlayerID: w.PlayerID, Amount: w.Balance.Cents(),
		Currency: string(w.Balance.Currency()), CreatedAt: at,
	})
}

func (t *pgTx) InsertOperation(ctx context.Context, r usecase.OperationRecord) (bool, error) {
	op := r.Operation
	var resultBalance *int64
	if r.ResultBalance != nil {
		cents := r.ResultBalance.Cents()
		resultBalance = &cents
	}
	var failureCode, reference *string
	if r.FailureCode != "" {
		failureCode = &r.FailureCode
	}
	if op.ReferenceExternalTransactionID != "" {
		reference = &op.ReferenceExternalTransactionID
	}
	kind := string(op.Kind)
	_, err := t.q.InsertOperation(ctx, sqlc.InsertOperationParams{
		ID: r.ID, Kind: kind, Status: string(r.Status), WalletID: r.WalletID, PlayerID: op.PlayerID,
		Amount: op.Money.Cents(), Currency: string(op.Money.Currency()),
		ProviderID: &r.ProviderID, ExternalTransactionID: &op.ExternalTransactionID,
		IdempotencyKey: &op.IdempotencyKey, PayloadHash: r.PayloadHash[:],
		RoundID: &op.RoundID, GameID: &op.GameID,
		ReferenceExternalTransactionID: reference, ReferenceTransactionID: r.ReferenceTransactionID,
		FailureCode: failureCode, ResultBalance: resultBalance,
		Column18:  r.RetryAfter.Milliseconds(), // atraso da primeira tentativa (só PENDING_REFERENCE)
		CreatedAt: r.At,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (t *pgTx) FindReference(ctx context.Context, providerID, externalID string) (*domain.Reference, error) {
	row, err := t.q.FindReferenceTransaction(ctx, sqlc.FindReferenceTransactionParams{
		ProviderID: &providerID, ExternalTransactionID: &externalID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	amount, err := domain.MoneyFromCents(row.Amount, domain.Currency(row.Currency))
	if err != nil {
		return nil, fmt.Errorf("reference %s amount: %w", row.ID, err)
	}
	ref := &domain.Reference{
		TransactionID: row.ID, Kind: domain.Kind(row.Kind), Status: domain.Status(row.Status),
		PlayerID: row.PlayerID, WalletID: row.WalletID, Money: amount, Compensated: row.Compensated,
	}
	if row.RoundID != nil {
		ref.RoundID = *row.RoundID
	}
	return ref, nil
}

func (t *pgTx) ClaimDuePendingReference(ctx context.Context) (usecase.PendingClaim, bool, error) {
	row, err := t.q.ClaimDuePendingReference(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.PendingClaim{}, false, nil
	}
	if err != nil {
		return usecase.PendingClaim{}, false, err
	}
	if row.WalletID == nil {
		return usecase.PendingClaim{}, false, fmt.Errorf("pending transaction %s has no wallet", row.ID)
	}
	return usecase.PendingClaim{TransactionID: row.ID, WalletID: *row.WalletID}, true, nil
}

// LoadDuePendingReference reidrata a operação a partir da linha; não há criação nem validação
// de entrada nova: os campos foram validados quando a operação foi aceita.
func (t *pgTx) LoadDuePendingReference(ctx context.Context, id uuid.UUID) (usecase.PendingOperation, bool, error) {
	row, err := t.q.GetDuePendingReference(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.PendingOperation{}, false, nil
	}
	if err != nil {
		return usecase.PendingOperation{}, false, err
	}
	amount, err := domain.MoneyFromCents(row.Amount, domain.Currency(row.Currency))
	if err != nil {
		return usecase.PendingOperation{}, false, fmt.Errorf("pending transaction %s amount: %w", row.ID, err)
	}
	if row.WalletID == nil || row.ProviderID == nil || row.ExternalTransactionID == nil ||
		row.IdempotencyKey == nil || row.RoundID == nil || row.GameID == nil || row.ReferenceExternalTransactionID == nil {
		return usecase.PendingOperation{}, false, fmt.Errorf("pending transaction %s has missing fields", row.ID)
	}
	return usecase.PendingOperation{
		TransactionID: row.ID,
		ProviderID:    *row.ProviderID,
		Attempts:      int(row.ReferenceAttempts),
		Operation: domain.Operation{
			Kind: domain.Kind(row.Kind), ExternalTransactionID: *row.ExternalTransactionID,
			IdempotencyKey: *row.IdempotencyKey, PlayerID: row.PlayerID, WalletID: *row.WalletID,
			RoundID: *row.RoundID, GameID: *row.GameID, Money: amount,
			ReferenceExternalTransactionID: *row.ReferenceExternalTransactionID,
		},
	}, true, nil
}

func (t *pgTx) ResolvePendingReference(ctx context.Context, r usecase.PendingResolution) error {
	var failureCode *string
	if r.FailureCode != "" {
		failureCode = &r.FailureCode
	}
	var resultBalance *int64
	if r.ResultBalance != nil {
		cents := r.ResultBalance.Cents()
		resultBalance = &cents
	}
	n, err := t.q.ResolvePendingReference(ctx, sqlc.ResolvePendingReferenceParams{
		ID: r.TransactionID, Status: string(r.Status), FailureCode: failureCode,
		ResultBalance: resultBalance, ReferenceTransactionID: r.ReferenceTransactionID, UpdatedAt: r.At,
	})
	return requireOneRow(n, err, r.TransactionID)
}

func (t *pgTx) ReschedulePendingReference(ctx context.Context, id uuid.UUID, attempts int, retryAfter time.Duration, at time.Time) error {
	n, err := t.q.ReschedulePendingReference(ctx, sqlc.ReschedulePendingReferenceParams{
		ID: id, ReferenceAttempts: int32(attempts), Column3: retryAfter.Milliseconds(), UpdatedAt: at,
	})
	return requireOneRow(n, err, id)
}

// requireOneRow: o worker segura o lock da carteira e revalidou a pendência; atualizar zero
// linhas significa que a premissa de exclusão foi violada, e o passo deve falhar.
func requireOneRow(n int64, err error, id uuid.UUID) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("pending transaction %s was not in PENDING_REFERENCE", id)
	}
	return nil
}

func (t *pgTx) FindProviderOperations(ctx context.Context, providerID, idempotencyKey, externalID string) ([]usecase.StoredOperation, error) {
	rows, err := t.q.FindProviderOperations(ctx, sqlc.FindProviderOperationsParams{
		ProviderID: &providerID, IdempotencyKey: &idempotencyKey, ExternalTransactionID: &externalID,
	})
	if err != nil {
		return nil, err
	}
	ops := make([]usecase.StoredOperation, 0, len(rows))
	for _, row := range rows {
		op := usecase.StoredOperation{
			TransactionID: row.ID, Kind: row.Kind, Status: domain.Status(row.Status), PayloadHash: row.PayloadHash,
		}
		if row.IdempotencyKey != nil {
			op.IdempotencyKey = *row.IdempotencyKey
		}
		if row.ExternalTransactionID != nil {
			op.ExternalTransactionID = *row.ExternalTransactionID
		}
		if row.FailureCode != nil {
			op.FailureCode = *row.FailureCode
		}
		if row.ResultBalance != nil {
			balance, err := domain.MoneyFromCents(*row.ResultBalance, domain.Currency(row.Currency))
			if err != nil {
				return nil, fmt.Errorf("transaction %s result balance: %w", row.ID, err)
			}
			op.ResultBalance = &balance
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (t *pgTx) InsertLedgerEntry(ctx context.Context, e usecase.LedgerEntry) error {
	return t.q.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		ID: e.ID, WalletID: e.WalletID, TransactionID: e.TransactionID, Direction: string(e.Direction),
		Amount: e.Amount.Cents(), BalanceBefore: e.BalanceBefore.Cents(), BalanceAfter: e.BalanceAfter.Cents(),
		WalletVersion: e.WalletVersion, CreatedAt: e.At,
	})
}

func (t *pgTx) InsertOutboxEvent(ctx context.Context, e usecase.OutboxEvent) error {
	return t.q.InsertOutboxEvent(ctx, sqlc.InsertOutboxEventParams{
		EventID: e.EventID, EventType: e.EventType, SchemaVersion: int32(e.SchemaVersion),
		AggregateID: e.AggregateID, MessageGroupID: e.MessageGroupID, TransactionID: e.TransactionID,
		Payload: e.Payload, OccurredAt: e.OccurredAt,
	})
}

func toWallet(id, playerID uuid.UUID, currency string, balance, version int64) (domain.Wallet, error) {
	money, err := domain.MoneyFromCents(balance, domain.Currency(currency))
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("wallet %s balance: %w", id, err)
	}
	return domain.Wallet{ID: id, PlayerID: playerID, Balance: money, Version: version}, nil
}

func (t *pgTx) GetWallet(ctx context.Context, id uuid.UUID) (domain.Wallet, error) {
	row, err := t.q.GetWallet(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, usecase.ErrWalletNotFound
	}
	if err != nil {
		return domain.Wallet{}, err
	}
	return toWallet(row.ID, row.PlayerID, row.Currency, row.Balance, row.Version)
}

func (t *pgTx) ListLedgerEntries(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]usecase.LedgerEntry, error) {
	w, err := t.GetWallet(ctx, walletID)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.ListLedgerEntries(ctx, sqlc.ListLedgerEntriesParams{WalletID: walletID, WalletVersion: afterVersion, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	currency := w.Balance.Currency()
	entries := make([]usecase.LedgerEntry, 0, len(rows))
	for _, r := range rows {
		var amounts [3]domain.Money
		for i, cents := range []int64{r.Amount, r.BalanceBefore, r.BalanceAfter} {
			if amounts[i], err = domain.MoneyFromCents(cents, currency); err != nil {
				return nil, fmt.Errorf("ledger entry %s: %w", r.ID, err)
			}
		}
		entries = append(entries, usecase.LedgerEntry{
			ID: r.ID, WalletID: walletID, TransactionID: r.TransactionID, Direction: domain.Direction(r.Direction),
			Amount: amounts[0], BalanceBefore: amounts[1], BalanceAfter: amounts[2], WalletVersion: r.WalletVersion, At: r.CreatedAt,
		})
	}
	return entries, nil
}

func (t *pgTx) GetProviderTransaction(ctx context.Context, providerID string, id uuid.UUID) (usecase.Transaction, error) {
	row, err := t.q.GetProviderTransaction(ctx, sqlc.GetProviderTransactionParams{ID: id, ProviderID: &providerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.Transaction{}, usecase.ErrTransactionNotFound
	}
	if err != nil {
		return usecase.Transaction{}, err
	}
	return toTransaction(row)
}

func (t *pgTx) GetProviderTransactionByExternalID(ctx context.Context, providerID, externalID string) (usecase.Transaction, error) {
	row, err := t.q.GetProviderTransactionByExternalID(ctx, sqlc.GetProviderTransactionByExternalIDParams{
		ProviderID: &providerID, ExternalTransactionID: &externalID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.Transaction{}, usecase.ErrTransactionNotFound
	}
	if err != nil {
		return usecase.Transaction{}, err
	}
	// As duas consultas selecionam as mesmas colunas; as structs geradas são conversíveis.
	return toTransaction(sqlc.GetProviderTransactionRow(row))
}

func toTransaction(r sqlc.GetProviderTransactionRow) (usecase.Transaction, error) {
	money, err := domain.MoneyFromCents(r.Amount, domain.Currency(r.Currency))
	if err != nil {
		return usecase.Transaction{}, fmt.Errorf("transaction %s amount: %w", r.ID, err)
	}
	t := usecase.Transaction{
		ID: r.ID, Kind: r.Kind, Status: domain.Status(r.Status), PlayerID: r.PlayerID, WalletID: r.WalletID,
		Money: money, ReferenceTransactionID: r.ReferenceTransactionID, ReferenceAttempts: int(r.ReferenceAttempts),
		NextAttemptAt: r.NextAttemptAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	for dst, src := range map[*string]*string{
		&t.ProviderID: r.ProviderID, &t.ExternalTransactionID: r.ExternalTransactionID, &t.RoundID: r.RoundID,
		&t.GameID: r.GameID, &t.ReferenceExternalTransactionID: r.ReferenceExternalTransactionID, &t.FailureCode: r.FailureCode,
	} {
		if src != nil {
			*dst = *src
		}
	}
	if r.ResultBalance != nil {
		balance, err := domain.MoneyFromCents(*r.ResultBalance, domain.Currency(r.Currency))
		if err != nil {
			return usecase.Transaction{}, fmt.Errorf("transaction %s result balance: %w", r.ID, err)
		}
		t.ResultBalance = &balance
	}
	return t, nil
}

func (t *pgTx) ReconciliationSnapshot(ctx context.Context, walletID uuid.UUID) (usecase.ReconciliationSnapshot, error) {
	row, err := t.q.GetReconciliationSnapshot(ctx, walletID)
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.ReconciliationSnapshot{}, usecase.ErrWalletNotFound
	}
	if err != nil {
		return usecase.ReconciliationSnapshot{}, err
	}
	currency := domain.Currency(row.Currency)
	stored, err := domain.MoneyFromCents(row.Balance, currency)
	if err != nil {
		return usecase.ReconciliationSnapshot{}, fmt.Errorf("wallet %s reconciliation: %w", walletID, err)
	}
	// O saldo reconstruído pode ser negativo num ledger corrompido; é justamente o que a
	// reconciliação deve reportar, não rejeitar.
	ledger, err := domain.MoneyFromSignedCents(row.LedgerBalance, currency)
	if err != nil {
		return usecase.ReconciliationSnapshot{}, fmt.Errorf("wallet %s reconciliation: %w", walletID, err)
	}
	return usecase.ReconciliationSnapshot{StoredBalance: stored, LedgerBalance: ledger, Entries: row.Entries}, nil
}

func (t *pgTx) FindInboxMessage(ctx context.Context, consumerName, messageID string) (usecase.StoredInboxMessage, bool, error) {
	row, err := t.q.FindInboxMessage(ctx, sqlc.FindInboxMessageParams{ConsumerName: consumerName, MessageID: messageID})
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.StoredInboxMessage{}, false, nil
	}
	if err != nil {
		return usecase.StoredInboxMessage{}, false, err
	}
	return usecase.StoredInboxMessage{PayloadHash: row.PayloadHash, TransactionID: row.TransactionID, Outcome: domain.Status(row.Outcome)}, true, nil
}

func (t *pgTx) InsertInboxMessage(ctx context.Context, r usecase.InboxRecord) (bool, error) {
	_, err := t.q.InsertInboxMessage(ctx, sqlc.InsertInboxMessageParams{
		ConsumerName: r.ConsumerName, MessageID: r.MessageID, PayloadHash: r.PayloadHash[:], TransactionID: r.TransactionID,
		Outcome: string(r.Outcome), ReceivedAt: r.ReceivedAt, CompletedAt: r.CompletedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
