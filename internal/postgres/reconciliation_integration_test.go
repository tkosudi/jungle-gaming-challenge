//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

func reconcile(t *testing.T, svc *usecase.Service, id uuid.UUID) usecase.Reconciliation {
	t.Helper()
	r, err := svc.Reconcile(context.Background(), id)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return r
}

func requireReconciliation(t *testing.T, r usecase.Reconciliation, stored, calculated, difference string, entries int64) {
	t.Helper()
	if r.StoredBalance.Amount() != stored || r.CalculatedBalance.Amount() != calculated ||
		r.Difference.Amount() != difference || r.CheckedEntries != entries || r.Consistent != (difference == "0.00") {
		t.Fatalf("reconciliation = stored %s calculated %s difference %s consistent %v entries %d; want %s %s %s %d",
			r.StoredBalance.Amount(), r.CalculatedBalance.Amount(), r.Difference.Amount(), r.Consistent, r.CheckedEntries,
			stored, calculated, difference, entries)
	}
}

func TestReconcile_ConsistentWallets(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]

	t.Run("opening and one bet", func(t *testing.T) {
		w := openFunded(t, svc, "1000.00")
		submit(t, svc, newOp(t, w, "BET", "bet-1", "25.00", ""))
		requireReconciliation(t, reconcile(t, svc, w.ID), "975.00", "975.00", "0.00", 2)
	})
	t.Run("zero opening has no entries", func(t *testing.T) {
		w := openFunded(t, svc, "0.00")
		requireReconciliation(t, reconcile(t, svc, w.ID), "0.00", "0.00", "0.00", 0)
	})
	t.Run("many movements of every kind", func(t *testing.T) {
		w := openFunded(t, svc, "100.00")
		for _, op := range []domain.Operation{
			newOp(t, w, "BET", "m-bet-1", "30.00", ""),
			newOp(t, w, "WIN", "m-win-1", "45.50", "m-bet-1"),
			newOp(t, w, "LOSS", "m-loss-1", "0.00", ""),
			newOp(t, w, "BET", "m-bet-2", "10.00", ""),
			newOp(t, w, "REFUND", "m-refund-2", "10.00", "m-bet-2"),
			newOp(t, w, "ROLLBACK", "m-rb-win", "45.50", "m-win-1"),
			newOp(t, w, "BET", "m-bet-big", "9999.00", ""), // rejeitada: sem lançamento
		} {
			submit(t, svc, op)
		}
		// 100 - 30 + 45.50 - 10 + 10 - 45.50 = 70.00; lançamentos: abertura + 5 movimentos.
		requireReconciliation(t, reconcile(t, svc, w.ID), "70.00", "70.00", "0.00", 6)
	})
	// Regressão: os totais históricos de cada direção podem passar de int64 mesmo com o saldo
	// líquido válido; só o líquido precisa caber em bigint.
	t.Run("historical credits above int64 with a valid net balance", func(t *testing.T) {
		w := openFunded(t, svc, "92233720368547758.07") // MaxInt64 centavos
		submit(t, svc, newOp(t, w, "BET", "max-bet", "0.01", ""))
		submit(t, svc, newOp(t, w, "WIN", "max-win", "0.01", ""))
		requireWallet(t, db, w.ID, math.MaxInt64, 3)
		requireReconciliation(t, reconcile(t, svc, w.ID), "92233720368547758.07", "92233720368547758.07", "0.00", 3)
	})
	t.Run("unknown wallet", func(t *testing.T) {
		if _, err := svc.Reconcile(context.Background(), uuid.New()); !errors.Is(err, usecase.ErrWalletNotFound) {
			t.Fatalf("error = %v, want ErrWalletNotFound", err)
		}
	})
}

// corrupt altera o saldo sem lançamento, como só o dono das tabelas consegue (limite D8):
// desliga os triggers, grava e religa. Simula exatamente o que a reconciliação deve detectar.
func (db *schemaDB) corrupt(t *testing.T, walletID uuid.UUID, deltaCents int64) {
	t.Helper()
	db.mustExec(t, fmt.Sprintf(`ALTER TABLE wallets DISABLE TRIGGER USER;
		UPDATE wallets SET balance = balance + (%d) WHERE id = '%s';
		ALTER TABLE wallets ENABLE TRIGGER USER;`, deltaCents, walletID))
}

func TestReconcile_ReportsDivergencesWithoutChangingAnything(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cases := []struct {
		name       string
		delta      int64
		stored     string
		difference string
	}{
		{"stored above the ledger", 500, "105.00", "5.00"},
		{"stored below the ledger", -2550, "74.50", "-25.50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := openFunded(t, svc, "100.00")
			db.corrupt(t, w.ID, tc.delta)
			cents, version := db.walletState(t, w.ID)
			entries := db.ledgerCount(t, w.ID)
			divergent := testutil.ToFloat64(metrics.Reconciliations.WithLabelValues("divergent"))
			logs.Reset()

			requireReconciliation(t, reconcile(t, svc, w.ID), tc.stored, "100.00", tc.difference, 1)

			requireWallet(t, db, w.ID, cents, version)
			if n := db.ledgerCount(t, w.ID); n != entries {
				t.Fatalf("ledger entries = %d, want %d", n, entries)
			}
			if got := testutil.ToFloat64(metrics.Reconciliations.WithLabelValues("divergent")); got != divergent+1 {
				t.Fatalf("divergent reconciliations metric = %v, want %v", got, divergent+1)
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("divergence log %q is not one JSON record: %v", logs.String(), err)
			}
			if entry["level"] != "WARN" || entry["walletId"] != w.ID.String() || entry["difference"] != tc.difference ||
				entry["storedBalance"] != tc.stored || entry["calculatedBalance"] != "100.00" || entry["currency"] != "BRL" {
				t.Fatalf("divergence log = %v", entry)
			}
		})
	}

	t.Run("consistent wallets are counted but not logged", func(t *testing.T) {
		w := openFunded(t, svc, "10.00")
		consistent := testutil.ToFloat64(metrics.Reconciliations.WithLabelValues("consistent"))
		logs.Reset()
		reconcile(t, svc, w.ID)
		if logs.Len() != 0 {
			t.Fatalf("unexpected log: %s", logs.String())
		}
		if got := testutil.ToFloat64(metrics.Reconciliations.WithLabelValues("consistent")); got != consistent+1 {
			t.Fatalf("consistent reconciliations metric = %v, want %v", got, consistent+1)
		}
	})
}

// A carteira e o ledger vêm do mesmo snapshot: com escritores concorrentes, nenhuma leitura
// pode ver o saldo de um commit e o ledger de outro (o que apareceria como divergência falsa).
func TestReconcile_ConsistentViewWhileWritersRun(t *testing.T) {
	db := newSchemaDB(t)
	instances := db.instances(t, 3)
	w := openFunded(t, instances[0], "1000.00")

	var stop atomic.Bool
	var wg sync.WaitGroup
	var writes atomic.Int64
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; !stop.Load() && n < 200; n++ {
				kind, amount := "BET", "1.00"
				if n%2 == 1 {
					kind, amount = "WIN", "1.50"
				}
				op := newOp(t, w, kind, fmt.Sprintf("w%d-%d", i, n), amount, "")
				if _, err := instances[i].Process(context.Background(), provider, op); err != nil {
					t.Errorf("writer %d: %v", i, err)
					return
				}
				writes.Add(1)
			}
		}()
	}

	reads, last := 0, int64(0)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && writes.Load() < 300; reads++ {
		r := reconcile(t, instances[2], w.ID)
		if !r.Consistent {
			t.Fatalf("read %d saw a divergence during concurrent writes: %+v", reads, r)
		}
		if r.CheckedEntries < last {
			t.Fatalf("checked entries went back from %d to %d", last, r.CheckedEntries)
		}
		last = r.CheckedEntries
	}
	stop.Store(true)
	wg.Wait()

	final := reconcile(t, instances[2], w.ID)
	if !final.Consistent || final.CheckedEntries != writes.Load()+1 {
		t.Fatalf("final = %+v after %d writes, want consistent with %d entries", final, writes.Load(), writes.Load()+1)
	}
	t.Logf("%d reconciliations during %d concurrent writes", reads, writes.Load())
}

// A reconciliação não usa lock: responde mesmo com a carteira travada por um escritor.
func TestReconcile_DoesNotWaitForAWalletLock(t *testing.T) {
	db := newSchemaDB(t)
	svc := newTestService(t, NewStore(openPool(t, db.url), 200*time.Millisecond), usecase.DefaultReferencePolicy())
	w := openFunded(t, svc, "100.00")

	ctx := context.Background()
	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id=$1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	requireReconciliation(t, reconcile(t, svc, w.ID), "100.00", "100.00", "0.00", 1)
}

func TestStore_ReadOnlyTransactionRefusesWrites(t *testing.T) {
	db := newSchemaDB(t)
	store := NewStore(openPool(t, db.url), time.Second)
	svc := newTestService(t, store, usecase.DefaultReferencePolicy())
	w := openFunded(t, svc, "100.00")

	err := store.InReadOnlyTx(context.Background(), func(tx usecase.Tx) error {
		return tx.UpdateWallet(context.Background(), domain.Wallet{ID: w.ID, PlayerID: w.PlayerID, Balance: w.Balance, Version: 1}, time.Now())
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write in a read-only transaction: %v, want SQLSTATE 25006", err)
	}
}

func TestOpenWallet_RejectsANegativeInitialBalance(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	zero, _ := domain.NewMoney("0.00", "BRL")
	cent, _ := domain.NewMoney("0.01", "BRL")
	negative, err := zero.Sub(cent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenWallet(context.Background(), uuid.New(), negative); !errors.Is(err, domain.ErrNegativeAmount) {
		t.Fatalf("OpenWallet(-0.01) error = %v, want ErrNegativeAmount", err)
	}
	if n := db.count(t, `SELECT count(*) FROM wallets`); n != 0 {
		t.Fatalf("wallets = %d, want 0", n)
	}
}
