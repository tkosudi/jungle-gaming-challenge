package usecase

import (
	"context"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

// As leituras usam a mesma transação READ COMMITTED das escritas, sem locks: veem apenas
// dados confirmados e nunca bloqueiam uma operação financeira.

func (s *Service) GetWallet(ctx context.Context, id uuid.UUID) (domain.Wallet, error) {
	var w domain.Wallet
	err := s.store.InTx(ctx, func(tx Tx) error {
		var err error
		w, err = tx.GetWallet(ctx, id)
		return err
	})
	return w, err
}

// ListLedger devolve até limit lançamentos depois de afterVersion (0 para o início), na ordem
// da versão da carteira; more indica que há lançamentos além desta página.
func (s *Service) ListLedger(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) (entries []LedgerEntry, more bool, err error) {
	err = s.store.InTx(ctx, func(tx Tx) error {
		var err error
		entries, err = tx.ListLedgerEntries(ctx, walletID, afterVersion, limit+1)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

// GetTransaction consulta uma transação do provedor pelo ID interno.
func (s *Service) GetTransaction(ctx context.Context, providerID string, id uuid.UUID) (Transaction, error) {
	var t Transaction
	err := s.store.InTx(ctx, func(tx Tx) error {
		var err error
		t, err = tx.GetProviderTransaction(ctx, providerID, id)
		return err
	})
	return t, err
}

// GetTransactionByExternalID consulta uma transação do provedor pelo ID externo.
func (s *Service) GetTransactionByExternalID(ctx context.Context, providerID, externalID string) (Transaction, error) {
	var t Transaction
	err := s.store.InTx(ctx, func(tx Tx) error {
		var err error
		t, err = tx.GetProviderTransactionByExternalID(ctx, providerID, externalID)
		return err
	})
	return t, err
}
