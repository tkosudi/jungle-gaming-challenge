// Package usecase coordena as operações financeiras e seus limites transacionais.
// Não conhece pgx: a persistência chega por Store e Tx, implementadas em internal/postgres.
package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

var (
	// ErrIdempotencyConflict: chave ou ID externo já usados por outra operação do provedor.
	ErrIdempotencyConflict = errors.New("usecase: idempotency conflict")
	ErrInvalidProvider     = errors.New("usecase: invalid provider")
	// ErrWalletNotFound é devolvido por Tx.LockWallet quando a carteira não existe.
	ErrWalletNotFound = errors.New("usecase: wallet not found")
	// ErrLockTimeout: a carteira ficou presa por outra transação além do limite; nada foi gravado
	// e a mesma requisição pode ser reenviada com segurança.
	ErrLockTimeout = errors.New("usecase: wallet lock timeout")
	// ErrCommitUnknown: a conexão falhou durante o COMMIT e não se sabe se a transação foi
	// confirmada. Reenviar a mesma requisição (mesma chave) é seguro: se o commit ocorreu, a
	// resposta é o replay do resultado original; se não, a operação é aplicada uma única vez.
	ErrCommitUnknown = errors.New("usecase: commit outcome unknown")
)

// Store abre uma transação PostgreSQL READ COMMITTED, executa fn e confirma. Qualquer erro de
// fn desfaz tudo; fn executa no máximo uma vez (sem retry implícito).
type Store interface {
	InTx(ctx context.Context, fn func(Tx) error) error
}

// Tx é o conjunto de operações de persistência de uma transação financeira.
type Tx interface {
	// InsertWalletIfAbsent cria a carteira (versão 1); created=false se (jogador, moeda) já existe.
	InsertWalletIfAbsent(ctx context.Context, id, playerID uuid.UUID, balance domain.Money, at time.Time) (created bool, err error)
	WalletByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency domain.Currency) (domain.Wallet, error)
	// LockWallet lê a carteira com SELECT ... FOR UPDATE; ErrWalletNotFound se não existir.
	LockWallet(ctx context.Context, id uuid.UUID) (domain.Wallet, error)
	UpdateWallet(ctx context.Context, w domain.Wallet, at time.Time) error

	InsertOpeningTransaction(ctx context.Context, id uuid.UUID, w domain.Wallet, at time.Time) error
	// InsertBetTransaction grava a BET; inserted=false se a chave ou o ID externo do provedor já existe.
	InsertBetTransaction(ctx context.Context, t BetTransaction) (inserted bool, err error)
	FindProviderOperations(ctx context.Context, providerID, idempotencyKey, externalID string) ([]StoredOperation, error)

	InsertLedgerEntry(ctx context.Context, e LedgerEntry) error
	InsertOutboxEvent(ctx context.Context, e OutboxEvent) error
}

// BetTransaction é a BET a persistir, já decidida: PROCESSED ou REJECTED.
type BetTransaction struct {
	ID          uuid.UUID
	WalletID    *uuid.UUID // nulo quando a carteira não existe ou não corresponde ao jogador/moeda
	ProviderID  string
	Bet         domain.Bet
	PayloadHash [32]byte
	Status      string
	FailureCode string
	// ResultBalance é o saldo informado ao provedor; é o que o replay devolve.
	ResultBalance *domain.Money
	At            time.Time
}

// StoredOperation é uma operação do provedor já persistida.
type StoredOperation struct {
	TransactionID         uuid.UUID
	Kind                  string
	Status                string
	IdempotencyKey        string
	ExternalTransactionID string
	PayloadHash           []byte
	FailureCode           string
	ResultBalance         *domain.Money
}

type LedgerEntry struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     string // DEBIT ou CREDIT
	Amount        domain.Money
	BalanceBefore domain.Money
	BalanceAfter  domain.Money
	WalletVersion int64
	At            time.Time
}

type OutboxEvent struct {
	EventID        uuid.UUID
	EventType      string
	SchemaVersion  int
	AggregateID    uuid.UUID
	MessageGroupID string
	TransactionID  uuid.UUID
	Payload        []byte
	OccurredAt     time.Time
}
