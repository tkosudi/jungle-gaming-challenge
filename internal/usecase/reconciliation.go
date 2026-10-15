package usecase

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
)

// Reconciliation compara o saldo armazenado com o reconstruído pelo ledger.
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     domain.Money
	CalculatedBalance domain.Money // créditos - débitos, incluindo a OPENING
	// Difference é armazenado - calculado; pode ser negativa.
	Difference     domain.Money
	Consistent     bool
	CheckedEntries int64
}

// Reconcile reconstrói o saldo da carteira a partir do ledger e o compara com o armazenado.
// Roda numa transação somente leitura e num único statement (um snapshot): não trava a
// carteira, não espera escritores e nunca altera saldo ou ledger. Divergências são registradas
// em log estruturado e na métrica wallet_reconciliations_total{result="divergent"}.
func (s *Service) Reconcile(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	var snap ReconciliationSnapshot
	err := s.store.InReadOnlyTx(ctx, func(tx Tx) error {
		var err error
		snap, err = tx.ReconciliationSnapshot(ctx, walletID)
		return err
	})
	if err != nil {
		return Reconciliation{}, err
	}

	calculated := snap.LedgerBalance
	difference, err := snap.StoredBalance.Sub(calculated)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile wallet %s: %w", walletID, err)
	}
	r := Reconciliation{
		WalletID: walletID, StoredBalance: snap.StoredBalance, CalculatedBalance: calculated,
		Difference: difference, Consistent: difference.IsZero(), CheckedEntries: snap.Entries,
	}

	if r.Consistent {
		metrics.Reconciliations.WithLabelValues("consistent").Inc()
		return r, nil
	}
	metrics.Reconciliations.WithLabelValues("divergent").Inc()
	slog.WarnContext(ctx, "wallet reconciliation divergence",
		"walletId", walletID.String(),
		"currency", string(r.StoredBalance.Currency()),
		"storedBalance", r.StoredBalance.Amount(),
		"calculatedBalance", r.CalculatedBalance.Amount(),
		"difference", r.Difference.Amount(),
		"checkedEntries", r.CheckedEntries,
	)
	return r, nil
}
