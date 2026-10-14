package domain

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func validOperationInput() OperationInput {
	return OperationInput{
		Kind:                  "BET",
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

func mustOperation(t *testing.T, mutate func(*OperationInput)) Operation {
	t.Helper()
	in := validOperationInput()
	if mutate != nil {
		mutate(&in)
	}
	b, err := NewOperation(in)
	if err != nil {
		t.Fatalf("NewOperation() error = %v", err)
	}
	return b
}

func TestNewOperation_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OperationInput)
		want   error
	}{
		{"empty external id", func(in *OperationInput) { in.ExternalTransactionID = "" }, ErrInvalidOperation},
		{"blank external id", func(in *OperationInput) { in.ExternalTransactionID = "   " }, ErrInvalidOperation},
		{"empty idempotency key", func(in *OperationInput) { in.IdempotencyKey = "" }, ErrInvalidOperation},
		{"empty round", func(in *OperationInput) { in.RoundID = "" }, ErrInvalidOperation},
		{"empty game", func(in *OperationInput) { in.GameID = "" }, ErrInvalidOperation},
		{"too long external id", func(in *OperationInput) { in.ExternalTransactionID = strings.Repeat("x", 256) }, ErrInvalidOperation},
		{"bad player uuid", func(in *OperationInput) { in.PlayerID = "player-1" }, ErrInvalidOperation},
		{"bad wallet uuid", func(in *OperationInput) { in.WalletID = "" }, ErrInvalidOperation},
		{"zero amount", func(in *OperationInput) { in.Amount = "0.00" }, ErrInvalidOperation},
		{"negative amount", func(in *OperationInput) { in.Amount = "-1.00" }, ErrNegativeAmount},
		{"malformed amount", func(in *OperationInput) { in.Amount = "1e2" }, ErrInvalidAmount},
		{"unknown currency", func(in *OperationInput) { in.Currency = "ZZZ" }, ErrInvalidCurrency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validOperationInput()
			tc.mutate(&in)
			if _, err := NewOperation(in); !errors.Is(err, tc.want) {
				t.Fatalf("NewOperation() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOperation_PayloadHash(t *testing.T) {
	base := mustOperation(t, nil).PayloadHash("provider-a")

	t.Run("deterministic", func(t *testing.T) {
		if got := mustOperation(t, nil).PayloadHash("provider-a"); got != base {
			t.Error("same input produced different hashes")
		}
	})
	t.Run("idempotency key is not part of the hash", func(t *testing.T) {
		got := mustOperation(t, func(in *OperationInput) { in.IdempotencyKey = "another-key" }).PayloadHash("provider-a")
		if got != base {
			t.Error("idempotency key changed the hash")
		}
	})
	t.Run("uuid spelling is normalized", func(t *testing.T) {
		got := mustOperation(t, func(in *OperationInput) { in.WalletID = strings.ToUpper(in.WalletID) }).PayloadHash("provider-a")
		if got != base {
			t.Error("uppercase UUID changed the hash")
		}
	})
	t.Run("every business field is part of the hash", func(t *testing.T) {
		mutations := map[string]func(*OperationInput){
			"external id": func(in *OperationInput) { in.ExternalTransactionID = "transaction-124" },
			"player":      func(in *OperationInput) { in.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" },
			"wallet":      func(in *OperationInput) { in.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" },
			"round":       func(in *OperationInput) { in.RoundID = "round-988" },
			"game":        func(in *OperationInput) { in.GameID = "other-game" },
			"amount":      func(in *OperationInput) { in.Amount = "25.01" },
			"currency":    func(in *OperationInput) { in.Currency = "USD" },
		}
		for name, mutate := range mutations {
			if got := mustOperation(t, mutate).PayloadHash("provider-a"); got == base {
				t.Errorf("changing %s did not change the hash", name)
			}
		}
		if mustOperation(t, nil).PayloadHash("provider-b") == base {
			t.Error("changing provider did not change the hash")
		}
	})
	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		a := mustOperation(t, func(in *OperationInput) { in.RoundID, in.GameID = "ab", "c" }).PayloadHash("p")
		b := mustOperation(t, func(in *OperationInput) { in.RoundID, in.GameID = "a", "bc" }).PayloadHash("p")
		if a == b {
			t.Error("shifting bytes between adjacent fields kept the hash")
		}
	})
}

// O vetor foi calculado fora do código (sha256sum sobre este JSON), para que o teste não
// dependa da implementação que verifica.
func TestOperation_CanonicalJSONAndKnownHash(t *testing.T) {
	const wantJSON = `{"amount":"25.00","currency":"BRL","externalTransactionId":"transaction-123",` +
		`"gameId":"fortune-chimp","kind":"BET","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	const wantHash = "4226c493492b649bb0e8a938aff92fe5fc6c4d2d1a6eb13a1846d4a4d43e4f49"

	bet := mustOperation(t, nil)
	if got := string(bet.CanonicalJSON("provider-a")); got != wantJSON {
		t.Errorf("CanonicalJSON() =\n%s\nwant\n%s", got, wantJSON)
	}
	hash := bet.PayloadHash("provider-a")
	if got := hex.EncodeToString(hash[:]); got != wantHash {
		t.Errorf("PayloadHash() = %s, want %s", got, wantHash)
	}
}

func TestNewOperation_RejectsInvalidUTF8(t *testing.T) {
	in := validOperationInput()
	in.RoundID = "round-\xff"
	if _, err := NewOperation(in); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("NewOperation() error = %v, want ErrInvalidOperation", err)
	}
}

func TestNewOperation_KindPolicy(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		amount    string
		reference string
		want      error // nil: válida
	}{
		{"BET", "BET", "25.00", "", nil},
		{"BET with reference", "BET", "25.00", "tx-0", ErrInvalidOperation},
		{"BET zero", "BET", "0.00", "", ErrInvalidOperation},
		{"WIN without reference", "WIN", "25.00", "", nil},
		{"WIN with reference", "WIN", "25.00", "tx-0", nil},
		{"WIN zero", "WIN", "0.00", "", ErrInvalidOperation},
		{"LOSS zero", "LOSS", "0.00", "", nil},
		{"LOSS positive", "LOSS", "1.00", "", ErrInvalidOperation},
		{"LOSS with reference", "LOSS", "0.00", "tx-0", ErrInvalidOperation},
		{"REFUND with reference", "REFUND", "25.00", "tx-0", nil},
		{"REFUND without reference", "REFUND", "25.00", "", ErrInvalidOperation},
		{"REFUND zero", "REFUND", "0.00", "tx-0", ErrInvalidOperation},
		{"ROLLBACK with reference", "ROLLBACK", "25.00", "tx-0", nil},
		{"ROLLBACK without reference", "ROLLBACK", "25.00", "", ErrInvalidOperation},
		{"OPENING is internal only", "OPENING", "25.00", "", ErrInvalidOperation},
		{"empty kind", "", "25.00", "", ErrInvalidOperation},
		{"lowercase kind", "bet", "25.00", "", ErrInvalidOperation},
		{"reference to itself", "ROLLBACK", "25.00", "transaction-123", ErrInvalidOperation},
		{"reference with surrounding spaces", "ROLLBACK", "25.00", " tx-0", ErrInvalidOperation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validOperationInput()
			in.Kind, in.Amount, in.ReferenceExternalTransactionID = tc.kind, tc.amount, tc.reference
			_, err := NewOperation(in)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("NewOperation() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOperation_CanonicalJSONWithReference(t *testing.T) {
	op := mustOperation(t, func(in *OperationInput) {
		in.Kind, in.ReferenceExternalTransactionID = "ROLLBACK", "transaction-100"
	})
	const want = `{"amount":"25.00","currency":"BRL","externalTransactionId":"transaction-123",` +
		`"gameId":"fortune-chimp","kind":"ROLLBACK","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","referenceExternalTransactionId":"transaction-100",` +
		`"roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	if got := string(op.CanonicalJSON("provider-a")); got != want {
		t.Errorf("CanonicalJSON() =\n%s\nwant\n%s", got, want)
	}
}

func TestOperation_KindAndReferenceAreHashed(t *testing.T) {
	base := mustOperation(t, func(in *OperationInput) {
		in.Kind, in.ReferenceExternalTransactionID = "ROLLBACK", "tx-0"
	}).PayloadHash("provider-a")
	refund := mustOperation(t, func(in *OperationInput) {
		in.Kind, in.ReferenceExternalTransactionID = "REFUND", "tx-0"
	}).PayloadHash("provider-a")
	other := mustOperation(t, func(in *OperationInput) {
		in.Kind, in.ReferenceExternalTransactionID = "ROLLBACK", "tx-1"
	}).PayloadHash("provider-a")
	if base == refund {
		t.Error("kind is not part of the hash")
	}
	if base == other {
		t.Error("reference is not part of the hash")
	}
}

func TestStatus_Transitions(t *testing.T) {
	tests := []struct {
		from, to Status
		want     bool
	}{
		{StatusPending, StatusProcessed, true},
		{StatusPending, StatusPendingReference, true},
		{StatusPending, StatusRejected, true},
		{StatusPending, StatusFailed, true},
		{StatusPendingReference, StatusProcessed, true},
		{StatusPendingReference, StatusRejected, true},
		{StatusPendingReference, StatusFailed, true},
		{StatusPendingReference, StatusPending, false},
		{StatusPendingReference, StatusPendingReference, false},
		{StatusProcessed, StatusRejected, false},
		{StatusProcessed, StatusPending, false},
		{StatusRejected, StatusProcessed, false},
		{StatusFailed, StatusProcessed, false},
	}
	for _, tc := range tests {
		if got := tc.from.CanTransitionTo(tc.to); got != tc.want {
			t.Errorf("%s -> %s = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
	for _, s := range []Status{StatusProcessed, StatusRejected, StatusFailed} {
		if !s.IsTerminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	for _, s := range []Status{StatusPending, StatusPendingReference} {
		if s.IsTerminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}
