package domain

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func testWallet(t *testing.T, balance string) Wallet {
	t.Helper()
	m, err := NewMoney(balance, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return Wallet{ID: uuid.New(), PlayerID: uuid.New(), Balance: m, Version: 3}
}

func TestWallet_Debit(t *testing.T) {
	w := testWallet(t, "100.00")
	bet, _ := NewMoney("80.00", "BRL")

	got, err := w.Debit(bet)
	if err != nil {
		t.Fatalf("Debit() error = %v", err)
	}
	if got.Balance.Amount() != "20.00" || got.Version != 4 {
		t.Errorf("Debit() = %s v%d, want 20.00 v4", got.Balance.Amount(), got.Version)
	}
	if w.Balance.Amount() != "100.00" || w.Version != 3 {
		t.Error("Debit mutated the original wallet")
	}
}

func TestWallet_DebitRejections(t *testing.T) {
	w := testWallet(t, "100.00")
	over, _ := NewMoney("100.01", "BRL")
	exact, _ := NewMoney("100.00", "BRL")
	zero, _ := NewMoney("0.00", "BRL")
	usd, _ := NewMoney("1.00", "USD")

	if _, err := w.Debit(over); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("over balance error = %v, want ErrInsufficientFunds", err)
	}
	if got, err := w.Debit(exact); err != nil || !got.Balance.IsZero() {
		t.Errorf("debit of the whole balance = %v, %v", got.Balance, err)
	}
	if _, err := w.Debit(zero); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("zero debit error = %v, want ErrNonPositiveAmount", err)
	}
	if _, err := w.Debit(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("other currency error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestWallet_Credit(t *testing.T) {
	w := testWallet(t, "100.00")
	amount, _ := NewMoney("25.50", "BRL")

	got, err := w.Credit(amount)
	if err != nil {
		t.Fatalf("Credit() error = %v", err)
	}
	if got.Balance.Amount() != "125.50" || got.Version != 4 {
		t.Errorf("Credit() = %s v%d, want 125.50 v4", got.Balance.Amount(), got.Version)
	}
	if w.Balance.Amount() != "100.00" || w.Version != 3 {
		t.Error("Credit mutated the original wallet")
	}
}

func TestWallet_CreditRejections(t *testing.T) {
	maxed, _ := MoneyFromCents(math.MaxInt64, "BRL")
	full := Wallet{ID: uuid.New(), PlayerID: uuid.New(), Balance: maxed, Version: 1}
	one, _ := NewMoney("0.01", "BRL")
	zero, _ := NewMoney("0.00", "BRL")
	usd, _ := NewMoney("1.00", "USD")
	w := testWallet(t, "100.00")

	if _, err := full.Credit(one); !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("overflow error = %v, want ErrAmountOverflow", err)
	}
	if _, err := w.Credit(zero); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("zero credit error = %v, want ErrNonPositiveAmount", err)
	}
	if _, err := w.Credit(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("other currency error = %v, want ErrCurrencyMismatch", err)
	}
}
