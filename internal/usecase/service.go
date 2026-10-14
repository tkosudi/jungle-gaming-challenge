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

const (
	statusProcessed = "PROCESSED"
	statusRejected  = "REJECTED"

	directionCredit = "CREDIT"
	directionDebit  = "DEBIT"
)

// Service executa as operações financeiras. Não guarda estado: toda a coordenação
// entre instâncias acontece no PostgreSQL.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

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
			ID: newID(), WalletID: wallet.ID, TransactionID: openingID, Direction: directionCredit,
			Amount: initial, BalanceBefore: zero, BalanceAfter: initial, WalletVersion: 1, At: at,
		}
		if err := tx.InsertLedgerEntry(ctx, entry); err != nil {
			return fmt.Errorf("insert opening ledger entry: %w", err)
		}
		processed := transactionEventData{
			TransactionID: openingID.String(), WalletID: wallet.ID.String(), PlayerID: playerID.String(),
			Kind: "OPENING", Status: statusProcessed, Money: toMoneyJSON(initial), Balance: ptr(toMoneyJSON(initial)),
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

type BetResult struct {
	TransactionID uuid.UUID
	Status        string // PROCESSED ou REJECTED
	FailureCode   string // vazio quando PROCESSED
	// Balance é o saldo informado ao provedor na operação original; nulo se a rejeição
	// aconteceu sem uma carteira válida.
	Balance *domain.Money
	// Replay indica que o resultado é o de uma operação já persistida.
	Replay bool
}

// PlaceBet debita uma BET da carteira. providerID deve vir da identidade autenticada do
// chamador, nunca do conteúdo enviado pelo provedor.
//
// Fluxo, numa única transação READ COMMITTED:
//  1. se (provedor, chave) ou (provedor, ID externo) já existe: replay ou conflito, sem lock;
//  2. SELECT ... FOR UPDATE da carteira (serializa as BETs da mesma carteira);
//  3. decide: saldo suficiente => PROCESSED; senão => REJECTED, sem alterar a carteira;
//  4. grava a transação com ON CONFLICT DO NOTHING: quem perde uma corrida de duplicata não
//     escreve mais nada e devolve o resultado do vencedor;
//  5. se PROCESSED: atualiza saldo/versão e grava o lançamento; grava os eventos da outbox.
func (s *Service) PlaceBet(ctx context.Context, providerID string, bet domain.Bet) (BetResult, error) {
	if providerID == "" || strings.TrimSpace(providerID) != providerID {
		return BetResult{}, ErrInvalidProvider
	}
	hash := bet.PayloadHash(providerID)

	var result BetResult
	err := s.store.InTx(ctx, func(tx Tx) error {
		var err error
		result, err = placeBet(ctx, tx, providerID, bet, hash)
		return err
	})
	if err != nil {
		return BetResult{}, err
	}
	return result, nil
}

func placeBet(ctx context.Context, tx Tx, providerID string, bet domain.Bet, hash [32]byte) (BetResult, error) {
	if res, found, err := existingResult(ctx, tx, providerID, bet, hash); found || err != nil {
		return res, err
	}

	wallet, err := tx.LockWallet(ctx, bet.WalletID)
	if err != nil && !errors.Is(err, ErrWalletNotFound) {
		return BetResult{}, fmt.Errorf("lock wallet: %w", err)
	}

	at := time.Now().UTC()
	record := BetTransaction{ID: newID(), ProviderID: providerID, Bet: bet, PayloadHash: hash, At: at}
	var debited domain.Wallet

	switch {
	case errors.Is(err, ErrWalletNotFound) || wallet.PlayerID != bet.PlayerID:
		// Mesma resposta para carteira inexistente e de outro jogador: não revela a existência.
		record.Status, record.FailureCode = statusRejected, domain.FailureWalletNotFound
	case wallet.Balance.Currency() != bet.Money.Currency():
		record.Status, record.FailureCode = statusRejected, domain.FailureCurrencyMismatch
	default:
		record.WalletID = &wallet.ID
		var debitErr error
		debited, debitErr = wallet.Debit(bet.Money)
		switch {
		case errors.Is(debitErr, domain.ErrInsufficientFunds):
			record.Status, record.FailureCode = statusRejected, domain.FailureInsufficientFunds
			record.ResultBalance = &wallet.Balance
		case debitErr != nil:
			return BetResult{}, fmt.Errorf("debit wallet: %w", debitErr)
		default:
			record.Status = statusProcessed
			record.ResultBalance = &debited.Balance
		}
	}

	inserted, err := tx.InsertBetTransaction(ctx, record)
	if err != nil {
		return BetResult{}, fmt.Errorf("insert bet transaction: %w", err)
	}
	if !inserted {
		// Outra transação confirmou a mesma chave ou ID externo entre a consulta inicial e o
		// insert; o resultado dela é agora visível.
		res, found, err := existingResult(ctx, tx, providerID, bet, hash)
		if err != nil {
			return BetResult{}, err
		}
		if !found {
			return BetResult{}, errors.New("bet transaction conflict without a visible operation")
		}
		return res, nil
	}

	data := transactionEventData{
		TransactionID: record.ID.String(), PlayerID: bet.PlayerID.String(), ProviderID: providerID,
		ExternalTransactionID: bet.ExternalTransactionID, RoundID: bet.RoundID, GameID: bet.GameID,
		Kind: "BET", Status: record.Status, FailureCode: record.FailureCode, Money: toMoneyJSON(bet.Money),
	}
	if record.WalletID != nil {
		data.WalletID = record.WalletID.String()
	}
	if record.ResultBalance != nil {
		data.Balance = ptr(toMoneyJSON(*record.ResultBalance))
	}
	aggregateID := record.ID
	if record.WalletID != nil {
		aggregateID = *record.WalletID
	}

	if record.Status == statusRejected {
		if err := insertEvents(ctx, tx, aggregateID, record.ID, at, event{eventTransactionRejected, data}); err != nil {
			return BetResult{}, err
		}
		return resultOf(record.ID, record, false), nil
	}

	if err := tx.UpdateWallet(ctx, debited, at); err != nil {
		return BetResult{}, fmt.Errorf("update wallet: %w", err)
	}
	entry := LedgerEntry{
		ID: newID(), WalletID: wallet.ID, TransactionID: record.ID, Direction: directionDebit,
		Amount: bet.Money, BalanceBefore: wallet.Balance, BalanceAfter: debited.Balance,
		WalletVersion: debited.Version, At: at,
	}
	if err := tx.InsertLedgerEntry(ctx, entry); err != nil {
		return BetResult{}, fmt.Errorf("insert ledger entry: %w", err)
	}
	err = insertEvents(ctx, tx, wallet.ID, record.ID, at,
		event{eventTransactionProcessed, data},
		event{eventBalanceChanged, balanceChanged(entry)},
	)
	if err != nil {
		return BetResult{}, err
	}
	return resultOf(record.ID, record, false), nil
}

// existingResult distingue replay de conflito entre as operações já persistidas do provedor:
// replay exige a mesma chave E o mesmo hash canônico; qualquer outra colisão é conflito.
func existingResult(ctx context.Context, tx Tx, providerID string, bet domain.Bet, hash [32]byte) (BetResult, bool, error) {
	ops, err := tx.FindProviderOperations(ctx, providerID, bet.IdempotencyKey, bet.ExternalTransactionID)
	if err != nil {
		return BetResult{}, false, fmt.Errorf("find provider operations: %w", err)
	}
	for _, op := range ops {
		if op.Kind != "BET" || op.IdempotencyKey != bet.IdempotencyKey || !bytes.Equal(op.PayloadHash, hash[:]) {
			continue
		}
		if op.Status != statusProcessed && op.Status != statusRejected {
			return BetResult{}, false, fmt.Errorf("operation %s is in unexpected status %s", op.TransactionID, op.Status)
		}
		return BetResult{
			TransactionID: op.TransactionID, Status: op.Status, FailureCode: op.FailureCode,
			Balance: op.ResultBalance, Replay: true,
		}, true, nil
	}
	if len(ops) > 0 {
		return BetResult{}, false, ErrIdempotencyConflict
	}
	return BetResult{}, false, nil
}

func resultOf(id uuid.UUID, r BetTransaction, replay bool) BetResult {
	return BetResult{TransactionID: id, Status: r.Status, FailureCode: r.FailureCode, Balance: r.ResultBalance, Replay: replay}
}

func balanceChanged(e LedgerEntry) balanceChangedData {
	return balanceChangedData{
		WalletID: e.WalletID.String(), TransactionID: e.TransactionID.String(), Direction: e.Direction,
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
