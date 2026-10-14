-- name: FindInboxMessage :one
SELECT payload_hash, transaction_id, outcome
FROM inbox_messages
WHERE consumer_name = $1 AND message_id = $2;

-- name: InsertInboxMessage :one
-- ON CONFLICT: a mesma mensagem tratada em paralelo espera o commit da outra e não insere nada.
INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, transaction_id, outcome, received_at, completed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (consumer_name, message_id) DO NOTHING
RETURNING message_id;
