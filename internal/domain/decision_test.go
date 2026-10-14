package domain

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

type decideFixture struct {
	wallet Wallet
	round  string
}

func newDecideFixture(t *testing.T, balance string) decideFixture {
	t.Helper()
	m, err := NewMoney(balance, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return decideFixture{wallet: Wallet{ID: uuid.New(), PlayerID: uuid.New(), Balance: m, Version: 4}, round: "round-1"}
}

func (f decideFixture) op(t *testing.T, kind Kind, amount, reference string) Operation {
	t.Helper()
	op, err := NewOperation(OperationInput{
		Kind: string(kind), ExternalTransactionID: "tx-new", IdempotencyKey: "key-new",
		PlayerID: f.wallet.PlayerID.String(), WalletID: f.wallet.ID.String(),
		RoundID: f.round, GameID: "game-1", Amount: amount, Currency: "BRL",
		ReferenceExternalTransactionID: reference,
	})
	if err != nil {
		t.Fatalf("NewOperation: %v", err)
	}
	return op
}

func (f decideFixture) ref(t *testing.T, kind Kind, status Status, amount string) *Reference {
	t.Helper()
	m, err := NewMoney(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	id := f.wallet.ID
	return &Reference{
		TransactionID: uuid.New(), Kind: kind, Status: status, PlayerID: f.wallet.PlayerID,
		WalletID: &id, RoundID: f.round, Money: m,
	}
}

func TestDecide(t *testing.T) {
	f := newDecideFixture(t, "100.00")
	processedBet := func() *Reference { return f.ref(t, KindBet, StatusProcessed, "25.00") }

	tests := []struct {
		name          string
		wallet        *Wallet
		op            Operation
		ref           *Reference
		wantStatus    Status
		wantCode      string
		wantDirection Direction
		wantBalance   string // saldo informado; "" = nenhum
		wantVersion   int64  // versão da carteira após a decisão
	}{
		{"BET debits", &f.wallet, f.op(t, KindBet, "25.00", ""), nil, StatusProcessed, "", DirectionDebit, "75.00", 5},
		{"BET with insufficient funds", &f.wallet, f.op(t, KindBet, "100.01", ""), nil, StatusRejected, FailureInsufficientFunds, "", "100.00", 4},
		{"BET exactly the balance", &f.wallet, f.op(t, KindBet, "100.00", ""), nil, StatusProcessed, "", DirectionDebit, "0.00", 5},
		{"unknown wallet", nil, f.op(t, KindBet, "1.00", ""), nil, StatusRejected, FailureWalletNotFound, "", "", 0},
		{"LOSS does not move", &f.wallet, f.op(t, KindLoss, "0.00", ""), nil, StatusProcessed, "", "", "100.00", 4},
		{"WIN without reference", &f.wallet, f.op(t, KindWin, "10.00", ""), nil, StatusProcessed, "", DirectionCredit, "110.00", 5},
		{"WIN referencing a BET of the same round", &f.wallet, f.op(t, KindWin, "10.00", "bet-1"), processedBet(), StatusProcessed, "", DirectionCredit, "110.00", 5},
		{"WIN with reference not yet received", &f.wallet, f.op(t, KindWin, "10.00", "bet-1"), nil, StatusPendingReference, "", "", "", 4},
		{"WIN referencing a WIN", &f.wallet, f.op(t, KindWin, "10.00", "w-1"), f.ref(t, KindWin, StatusProcessed, "10.00"), StatusRejected, FailureReferenceKindInvalid, "", "100.00", 4},
		{"WIN referencing another round", &f.wallet, f.op(t, KindWin, "10.00", "bet-1"), func() *Reference {
			r := processedBet()
			r.RoundID = "round-2"
			return r
		}(), StatusRejected, FailureReferenceMismatch, "", "100.00", 4},

		{"REFUND credits the BET", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), processedBet(), StatusProcessed, "", DirectionCredit, "125.00", 5},
		{"REFUND referencing a WIN", &f.wallet, f.op(t, KindRefund, "25.00", "w-1"), f.ref(t, KindWin, StatusProcessed, "25.00"), StatusRejected, FailureReferenceKindInvalid, "", "100.00", 4},
		{"REFUND with another amount", &f.wallet, f.op(t, KindRefund, "24.99", "bet-1"), processedBet(), StatusRejected, FailureAmountMismatch, "", "100.00", 4},
		{"REFUND of a rejected BET", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), f.ref(t, KindBet, StatusRejected, "25.00"), StatusRejected, FailureReferenceNotProcessed, "", "100.00", 4},
		{"REFUND of a failed BET", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), f.ref(t, KindBet, StatusFailed, "25.00"), StatusRejected, FailureReferenceNotProcessed, "", "100.00", 4},
		{"REFUND of an already compensated BET", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), func() *Reference {
			r := processedBet()
			r.Compensated = true
			return r
		}(), StatusRejected, FailureReferenceAlreadyCompensated, "", "100.00", 4},
		{"REFUND referencing a BET of another player", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), func() *Reference {
			r := processedBet()
			r.PlayerID = uuid.New()
			return r
		}(), StatusRejected, FailureReferenceMismatch, "", "100.00", 4},
		{"REFUND referencing a BET of another wallet", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), func() *Reference {
			r := processedBet()
			other := uuid.New()
			r.WalletID = &other
			return r
		}(), StatusRejected, FailureReferenceMismatch, "", "100.00", 4},
		{"REFUND referencing a rejected BET without wallet", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), func() *Reference {
			r := f.ref(t, KindBet, StatusRejected, "25.00")
			r.WalletID = nil
			return r
		}(), StatusRejected, FailureReferenceNotProcessed, "", "100.00", 4},
		{"REFUND without the BET", &f.wallet, f.op(t, KindRefund, "25.00", "bet-1"), nil, StatusPendingReference, "", "", "", 4},

		{"ROLLBACK of a BET credits", &f.wallet, f.op(t, KindRollback, "25.00", "bet-1"), processedBet(), StatusProcessed, "", DirectionCredit, "125.00", 5},
		{"ROLLBACK of a WIN debits", &f.wallet, f.op(t, KindRollback, "30.00", "w-1"), f.ref(t, KindWin, StatusProcessed, "30.00"), StatusProcessed, "", DirectionDebit, "70.00", 5},
		{"ROLLBACK of a REFUND debits", &f.wallet, f.op(t, KindRollback, "30.00", "r-1"), f.ref(t, KindRefund, StatusProcessed, "30.00"), StatusProcessed, "", DirectionDebit, "70.00", 5},
		{"ROLLBACK of a WIN without funds", &f.wallet, f.op(t, KindRollback, "100.01", "w-1"), f.ref(t, KindWin, StatusProcessed, "100.01"), StatusRejected, FailureReversalInsufficientFunds, "", "100.00", 4},
		{"ROLLBACK of a ROLLBACK", &f.wallet, f.op(t, KindRollback, "30.00", "rb-1"), f.ref(t, KindRollback, StatusProcessed, "30.00"), StatusRejected, FailureReferenceKindInvalid, "", "100.00", 4},
		{"ROLLBACK of a pending WIN stays pending", &f.wallet, f.op(t, KindRollback, "30.00", "w-1"), f.ref(t, KindWin, StatusPendingReference, "30.00"), StatusPendingReference, "", "", "", 4},
		{"ROLLBACK of a BET after REFUND", &f.wallet, f.op(t, KindRollback, "25.00", "bet-1"), func() *Reference {
			r := processedBet()
			r.Compensated = true
			return r
		}(), StatusRejected, FailureReferenceAlreadyCompensated, "", "100.00", 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Decide(tc.wallet, tc.op, tc.ref)
			if err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			if d.Status != tc.wantStatus || d.FailureCode != tc.wantCode || d.Direction != tc.wantDirection {
				t.Fatalf("Decide() = %s %q %q, want %s %q %q", d.Status, d.FailureCode, d.Direction, tc.wantStatus, tc.wantCode, tc.wantDirection)
			}
			switch {
			case tc.wantBalance == "" && d.ResultBalance != nil:
				t.Errorf("ResultBalance = %s, want none", d.ResultBalance)
			case tc.wantBalance != "" && (d.ResultBalance == nil || d.ResultBalance.Amount() != tc.wantBalance):
				t.Errorf("ResultBalance = %v, want %s", d.ResultBalance, tc.wantBalance)
			}
			if tc.wantBalance != "" && d.Status == StatusProcessed && d.Wallet.Balance.Amount() != tc.wantBalance {
				t.Errorf("Wallet.Balance = %s, want %s", d.Wallet.Balance, tc.wantBalance)
			}
			if tc.wallet != nil && d.Wallet.Version != tc.wantVersion && d.WalletMatched {
				t.Errorf("Wallet.Version = %d, want %d", d.Wallet.Version, tc.wantVersion)
			}
			if d.Status == StatusProcessed && tc.ref != nil && (d.ReferenceID == nil || *d.ReferenceID != tc.ref.TransactionID) {
				t.Errorf("ReferenceID = %v, want %s", d.ReferenceID, tc.ref.TransactionID)
			}
			if d.Status != StatusProcessed && d.ReferenceID != nil {
				t.Error("ReferenceID set on a non-processed decision")
			}
		})
	}
}

func TestDecide_WalletProblemsPersistNoWallet(t *testing.T) {
	f := newDecideFixture(t, "100.00")
	foreign := f.wallet
	foreign.PlayerID = uuid.New()
	usd := f.op(t, KindBet, "1.00", "")
	usd.Money, _ = NewMoney("1.00", "USD")

	for name, d := range map[string]Decision{
		"wallet of another player": mustDecide(t, &foreign, f.op(t, KindBet, "1.00", ""), nil),
		"currency differs":         mustDecide(t, &f.wallet, usd, nil),
	} {
		if d.Status != StatusRejected || d.WalletMatched || d.ResultBalance != nil {
			t.Errorf("%s: %+v, want rejection without wallet", name, d)
		}
	}
	if d := mustDecide(t, &f.wallet, usd, nil); d.FailureCode != FailureCurrencyMismatch {
		t.Errorf("code = %s, want %s", d.FailureCode, FailureCurrencyMismatch)
	}
}

func TestDecide_CreditOverflowIsAnAuditableRejection(t *testing.T) {
	f := newDecideFixture(t, "100.00")
	f.wallet.Balance, _ = MoneyFromCents(math.MaxInt64, "BRL")

	d := mustDecide(t, &f.wallet, f.op(t, KindWin, "0.01", ""), nil)
	if d.Status != StatusRejected || d.FailureCode != FailureBalanceOverflow || d.Direction != "" {
		t.Fatalf("Decide() = %+v, want REJECTED %s", d, FailureBalanceOverflow)
	}
}

func mustDecide(t *testing.T, w *Wallet, op Operation, ref *Reference) Decision {
	t.Helper()
	d, err := Decide(w, op, ref)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	return d
}

func TestDecide_MalformedOperationIsAnErrorNotARejection(t *testing.T) {
	f := newDecideFixture(t, "100.00")
	var zero Operation
	zero.Kind, zero.PlayerID = KindBet, f.wallet.PlayerID
	if _, err := Decide(&f.wallet, zero, nil); err == nil {
		t.Fatal("Decide() accepted an operation without money")
	}
}
