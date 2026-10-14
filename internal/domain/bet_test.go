package domain

import (
	"errors"
	"strings"
	"testing"
)

func validBetInput() BetInput {
	return BetInput{
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PlayerID:              "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
		WalletID:              "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Amount:                "25.00",
		Currency:              "BRL",
	}
}

func mustBet(t *testing.T, mutate func(*BetInput)) Bet {
	t.Helper()
	in := validBetInput()
	if mutate != nil {
		mutate(&in)
	}
	b, err := NewBet(in)
	if err != nil {
		t.Fatalf("NewBet() error = %v", err)
	}
	return b
}

func TestNewBet_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BetInput)
		want   error
	}{
		{"empty external id", func(in *BetInput) { in.ExternalTransactionID = "" }, ErrInvalidBet},
		{"blank external id", func(in *BetInput) { in.ExternalTransactionID = "   " }, ErrInvalidBet},
		{"empty idempotency key", func(in *BetInput) { in.IdempotencyKey = "" }, ErrInvalidBet},
		{"empty round", func(in *BetInput) { in.RoundID = "" }, ErrInvalidBet},
		{"empty game", func(in *BetInput) { in.GameID = "" }, ErrInvalidBet},
		{"too long external id", func(in *BetInput) { in.ExternalTransactionID = strings.Repeat("x", 256) }, ErrInvalidBet},
		{"bad player uuid", func(in *BetInput) { in.PlayerID = "player-1" }, ErrInvalidBet},
		{"bad wallet uuid", func(in *BetInput) { in.WalletID = "" }, ErrInvalidBet},
		{"zero amount", func(in *BetInput) { in.Amount = "0.00" }, ErrInvalidBet},
		{"negative amount", func(in *BetInput) { in.Amount = "-1.00" }, ErrNegativeAmount},
		{"malformed amount", func(in *BetInput) { in.Amount = "1e2" }, ErrInvalidAmount},
		{"unknown currency", func(in *BetInput) { in.Currency = "ZZZ" }, ErrInvalidCurrency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validBetInput()
			tc.mutate(&in)
			if _, err := NewBet(in); !errors.Is(err, tc.want) {
				t.Fatalf("NewBet() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBet_PayloadHash(t *testing.T) {
	base := mustBet(t, nil).PayloadHash("provider-a")

	t.Run("deterministic", func(t *testing.T) {
		if got := mustBet(t, nil).PayloadHash("provider-a"); got != base {
			t.Error("same input produced different hashes")
		}
	})
	t.Run("idempotency key is not part of the hash", func(t *testing.T) {
		got := mustBet(t, func(in *BetInput) { in.IdempotencyKey = "another-key" }).PayloadHash("provider-a")
		if got != base {
			t.Error("idempotency key changed the hash")
		}
	})
	t.Run("uuid spelling is normalized", func(t *testing.T) {
		got := mustBet(t, func(in *BetInput) { in.WalletID = strings.ToUpper(in.WalletID) }).PayloadHash("provider-a")
		if got != base {
			t.Error("uppercase UUID changed the hash")
		}
	})
	t.Run("every business field is part of the hash", func(t *testing.T) {
		mutations := map[string]func(*BetInput){
			"external id": func(in *BetInput) { in.ExternalTransactionID = "transaction-124" },
			"player":      func(in *BetInput) { in.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" },
			"wallet":      func(in *BetInput) { in.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" },
			"round":       func(in *BetInput) { in.RoundID = "round-988" },
			"game":        func(in *BetInput) { in.GameID = "other-game" },
			"amount":      func(in *BetInput) { in.Amount = "25.01" },
			"currency":    func(in *BetInput) { in.Currency = "USD" },
		}
		for name, mutate := range mutations {
			if got := mustBet(t, mutate).PayloadHash("provider-a"); got == base {
				t.Errorf("changing %s did not change the hash", name)
			}
		}
		if mustBet(t, nil).PayloadHash("provider-b") == base {
			t.Error("changing provider did not change the hash")
		}
	})
	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		a := mustBet(t, func(in *BetInput) { in.RoundID, in.GameID = "ab", "c" }).PayloadHash("p")
		b := mustBet(t, func(in *BetInput) { in.RoundID, in.GameID = "a", "bc" }).PayloadHash("p")
		if a == b {
			t.Error("shifting bytes between adjacent fields kept the hash")
		}
	})
}
