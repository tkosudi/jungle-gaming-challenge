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
	// InsertOperation grava a operação já decidida; inserted=false se a chave ou o ID externo
	// do provedor já existe.
	InsertOperation(ctx context.Context, r OperationRecord) (inserted bool, err error)
	FindProviderOperations(ctx context.Context, providerID, idempotencyKey, externalID string) ([]StoredOperation, error)
	// FindReference procura a transação do provedor com esse ID externo; nil se não existe.
	// Chamado depois do lock da carteira: a leitura enxerga todo commit anterior dela.
	FindReference(ctx context.Context, providerID, externalID string) (*domain.Reference, error)

	// ClaimDuePendingReference escolhe uma pendência vencida travando a carteira dela com
	// FOR UPDATE SKIP LOCKED (nunca espera por carteira ocupada). A linha da pendência não é
	// travada: o lock da carteira basta e mantém a ordem carteira -> transação.
	ClaimDuePendingReference(ctx context.Context) (claim PendingClaim, found bool, err error)
	// LoadDuePendingReference relê a pendência em novo statement, depois do lock: found=false
	// se outra instância a resolveu ou ela não está mais vencida.
	LoadDuePendingReference(ctx context.Context, transactionID uuid.UUID) (op PendingOperation, found bool, err error)
	// ResolvePendingReference leva PENDING_REFERENCE a um estado terminal.
	ResolvePendingReference(ctx context.Context, r PendingResolution) error
	// ReschedulePendingReference registra a tentativa e agenda a próxima pelo relógio do banco.
	ReschedulePendingReference(ctx context.Context, transactionID uuid.UUID, attempts int, retryAfter time.Duration, at time.Time) error

	InsertLedgerEntry(ctx context.Context, e LedgerEntry) error
	InsertOutboxEvent(ctx context.Context, e OutboxEvent) error
}

// OperationRecord é a operação a persistir, já decidida.
type OperationRecord struct {
	ID          uuid.UUID
	WalletID    *uuid.UUID // nulo quando a carteira não existe ou não corresponde ao jogador/moeda
	ProviderID  string
	Operation   domain.Operation
	PayloadHash [32]byte
	Status      domain.Status
	FailureCode string
	// ResultBalance é o saldo informado ao provedor; é o que o replay devolve.
	ResultBalance *domain.Money
	// ReferenceTransactionID é a referência resolvida; só em PROCESSED com referência.
	ReferenceTransactionID *uuid.UUID
	// RetryAfter agenda a primeira tentativa do worker (PENDING_REFERENCE), pelo relógio do banco.
	RetryAfter time.Duration
	At         time.Time
}

// StoredOperation é uma operação do provedor já persistida.
type StoredOperation struct {
	TransactionID         uuid.UUID
	Kind                  string
	Status                domain.Status
	IdempotencyKey        string
	ExternalTransactionID string
	PayloadHash           []byte
	FailureCode           string
	ResultBalance         *domain.Money
}

// PendingClaim identifica a pendência escolhida e a carteira já travada para ela.
type PendingClaim struct {
	TransactionID uuid.UUID
	WalletID      uuid.UUID
}

// PendingOperation é uma operação PENDING_REFERENCE reidratada do banco, sem reaplicar nada.
type PendingOperation struct {
	TransactionID uuid.UUID
	ProviderID    string
	Operation     domain.Operation
	// Attempts é o número de tentativas do worker já registradas.
	Attempts int
}

// PendingResolution é o estado terminal de uma pendência.
type PendingResolution struct {
	TransactionID          uuid.UUID
	Status                 domain.Status
	FailureCode            string
	ResultBalance          *domain.Money
	ReferenceTransactionID *uuid.UUID
	At                     time.Time
}

type LedgerEntry struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     domain.Direction
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
