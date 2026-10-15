-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (
    event_id, event_type, schema_version, aggregate_id, message_group_id,
    transaction_id, payload, occurred_at, next_attempt_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $8);

-- name: ClaimOutboxHeads :many
-- Só a cabeça de cada grupo (carteira): um evento é elegível se nenhum anterior do mesmo grupo
-- está pendente, vencido ou não. SKIP LOCKED sozinho deixaria um segundo publicador pegar o
-- evento seguinte enquanto o anterior ainda está em envio; o NOT EXISTS impede essa
-- ultrapassagem, e o lock de linha impede dois publicadores no mesmo evento. Grupos
-- diferentes avançam em paralelo; não há lock global.
SELECT e.event_id, e.event_type, e.message_group_id, e.payload, e.sequence, e.attempts, e.created_at
FROM outbox_events e
WHERE e.published_at IS NULL
  AND e.next_attempt_at <= now()
  AND NOT EXISTS (
      SELECT 1 FROM outbox_events p
       WHERE p.message_group_id = e.message_group_id
         AND p.published_at IS NULL
         AND p.sequence < e.sequence)
ORDER BY e.sequence
LIMIT $1
FOR UPDATE OF e SKIP LOCKED;

-- name: MarkOutboxPublished :exec
UPDATE outbox_events SET published_at = now(), attempts = attempts + 1 WHERE event_id = $1;

-- name: ScheduleOutboxRetry :exec
UPDATE outbox_events
SET attempts = attempts + 1, next_attempt_at = now() + ($2::bigint * interval '1 millisecond')
WHERE event_id = $1;

-- name: OutboxBacklog :one
-- Para a métrica de atraso: idade do evento pendente mais antigo e quantidade pendente.
SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8 AS oldest_age_seconds,
       count(*) AS pending
FROM outbox_events
WHERE published_at IS NULL;
