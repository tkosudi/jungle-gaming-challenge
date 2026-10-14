package domain

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	ErrInsufficientFunds = errors.New("domain: insufficient funds")
	ErrNonPositiveAmount = errors.New("domain: amount must be greater than zero")
)

// Wallet é a raiz do agregado financeiro. O saldo nunca é negativo e a versão sobe em 1
// a cada mudança de saldo (o mesmo contrato imposto pelo trigger do banco).
type Wallet struct {
	ID       uuid.UUID
	PlayerID uuid.UUID
	Balance  Money
	Version  int64
}

// Debit devolve a carteira após o débito; o receptor não é alterado.
func (w Wallet) Debit(amount Money) (Wallet, error) {
	if !amount.IsPositive() {
		return Wallet{}, ErrNonPositiveAmount
	}
	cmp, err := w.Balance.Compare(amount)
	if err != nil {
		return Wallet{}, err
	}
	if cmp < 0 {
		return Wallet{}, fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, w.Balance, amount)
	}
	balance, err := w.Balance.Sub(amount)
	if err != nil {
		return Wallet{}, err
	}
	return Wallet{ID: w.ID, PlayerID: w.PlayerID, Balance: balance, Version: w.Version + 1}, nil
}

// Credit devolve a carteira após o crédito; o receptor não é alterado. Estourar o limite do
// int64 é erro (ErrAmountOverflow), nunca saldo truncado.
func (w Wallet) Credit(amount Money) (Wallet, error) {
	if !amount.IsPositive() {
		return Wallet{}, ErrNonPositiveAmount
	}
	balance, err := w.Balance.Add(amount)
	if err != nil {
		return Wallet{}, err
	}
	return Wallet{ID: w.ID, PlayerID: w.PlayerID, Balance: balance, Version: w.Version + 1}, nil
}
