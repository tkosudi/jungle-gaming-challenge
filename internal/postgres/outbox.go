package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tkosudi/jungle-gaming-challenge/db/sqlc"
)

// OutboxMessage é um evento pronto para envio. Payload é o envelope imutável gravado com a
// operação; EventID é também o MessageDeduplicationId e MessageGroupID, o MessageGroupId.
type OutboxMessage struct {
	EventID        uuid.UUID
	EventType      string
	MessageGroupID string
	Payload        []byte
	Sequence       int64
	Attempts       int
	CreatedAt      time.Time
}

// PublishResult conta o que um lote publicou e o que ficou para nova tentativa.
type PublishResult struct {
	Published int
	Failed    int
}

// PublishOutbox publica um lote numa transação: seleciona até limit eventos que são cabeça do
// seu grupo (FOR UPDATE SKIP LOCKED), chama send para cada um na ordem de sequence e só depois
// de send devolver nil marca o evento como publicado. Se send falha, o evento ganha nova
// tentativa após retryDelay(tentativas anteriores) e os seguintes do mesmo grupo continuam
// esperando, porque o predecessor segue pendente.
//
// A garantia é at-least-once: se a confirmação no PostgreSQL falhar depois de o broker aceitar
// o envio (erro, queda do processo, COMMIT perdido), o lote inteiro volta a pendente e é
// reenviado com o mesmo eventId. Os consumidores deduplicam por eventId.
func (s *Store) PublishOutbox(ctx context.Context, limit int, retryDelay func(attempts int) time.Duration,
	send func(context.Context, OutboxMessage) error) (PublishResult, error) {
	var res PublishResult
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PublishResult{}, fmt.Errorf("begin outbox transaction: %w", mapError(err))
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	q := sqlc.New(tx)
	rows, err := q.ClaimOutboxHeads(ctx, int32(limit))
	if err != nil {
		return PublishResult{}, fmt.Errorf("claim outbox events: %w", mapError(err))
	}
	for _, row := range rows {
		m := OutboxMessage{
			EventID: row.EventID, EventType: row.EventType, MessageGroupID: row.MessageGroupID,
			Payload: row.Payload, Attempts: int(row.Attempts), CreatedAt: row.CreatedAt,
		}
		if row.Sequence != nil {
			m.Sequence = *row.Sequence
		}
		if sendErr := send(ctx, m); sendErr != nil {
			delay := retryDelay(m.Attempts)
			if err := q.ScheduleOutboxRetry(ctx, sqlc.ScheduleOutboxRetryParams{EventID: m.EventID, Column2: delay.Milliseconds()}); err != nil {
				return PublishResult{}, fmt.Errorf("schedule retry of event %s: %w", m.EventID, mapError(err))
			}
			res.Failed++
			continue
		}
		if err := q.MarkOutboxPublished(ctx, m.EventID); err != nil {
			return PublishResult{}, fmt.Errorf("mark event %s published: %w", m.EventID, mapError(err))
		}
		res.Published++
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, fmt.Errorf("commit outbox batch: %w", mapError(err))
	}
	return res, nil
}

// OutboxBacklog devolve a idade do evento pendente mais antigo e quantos estão pendentes.
func (s *Store) OutboxBacklog(ctx context.Context) (oldest time.Duration, pending int64, err error) {
	row, err := sqlc.New(s.pool).OutboxBacklog(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, mapError(err)
	}
	return time.Duration(row.OldestAgeSeconds * float64(time.Second)), row.Pending, nil
}
