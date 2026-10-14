package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

// Service executa as operações financeiras. Não guarda estado de negócio: toda a coordenação
// entre instâncias acontece no PostgreSQL.
type Service struct {
	store  Store
	policy ReferencePolicy
}

func NewService(store Store, policy ReferencePolicy) (*Service, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &Service{store: store, policy: policy}, nil
}

// newID gera UUIDv7; só falha se o gerador de entropia do sistema falhar, situação fatal.
func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

type OpenWalletResult struct {
	Wallet domain.Wallet
	// Created é falso quando já existia carteira para (jogador, moeda); nesse caso nada foi alterado.
	Created bool
}

// OpenWallet cria a carteira do jogador na moeda de initial. Com saldo positivo grava, na
// mesma transação, a operação OPENING, o lançamento de crédito e os eventos. Com saldo zero
// grava só a carteira. A unicidade (jogador, moeda) do banco serializa chamadas simultâneas:
// apenas uma cria e concede o crédito inicial.
func (s *Service) OpenWallet(ctx context.Context, playerID uuid.UUID, initial domain.Money) (OpenWalletResult, error) {
	if initial.Currency() == "" {
		return OpenWalletResult{}, domain.ErrInvalidCurrency
	}
	// Money admite negativos em cálculos internos; o saldo de uma carteira, nunca.
	if initial.IsNegative() {
		return OpenWalletResult{}, domain.ErrNegativeAmount
	}
	var result OpenWalletResult
	err := s.store.InTx(ctx, func(tx Tx) error {
		at := time.Now().UTC()
		wallet := domain.Wallet{ID: newID(), PlayerID: playerID, Balance: initial, Version: 1}

		created, err := tx.InsertWalletIfAbsent(ctx, wallet.ID, playerID, initial, at)
		if err != nil {
			return fmt.Errorf("insert wallet: %w", err)
		}
		if !created {
			existing, err := tx.WalletByPlayerAndCurrency(ctx, playerID, initial.Currency())
			if err != nil {
				return fmt.Errorf("load existing wallet: %w", err)
			}
			result = OpenWalletResult{Wallet: existing}
			return nil
		}
		result = OpenWalletResult{Wallet: wallet, Created: true}
		if initial.IsZero() {
			return nil
		}

		openingID := newID()
		if err := tx.InsertOpeningTransaction(ctx, openingID, wallet, at); err != nil {
			return fmt.Errorf("insert opening transaction: %w", err)
		}
		zero, err := domain.MoneyFromCents(0, initial.Currency())
		if err != nil {
			return err
		}
		entry := LedgerEntry{
			ID: newID(), WalletID: wallet.ID, TransactionID: openingID, Direction: domain.DirectionCredit,
			Amount: initial, BalanceBefore: zero, BalanceAfter: initial, WalletVersion: 1, At: at,
		}
		if err := tx.InsertLedgerEntry(ctx, entry); err != nil {
			return fmt.Errorf("insert opening ledger entry: %w", err)
		}
		processed := transactionEventData{
			TransactionID: openingID.String(), WalletID: wallet.ID.String(), PlayerID: playerID.String(),
			Kind: "OPENING", Status: string(domain.StatusProcessed), Money: toMoneyJSON(initial), Balance: ptr(toMoneyJSON(initial)),
		}
		return insertEvents(ctx, tx, wallet.ID, openingID, at,
			event{eventTransactionProcessed, processed},
			event{eventBalanceChanged, balanceChanged(entry)},
		)
	})
	if err != nil {
		return OpenWalletResult{}, err
	}
	return result, nil
}

// Result é a resposta ao provedor para uma operação.
type Result struct {
	TransactionID uuid.UUID
	// Status é PROCESSED, REJECTED ou PENDING_REFERENCE (a referência ainda não chegou).
	Status      domain.Status
	FailureCode string // vazio fora de REJECTED
	// Balance é o saldo informado na operação original; nulo se a rejeição aconteceu sem uma
	// carteira válida ou se a operação ainda está pendente.
	Balance *domain.Money
	// Replay indica que o resultado é o de uma operação já persistida.
	Replay bool
}

// Process aplica uma operação externa (BET, WIN, LOSS, REFUND ou ROLLBACK). providerID deve
// vir da identidade autenticada do chamador, nunca do conteúdo enviado pelo provedor.
//
// Fluxo, numa única transação READ COMMITTED:
//  1. se (provedor, chave) ou (provedor, ID externo) já existe: replay ou conflito, sem lock;
//  2. SELECT ... FOR UPDATE da carteira, o único lock da operação: serializa tudo que muda
//     a carteira e a compensação das transações dela (a referência é da mesma carteira);
//  3. com o lock, lê a referência e decide (domain.Decide);
//  4. grava a transação com ON CONFLICT DO NOTHING: quem perde uma corrida de duplicata não
//     escreve mais nada e devolve o resultado do vencedor;
//  5. saldo/versão, lançamento e eventos da outbox, no mesmo commit.
//
// Rejeições de negócio e pendências são persistidas; o resultado devolvido é o persistido.
func (s *Service) Process(ctx context.Context, providerID string, op domain.Operation) (Result, error) {
	if providerID == "" || strings.TrimSpace(providerID) != providerID {
		return Result{}, ErrInvalidProvider
	}
	hash := op.PayloadHash(providerID)

	var result Result
	err := s.store.InTx(ctx, func(tx Tx) error {
		var err error
		result, err = s.process(ctx, tx, providerID, op, hash)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) process(ctx context.Context, tx Tx, providerID string, op domain.Operation, hash [32]byte) (Result, error) {
	if res, found, err := existingResult(ctx, tx, providerID, op, hash); found || err != nil {
		return res, err
	}

	var wallet *domain.Wallet
	locked, err := tx.LockWallet(ctx, op.WalletID)
	switch {
	case errors.Is(err, ErrWalletNotFound):
	case err != nil:
		return Result{}, fmt.Errorf("lock wallet: %w", err)
	default:
		wallet = &locked
	}

	// Sem carteira válida Decide não consulta a referência; com ela, a leitura vem depois do lock.
	var ref *domain.Reference
	if wallet != nil && op.ReferenceExternalTransactionID != "" {
		if ref, err = tx.FindReference(ctx, providerID, op.ReferenceExternalTransactionID); err != nil {
			return Result{}, fmt.Errorf("find reference: %w", err)
		}
	}
	decision, err := domain.Decide(wallet, op, ref)
	if err != nil {
		return Result{}, err
	}

	at := time.Now().UTC()
	record := OperationRecord{
		ID: newID(), ProviderID: providerID, Operation: op, PayloadHash: hash,
		Status: decision.Status, FailureCode: decision.FailureCode,
		ResultBalance: decision.ResultBalance, ReferenceTransactionID: decision.ReferenceID, At: at,
	}
	if decision.WalletMatched {
		record.WalletID = &wallet.ID
	}
	if decision.Status == domain.StatusPendingReference {
		record.RetryAfter = s.policy.backoff(0)
	}

	inserted, err := tx.InsertOperation(ctx, record)
	if err != nil {
		return Result{}, fmt.Errorf("insert %s transaction: %w", op.Kind, err)
	}
	if !inserted {
		// Outra transação confirmou a mesma chave ou ID externo entre a consulta inicial e o
		// insert; o resultado dela é agora visível.
		res, found, err := existingResult(ctx, tx, providerID, op, hash)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return Result{}, errors.New("operation insert conflicted without a visible operation")
		}
		return res, nil
	}

	var before domain.Wallet
	if wallet != nil {
		before = *wallet
	}
	t := storedTransaction{id: record.ID, providerID: providerID, op: op, walletID: record.WalletID}
	if err := writeEffects(ctx, tx, t, before, decision, at); err != nil {
		return Result{}, err
	}
	return Result{
		TransactionID: record.ID, Status: decision.Status, FailureCode: decision.FailureCode,
		Balance: decision.ResultBalance,
	}, nil
}

// storedTransaction reúne o que os eventos precisam saber da transação já persistida.
type storedTransaction struct {
	id         uuid.UUID
	providerID string
	op         domain.Operation
	walletID   *uuid.UUID // nulo sem carteira válida
}

// writeEffects grava, na transação corrente, o que decorre de uma decisão: saldo e lançamento
// (somente com movimentação) e os eventos. before é a carteira travada antes da decisão.
//
// PROCESSED: WagerTransactionProcessed (também LOSS, sem movimentação) e, havendo movimento,
// WalletBalanceChanged. REJECTED: WagerTransactionRejected. PENDING_REFERENCE:
// WagerTransactionPendingReference; nada de saldo nem ledger.
func writeEffects(ctx context.Context, tx Tx, t storedTransaction, before domain.Wallet, d domain.Decision, at time.Time) error {
	data := transactionEventData{
		TransactionID: t.id.String(), PlayerID: t.op.PlayerID.String(), ProviderID: t.providerID,
		ExternalTransactionID: t.op.ExternalTransactionID, RoundID: t.op.RoundID, GameID: t.op.GameID,
		Kind: string(t.op.Kind), Status: string(d.Status), FailureCode: d.FailureCode,
		Money: toMoneyJSON(t.op.Money), ReferenceExternalTransactionID: t.op.ReferenceExternalTransactionID,
	}
	aggregateID := t.id
	if t.walletID != nil {
		data.WalletID = t.walletID.String()
		aggregateID = *t.walletID
	}
	if d.ResultBalance != nil {
		data.Balance = ptr(toMoneyJSON(*d.ResultBalance))
	}

	switch d.Status {
	case domain.StatusRejected:
		return insertEvents(ctx, tx, aggregateID, t.id, at, event{eventTransactionRejected, data})
	case domain.StatusPendingReference:
		return insertEvents(ctx, tx, aggregateID, t.id, at, event{eventPendingReference, data})
	case domain.StatusProcessed:
	default:
		return fmt.Errorf("decision with unexpected status %s", d.Status)
	}

	if d.Direction == "" { // LOSS
		return insertEvents(ctx, tx, aggregateID, t.id, at, event{eventTransactionProcessed, data})
	}
	if err := tx.UpdateWallet(ctx, d.Wallet, at); err != nil {
		return fmt.Errorf("update wallet: %w", err)
	}
	entry := LedgerEntry{
		ID: newID(), WalletID: before.ID, TransactionID: t.id, Direction: d.Direction,
		Amount: t.op.Money, BalanceBefore: before.Balance, BalanceAfter: d.Wallet.Balance,
		WalletVersion: d.Wallet.Version, At: at,
	}
	if err := tx.InsertLedgerEntry(ctx, entry); err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}
	return insertEvents(ctx, tx, aggregateID, t.id, at,
		event{eventTransactionProcessed, data},
		event{eventBalanceChanged, balanceChanged(entry)},
	)
}

// existingResult distingue replay de conflito entre as operações já persistidas do provedor:
// replay exige a mesma chave E o mesmo hash canônico (que inclui o tipo e a referência);
// qualquer outra colisão é conflito. O resultado é o persistido agora: uma pendência replayada
// depois de resolvida devolve o estado terminal, com o saldo do processamento original.
func existingResult(ctx context.Context, tx Tx, providerID string, op domain.Operation, hash [32]byte) (Result, bool, error) {
	ops, err := tx.FindProviderOperations(ctx, providerID, op.IdempotencyKey, op.ExternalTransactionID)
	if err != nil {
		return Result{}, false, fmt.Errorf("find provider operations: %w", err)
	}
	for _, stored := range ops {
		if stored.IdempotencyKey != op.IdempotencyKey || !bytes.Equal(stored.PayloadHash, hash[:]) {
			continue
		}
		switch stored.Status {
		case domain.StatusProcessed, domain.StatusRejected, domain.StatusFailed, domain.StatusPendingReference:
		default:
			return Result{}, false, fmt.Errorf("operation %s is in unexpected status %s", stored.TransactionID, stored.Status)
		}
		return Result{
			TransactionID: stored.TransactionID, Status: stored.Status, FailureCode: stored.FailureCode,
			Balance: stored.ResultBalance, Replay: true,
		}, true, nil
	}
	if len(ops) > 0 {
		return Result{}, false, ErrIdempotencyConflict
	}
	return Result{}, false, nil
}

func balanceChanged(e LedgerEntry) balanceChangedData {
	return balanceChangedData{
		WalletID: e.WalletID.String(), TransactionID: e.TransactionID.String(), Direction: string(e.Direction),
		Money: toMoneyJSON(e.Amount), BalanceBefore: toMoneyJSON(e.BalanceBefore),
		BalanceAfter: toMoneyJSON(e.BalanceAfter), WalletVersion: e.WalletVersion,
	}
}

type event struct {
	eventType string
	data      any
}

func insertEvents(ctx context.Context, tx Tx, aggregateID, transactionID uuid.UUID, at time.Time, events ...event) error {
	for _, e := range events {
		out, err := newOutboxEvent(e.eventType, aggregateID, transactionID, at, e.data)
		if err != nil {
			return err
		}
		if err := tx.InsertOutboxEvent(ctx, out); err != nil {
			return fmt.Errorf("insert %s event: %w", e.eventType, err)
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
