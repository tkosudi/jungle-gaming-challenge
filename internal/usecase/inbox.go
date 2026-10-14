package usecase

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

// InboxMessage identifica uma mensagem recebida por um consumidor.
type InboxMessage struct {
	ConsumerName string
	MessageID    string
	// PayloadHash é o hash do conteúdo canônico da mensagem, comparado nas reentregas.
	PayloadHash [32]byte
	// ReceivedAt é o primeiro recebimento informado pelo broker.
	ReceivedAt time.Time
}

// MessageResult é o resultado de uma mensagem: o da operação ou, em duplicata, o registrado.
type MessageResult struct {
	Result
	// Duplicate: a mensagem já estava concluída; nada foi alterado (Balance não é informado).
	Duplicate bool
}

// ProcessMessage aplica a operação de uma mensagem e registra sua conclusão na inbox, na
// MESMA transação: os efeitos financeiros, a outbox e a linha da inbox são confirmados juntos
// ou nenhum deles. Usa o mesmo fluxo de Process (idempotência por chave e hash da operação);
// a inbox acrescenta a deduplicação por messageId.
//
//  1. Inbox já tem a mensagem: mesmo hash é duplicata (sem efeitos); outro hash é
//     ErrMessageConflict.
//  2. Senão, processa a operação (replay se ela já existe, inclusive vinda do HTTP).
//  3. Registra a conclusão. Se outra transação a registrou no meio-tempo: mesmo hash, a
//     operação acima foi um replay e o resultado é duplicata; outro hash, ErrMessageConflict
//     e o rollback desfaz tudo o que esta transação fez.
//
// Rejeições de negócio e pendências são conclusões válidas da mensagem.
func (s *Service) ProcessMessage(ctx context.Context, msg InboxMessage, providerID string, op domain.Operation) (MessageResult, error) {
	if providerID == "" || strings.TrimSpace(providerID) != providerID {
		return MessageResult{}, ErrInvalidProvider
	}
	if msg.ConsumerName == "" || msg.MessageID == "" {
		return MessageResult{}, fmt.Errorf("%w: message without consumer or id", domain.ErrInvalidOperation)
	}
	opHash := op.PayloadHash(providerID)

	var result MessageResult
	err := s.store.InTx(ctx, func(tx Tx) error {
		if res, done, err := completedMessage(ctx, tx, msg); done || err != nil {
			result = res
			return err
		}
		res, err := s.process(ctx, tx, providerID, op, opHash)
		if err != nil {
			return err
		}
		inserted, err := tx.InsertInboxMessage(ctx, InboxRecord{
			ConsumerName: msg.ConsumerName, MessageID: msg.MessageID, PayloadHash: msg.PayloadHash,
			TransactionID: res.TransactionID, Outcome: res.Status, ReceivedAt: msg.ReceivedAt, CompletedAt: time.Now().UTC(),
		})
		if err != nil {
			return fmt.Errorf("insert inbox message: %w", err)
		}
		if !inserted {
			res, _, err := completedMessage(ctx, tx, msg)
			result = res
			return err
		}
		result = MessageResult{Result: res}
		return nil
	})
	if err != nil {
		return MessageResult{}, err
	}
	return result, nil
}

// completedMessage devolve done=true se a mensagem já foi concluída com o mesmo conteúdo, e
// ErrMessageConflict se foi concluída com outro.
func completedMessage(ctx context.Context, tx Tx, msg InboxMessage) (MessageResult, bool, error) {
	stored, found, err := tx.FindInboxMessage(ctx, msg.ConsumerName, msg.MessageID)
	if err != nil {
		return MessageResult{}, false, fmt.Errorf("find inbox message: %w", err)
	}
	if !found {
		return MessageResult{}, false, nil
	}
	if !bytes.Equal(stored.PayloadHash, msg.PayloadHash[:]) {
		return MessageResult{}, false, fmt.Errorf("%w: %s", ErrMessageConflict, msg.MessageID)
	}
	return MessageResult{
		Result:    Result{TransactionID: stored.TransactionID, Status: stored.Outcome, Replay: true},
		Duplicate: true,
	}, true, nil
}
