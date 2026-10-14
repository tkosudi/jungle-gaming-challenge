//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

func TestReads_WalletAndLedgerPagination(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "100.00")
	for i, amount := range []string{"10.00", "20.00", "30.00"} {
		submit(t, svc, newOp(t, w, "BET", "bet-"+string(rune('a'+i)), amount, ""))
	}

	got, err := svc.GetWallet(ctx, w.ID)
	if err != nil || got.Balance.Amount() != "40.00" || got.Version != 4 || got.PlayerID != w.PlayerID {
		t.Fatalf("GetWallet = %+v, %v", got, err)
	}

	var versions []int64
	after := int64(0)
	for page := 0; page < 5; page++ {
		entries, more, err := svc.ListLedger(ctx, w.ID, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			versions = append(versions, e.WalletVersion)
			after = e.WalletVersion
		}
		if !more {
			break
		}
	}
	if len(versions) != 4 || versions[0] != 1 || versions[3] != 4 {
		t.Fatalf("paged versions = %v, want 1..4 in order", versions)
	}
	first, _, err := svc.ListLedger(ctx, w.ID, 0, 1)
	if err != nil || len(first) != 1 || first[0].Direction != domain.DirectionCredit || first[0].BalanceAfter.Amount() != "100.00" {
		t.Fatalf("first entry = %+v, %v", first, err)
	}
	if entries, more, err := svc.ListLedger(ctx, w.ID, 4, 2); err != nil || len(entries) != 0 || more {
		t.Fatalf("after the last entry = %v %v %v", entries, more, err)
	}

	if _, err := svc.GetWallet(ctx, uuid.New()); !errors.Is(err, usecase.ErrWalletNotFound) {
		t.Fatalf("unknown wallet: %v", err)
	}
	if _, _, err := svc.ListLedger(ctx, uuid.New(), 0, 10); !errors.Is(err, usecase.ErrWalletNotFound) {
		t.Fatalf("ledger of unknown wallet: %v", err)
	}
}

func TestReads_TransactionsAreScopedToTheirProvider(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	ctx := context.Background()
	w := openFunded(t, svc, "100.00")
	bet := submit(t, svc, newOp(t, w, "BET", "bet-1", "30.00", ""))
	pending := submit(t, svc, newOp(t, w, "ROLLBACK", "rb-1", "5.00", "bet-unknown"))

	got, err := svc.GetTransaction(ctx, provider, bet.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "BET" || got.Status != domain.StatusProcessed || got.ExternalTransactionID != "bet-1" ||
		got.ResultBalance == nil || got.ResultBalance.Amount() != "70.00" || got.Money.Amount() != "30.00" {
		t.Fatalf("transaction = %+v", got)
	}
	byExternal, err := svc.GetTransactionByExternalID(ctx, provider, "bet-1")
	if err != nil || byExternal.ID != bet.TransactionID {
		t.Fatalf("by external id = %+v, %v", byExternal, err)
	}

	p, err := svc.GetTransaction(ctx, provider, pending.TransactionID)
	if err != nil || p.Status != domain.StatusPendingReference || p.NextAttemptAt == nil ||
		p.ReferenceExternalTransactionID != "bet-unknown" || p.ResultBalance != nil {
		t.Fatalf("pending = %+v, %v", p, err)
	}

	// Outro provedor não distingue "não existe" de "é de outro provedor".
	if _, err := svc.GetTransaction(ctx, "provider-b", bet.TransactionID); !errors.Is(err, usecase.ErrTransactionNotFound) {
		t.Fatalf("other provider by id: %v", err)
	}
	if _, err := svc.GetTransactionByExternalID(ctx, "provider-b", "bet-1"); !errors.Is(err, usecase.ErrTransactionNotFound) {
		t.Fatalf("other provider by external id: %v", err)
	}
}

func TestStore_UnreachableDatabaseIsUnavailable(t *testing.T) {
	pool, err := newPool("postgres://jungle:x@127.0.0.1:1/none?sslmode=disable", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc := newTestService(t, NewStore(pool, time.Second), usecase.DefaultReferencePolicy())
	if _, err := svc.GetWallet(context.Background(), uuid.New()); !errors.Is(err, usecase.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}
