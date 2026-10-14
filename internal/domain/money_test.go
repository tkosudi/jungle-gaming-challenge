package domain

import (
	"errors"
	"math"
	"testing"
)

func TestNewMoney_Valid(t *testing.T) {
	tests := []struct {
		amount    string
		wantCents int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.99", 99},
		{"1.00", 100},
		{"25.00", 2500},
		{"1000.50", 100050},
		{"92233720368547758.07", math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.amount, func(t *testing.T) {
			m, err := NewMoney(tc.amount, "BRL")
			if err != nil {
				t.Fatalf("NewMoney(%q) error = %v", tc.amount, err)
			}
			if m.Cents() != tc.wantCents {
				t.Errorf("Cents() = %d, want %d", m.Cents(), tc.wantCents)
			}
			if m.Amount() != tc.amount {
				t.Errorf("Amount() = %q, want %q", m.Amount(), tc.amount)
			}
			if m.Currency() != "BRL" {
				t.Errorf("Currency() = %q", m.Currency())
			}
		})
	}
}

func TestNewMoney_InvalidAmount(t *testing.T) {
	tests := []struct {
		name   string
		amount string
		want   error
	}{
		{"empty", "", ErrInvalidAmount},
		{"integer without scale", "25", ErrInvalidAmount},
		{"one decimal", "25.0", ErrInvalidAmount},
		{"three decimals", "25.001", ErrInvalidAmount},
		{"comma separator", "25,00", ErrInvalidAmount},
		{"missing integer part", ".50", ErrInvalidAmount},
		{"missing fraction", "25.", ErrInvalidAmount},
		{"leading plus", "+25.00", ErrInvalidAmount},
		{"leading space", " 25.00", ErrInvalidAmount},
		{"trailing space", "25.00 ", ErrInvalidAmount},
		{"leading zeros", "025.00", ErrInvalidAmount},
		{"scientific notation", "1e2", ErrInvalidAmount},
		{"scientific with scale", "1.00e2", ErrInvalidAmount},
		{"NaN", "NaN", ErrInvalidAmount},
		{"Infinity", "Infinity", ErrInvalidAmount},
		{"two dots", "1.0.00", ErrInvalidAmount},
		{"thousands separator", "1_000.00", ErrInvalidAmount},
		{"non ascii digits", "٢٥.٠٠", ErrInvalidAmount},
		{"negative", "-1.00", ErrNegativeAmount},
		{"negative zero", "-0.00", ErrNegativeAmount},
		{"overflow by one cent", "92233720368547758.08", ErrAmountOverflow},
		{"far beyond int64", "999999999999999999999999.99", ErrAmountOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewMoney(tc.amount, "BRL")
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewMoney(%q) error = %v, want %v", tc.amount, err, tc.want)
			}
		})
	}
}

func TestNewMoney_InvalidCurrency(t *testing.T) {
	for _, code := range []string{"", "br", "brl", "BRLX", "ZZZ", "XXX", "ABC", "B1L", " BRL", "JPY"} {
		t.Run(code, func(t *testing.T) {
			if _, err := NewMoney("1.00", code); !errors.Is(err, ErrInvalidCurrency) {
				t.Fatalf("NewMoney(_, %q) error = %v, want ErrInvalidCurrency", code, err)
			}
		})
	}
	for _, code := range []string{"BRL", "USD", "EUR", "GBP"} {
		if _, err := NewMoney("1.00", code); err != nil {
			t.Errorf("NewMoney(_, %q) error = %v", code, err)
		}
	}
}

func TestMoney_ZeroAndPositive(t *testing.T) {
	zero, _ := NewMoney("0.00", "BRL")
	one, _ := NewMoney("0.01", "BRL")
	if !zero.IsZero() || zero.IsPositive() {
		t.Errorf("0.00: IsZero=%v IsPositive=%v", zero.IsZero(), zero.IsPositive())
	}
	if one.IsZero() || !one.IsPositive() {
		t.Errorf("0.01: IsZero=%v IsPositive=%v", one.IsZero(), one.IsPositive())
	}
	if (Money{}).IsPositive() {
		t.Error("uninitialized Money must not be positive")
	}
}

func TestMoney_AddSub(t *testing.T) {
	a, _ := NewMoney("80.00", "BRL")
	b, _ := NewMoney("20.50", "BRL")

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "100.50" {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != "59.50" {
		t.Fatalf("Sub = %v, %v", diff, err)
	}
	if _, err := b.Sub(a); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("Sub below zero error = %v, want ErrNegativeAmount", err)
	}
	max, _ := MoneyFromCents(math.MaxInt64, "BRL")
	cent, _ := NewMoney("0.01", "BRL")
	if _, err := max.Add(cent); !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("Add overflow error = %v, want ErrAmountOverflow", err)
	}
}

func TestMoney_CurrencyMismatchAndUninitialized(t *testing.T) {
	brl, _ := NewMoney("1.00", "BRL")
	usd, _ := NewMoney("1.00", "USD")
	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Compare error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Add(Money{}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("Add uninitialized error = %v, want ErrInvalidCurrency", err)
	}
}

func TestMoney_Compare(t *testing.T) {
	small, _ := NewMoney("20.00", "BRL")
	big, _ := NewMoney("80.00", "BRL")
	for _, tc := range []struct {
		a, b Money
		want int
	}{{small, big, -1}, {big, small, 1}, {big, big, 0}} {
		got, err := tc.a.Compare(tc.b)
		if err != nil || got != tc.want {
			t.Errorf("Compare(%v, %v) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
		}
	}
}

func TestMoneyFromCents(t *testing.T) {
	if _, err := MoneyFromCents(-1, "BRL"); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("negative cents error = %v", err)
	}
	if _, err := MoneyFromCents(1, "ZZZ"); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("invalid currency error = %v", err)
	}
	m, err := MoneyFromCents(97500, "BRL")
	if err != nil || m.Amount() != "975.00" {
		t.Errorf("MoneyFromCents(97500) = %v, %v", m, err)
	}
}
