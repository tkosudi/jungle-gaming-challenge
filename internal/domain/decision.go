package domain

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Códigos de falha estáveis persistidos em wager_transactions.failure_code.
//
// Corrigíveis: o provedor pode enviar uma nova operação (novo ID externo) depois de corrigir
// o dado. Definitivos: o resultado depende do estado financeiro ou histórico e não muda
// reenviando a mesma operação.
const (
	// Corrigíveis.
	FailureWalletNotFound       = "WALLET_NOT_FOUND"
	FailureCurrencyMismatch     = "CURRENCY_MISMATCH"
	FailureReferenceKindInvalid = "REFERENCE_KIND_INVALID"
	FailureReferenceMismatch    = "REFERENCE_MISMATCH"
	FailureAmountMismatch       = "AMOUNT_MISMATCH"

	// Definitivos.
	FailureInsufficientFunds           = "INSUFFICIENT_FUNDS"
	FailureReversalInsufficientFunds   = "REVERSAL_INSUFFICIENT_FUNDS"
	FailureReferenceNotFound           = "REFERENCE_NOT_FOUND"
	FailureReferenceNotProcessed       = "REFERENCE_NOT_PROCESSED"
	FailureReferenceAlreadyCompensated = "REFERENCE_ALREADY_COMPENSATED"
	FailureBalanceOverflow             = "BALANCE_OVERFLOW"
)

// Direction é o sentido de uma movimentação no ledger.
type Direction string

const (
	DirectionCredit Direction = "CREDIT"
	DirectionDebit  Direction = "DEBIT"
)

// Reference é a transação referenciada como persistida, do mesmo provedor da operação.
type Reference struct {
	TransactionID uuid.UUID
	Kind          Kind
	Status        Status
	PlayerID      uuid.UUID
	WalletID      *uuid.UUID // nulo em rejeições sem carteira válida
	RoundID       string
	Money         Money
	// Compensated indica que já existe REFUND ou ROLLBACK processado sobre esta transação
	// (DEC-001: a vaga é histórica e nunca é liberada).
	Compensated bool
}

// Decision é o resultado de aplicar uma operação à carteira e à sua referência.
type Decision struct {
	Status      Status // PROCESSED, REJECTED ou PENDING_REFERENCE
	FailureCode string // só em REJECTED
	// WalletMatched: a carteira existe e é do jogador na moeda da operação; só então
	// a transação referencia a carteira (wallet_id).
	WalletMatched bool
	Direction     Direction // vazio quando não há movimentação
	// Wallet é a carteira depois da decisão; igual à de entrada sem movimentação.
	Wallet Wallet
	// ResultBalance é o saldo informado ao provedor; nulo sem carteira ou em pendência.
	ResultBalance *Money
	// ReferenceID é a referência resolvida, presente apenas em PROCESSED com referência.
	ReferenceID *uuid.UUID
}

// Decide aplica as regras financeiras de uma operação sobre a carteira (nil se inexistente)
// e a referência já resolvida (nil se ainda não chegou). É pura: quem chama persiste. O erro
// indica operação ou carteira malformadas (valor não positivo, moeda divergente), nunca uma
// rejeição de negócio.
//
// Ordem das verificações; ela define qual código o provedor recebe e impede que uma
// pendência espere por algo que nunca poderá ser válido:
//  1. carteira/jogador, moeda;
//  2. tipo da referência, concordância de jogador/carteira/moeda/rodada, valor integral;
//  3. estado da referência: pendente mantém a operação pendente; rejeitada ou falha, rejeita;
//  4. DEC-001: referência já compensada;
//  5. saldo: débito sem saldo, ou crédito que estoura o int64.
func Decide(w *Wallet, op Operation, ref *Reference) (Decision, error) {
	if op.Kind == "" || op.Money.Currency() == "" {
		return Decision{}, fmt.Errorf("%w: operation was not built by NewOperation", ErrInvalidOperation)
	}
	if w == nil || w.PlayerID != op.PlayerID {
		return reject(nil, FailureWalletNotFound), nil
	}
	if w.Balance.Currency() != op.Money.Currency() {
		return reject(nil, FailureCurrencyMismatch), nil
	}

	switch op.Kind {
	case KindLoss:
		return Decision{Status: StatusProcessed, WalletMatched: true, Wallet: *w, ResultBalance: &w.Balance}, nil
	case KindBet:
		return settle(*w, DirectionDebit, op.Money, nil, FailureInsufficientFunds)
	}
	if op.ReferenceExternalTransactionID == "" { // WIN sem referência
		return settle(*w, DirectionCredit, op.Money, nil, FailureBalanceOverflow)
	}

	if ref == nil {
		return pendingReference(*w), nil
	}
	if !referenceKindAllowed(op.Kind, ref.Kind) {
		return reject(w, FailureReferenceKindInvalid), nil
	}
	if ref.PlayerID != op.PlayerID || ref.RoundID != op.RoundID ||
		ref.Money.Currency() != op.Money.Currency() ||
		(ref.WalletID != nil && *ref.WalletID != w.ID) {
		return reject(w, FailureReferenceMismatch), nil
	}
	if op.Kind != KindWin && ref.Money != op.Money {
		return reject(w, FailureAmountMismatch), nil
	}
	switch ref.Status {
	case StatusProcessed:
	case StatusRejected, StatusFailed:
		return reject(w, FailureReferenceNotProcessed), nil
	default: // PENDING e PENDING_REFERENCE: ainda sem resultado definitivo
		return pendingReference(*w), nil
	}
	if op.Kind != KindWin && ref.Compensated {
		return reject(w, FailureReferenceAlreadyCompensated), nil
	}

	switch {
	case op.Kind == KindRollback && ref.Kind != KindBet:
		// Desfaz um crédito (WIN ou REFUND) com débito.
		return settle(*w, DirectionDebit, op.Money, &ref.TransactionID, FailureReversalInsufficientFunds)
	default:
		return settle(*w, DirectionCredit, op.Money, &ref.TransactionID, FailureBalanceOverflow)
	}
}

// referenceKindAllowed: WIN e REFUND referenciam uma BET; ROLLBACK, uma BET, WIN ou REFUND.
// ROLLBACK de ROLLBACK não existe, o que também limita a cadeia de pendências.
func referenceKindAllowed(op, ref Kind) bool {
	switch op {
	case KindWin, KindRefund:
		return ref == KindBet
	case KindRollback:
		return ref == KindBet || ref == KindWin || ref == KindRefund
	}
	return false
}

// reject recusa a operação. Com carteira válida o saldo atual é informado; sem ela, não.
func reject(w *Wallet, code string) Decision {
	d := Decision{Status: StatusRejected, FailureCode: code}
	if w != nil {
		d.WalletMatched, d.Wallet, d.ResultBalance = true, *w, &w.Balance
	}
	return d
}

func pendingReference(w Wallet) Decision {
	return Decision{Status: StatusPendingReference, WalletMatched: true, Wallet: w}
}

// settle aplica a movimentação. Débito sem saldo, ou crédito que estoura o limite, vira
// rejeição com rejectCode; qualquer outro erro é violação de invariante e é devolvido.
func settle(w Wallet, dir Direction, amount Money, ref *uuid.UUID, rejectCode string) (Decision, error) {
	var after Wallet
	var err error
	if dir == DirectionDebit {
		after, err = w.Debit(amount)
	} else {
		after, err = w.Credit(amount)
	}
	switch {
	case errors.Is(err, ErrInsufficientFunds), errors.Is(err, ErrAmountOverflow):
		return reject(&w, rejectCode), nil
	case err != nil:
		return Decision{}, fmt.Errorf("domain: settle %s of %s: %w", dir, amount, err)
	}
	return Decision{
		Status: StatusProcessed, WalletMatched: true, Direction: dir, Wallet: after,
		ResultBalance: &after.Balance, ReferenceID: ref,
	}, nil
}

// ExpiredReference é a decisão quando a referência não chegou até o fim das tentativas.
// Se a referência existe, mesmo pendente ou sem sucesso, Decide já cobriu o caso.
func ExpiredReference(w Wallet) Decision {
	return reject(&w, FailureReferenceNotFound)
}
