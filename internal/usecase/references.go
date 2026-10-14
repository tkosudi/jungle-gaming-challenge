package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

// ReferencePolicy governa as tentativas de uma operação PENDING_REFERENCE. O prazo é dado
// por número de tentativas, não por relógio: o tempo em que nenhum worker rodou não consome
// tentativas, e a expiração fica determinística.
//
// A espera depois de n tentativas é min(InitialBackoff * 2^n, MaxBackoff), medida pelo
// relógio do banco, comum a todas as instâncias.
type ReferencePolicy struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// MaxAttempts é a quantidade de tentativas do worker; na última, se a referência continua
	// inexistente, a operação é rejeitada com REFERENCE_NOT_FOUND.
	MaxAttempts int
}

// DefaultReferencePolicy: 1s, dobrando até 60s, com 10 tentativas (cerca de 4 minutos).
func DefaultReferencePolicy() ReferencePolicy {
	return ReferencePolicy{InitialBackoff: time.Second, MaxBackoff: time.Minute, MaxAttempts: 10}
}

func (p ReferencePolicy) validate() error {
	switch {
	case p.InitialBackoff < 0:
		return errors.New("usecase: reference policy: initial backoff must not be negative")
	case p.MaxBackoff < p.InitialBackoff:
		return errors.New("usecase: reference policy: max backoff must not be below the initial backoff")
	case p.MaxAttempts < 1:
		return errors.New("usecase: reference policy: max attempts must be at least 1")
	}
	return nil
}

func (p ReferencePolicy) backoff(attempts int) time.Duration {
	delay := p.InitialBackoff
	for i := 0; i < attempts && delay < p.MaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, p.MaxBackoff)
}

// PendingStep descreve o que um passo do worker fez.
type PendingStep struct {
	// Found é falso quando não havia pendência vencida e liberável (ou outra instância a
	// resolveu entre a seleção e o lock): nada foi alterado.
	Found         bool
	TransactionID uuid.UUID
	// Status é o estado depois do passo: PENDING_REFERENCE (reagendada), PROCESSED ou REJECTED.
	Status      domain.Status
	FailureCode string
	Attempts    int
}

// ResolveNextPendingReference executa um passo durável do worker de referências: escolhe uma
// pendência vencida e a leva a PROCESSED ou REJECTED, ou registra a tentativa e a reagenda.
// Tudo acontece numa transação; qualquer erro desfaz o passo sem consumir tentativa.
//
// Ordem de locks: a carteira da pendência (FOR UPDATE SKIP LOCKED) sempre antes de qualquer
// outra leitura da pendência ou da referência. Depois do lock a pendência é relida e
// revalidada: outra instância pode tê-la resolvido entre a seleção e o lock.
//
// A referência é procurada antes de qualquer expiração: se ela chegou, a operação é decidida
// por Decide mesmo na última tentativa. Referência existente mas pendente nunca expira (a
// cadeia é finita: ROLLBACK -> WIN/REFUND pendente -> BET ausente, que expira).
//
// Erros SQL inesperados (inclusive de integridade) voltam como erro e desfazem o passo; não
// há conversão automática para FAILED, que exigiria uma decisão explícita.
func (s *Service) ResolveNextPendingReference(ctx context.Context) (PendingStep, error) {
	var step PendingStep
	err := s.store.InTx(ctx, func(tx Tx) error {
		claim, found, err := tx.ClaimDuePendingReference(ctx)
		if err != nil {
			return fmt.Errorf("claim pending reference: %w", err)
		}
		if !found {
			return nil
		}
		pending, found, err := tx.LoadDuePendingReference(ctx, claim.TransactionID)
		if err != nil {
			return fmt.Errorf("reload pending reference: %w", err)
		}
		if !found {
			return nil
		}
		step, err = s.resolve(ctx, tx, claim.WalletID, pending)
		return err
	})
	if err != nil {
		return PendingStep{}, err
	}
	return step, nil
}

func (s *Service) resolve(ctx context.Context, tx Tx, walletID uuid.UUID, p PendingOperation) (PendingStep, error) {
	// A carteira já está travada por este passo; a leitura traz o saldo e a versão atuais.
	wallet, err := tx.LockWallet(ctx, walletID)
	if err != nil {
		return PendingStep{}, fmt.Errorf("lock wallet of pending transaction %s: %w", p.TransactionID, err)
	}
	ref, err := tx.FindReference(ctx, p.ProviderID, p.Operation.ReferenceExternalTransactionID)
	if err != nil {
		return PendingStep{}, fmt.Errorf("find reference: %w", err)
	}
	decision, err := domain.Decide(&wallet, p.Operation, ref)
	if err != nil {
		return PendingStep{}, err
	}
	if !decision.WalletMatched {
		return PendingStep{}, fmt.Errorf("pending transaction %s no longer matches its wallet", p.TransactionID)
	}

	attempts := p.Attempts + 1
	at := time.Now().UTC()
	step := PendingStep{Found: true, TransactionID: p.TransactionID, Attempts: attempts}

	if decision.Status == domain.StatusPendingReference {
		if ref != nil || attempts < s.policy.MaxAttempts {
			err := tx.ReschedulePendingReference(ctx, p.TransactionID, attempts, s.policy.backoff(attempts), at)
			if err != nil {
				return PendingStep{}, fmt.Errorf("reschedule pending reference: %w", err)
			}
			step.Status = domain.StatusPendingReference
			return step, nil
		}
		decision = domain.ExpiredReference(wallet)
	}

	if !domain.StatusPendingReference.CanTransitionTo(decision.Status) {
		return PendingStep{}, fmt.Errorf("invalid transition PENDING_REFERENCE -> %s", decision.Status)
	}
	err = tx.ResolvePendingReference(ctx, PendingResolution{
		TransactionID: p.TransactionID, Status: decision.Status, FailureCode: decision.FailureCode,
		ResultBalance: decision.ResultBalance, ReferenceTransactionID: decision.ReferenceID, At: at,
	})
	if err != nil {
		return PendingStep{}, fmt.Errorf("resolve pending reference: %w", err)
	}
	t := storedTransaction{id: p.TransactionID, providerID: p.ProviderID, op: p.Operation, walletID: &walletID}
	if err := writeEffects(ctx, tx, t, wallet, decision, at); err != nil {
		return PendingStep{}, err
	}
	step.Status, step.FailureCode = decision.Status, decision.FailureCode
	return step, nil
}
