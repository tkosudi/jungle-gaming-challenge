-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (
    event_id, event_type, schema_version, aggregate_id, message_group_id,
    transaction_id, payload, occurred_at, next_attempt_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $8);
