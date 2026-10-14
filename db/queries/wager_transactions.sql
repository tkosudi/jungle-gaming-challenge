-- name: InsertOpeningTransaction :exec
INSERT INTO wager_transactions (
    id, kind, status, wallet_id, player_id, amount, currency, result_balance, created_at, updated_at)
VALUES ($1, 'OPENING', 'PROCESSED', $2, $3, $4, $5, $4, $6, $6);

-- name: InsertBetTransaction :one
-- ON CONFLICT sem alvo cobre as duas UNIQUEs de idempotência do provedor; quem perde a corrida
-- espera o commit do vencedor e recebe zero linhas, sem abortar a transação.
INSERT INTO wager_transactions (
    id, kind, status, wallet_id, player_id, amount, currency,
    provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
    failure_code, result_balance, created_at, updated_at)
VALUES ($1, 'BET', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: FindProviderOperations :many
-- Uma linha por colisão possível: a da chave de idempotência e a do ID externo (podem ser distintas).
SELECT id, kind, status, idempotency_key, external_transaction_id, payload_hash,
       failure_code, result_balance, currency
FROM wager_transactions
WHERE provider_id = $1
  AND (idempotency_key = $2 OR external_transaction_id = $3);
