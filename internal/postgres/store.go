package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
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

func mapError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable {
		return fmt.Errorf("%w: %v", usecase.ErrLockTimeout, err)
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
