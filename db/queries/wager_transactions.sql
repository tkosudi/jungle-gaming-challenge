-- name: InsertOpeningTransaction :exec
INSERT INTO wager_transactions (
    id, kind, status, wallet_id, player_id, amount, currency, result_balance, created_at, updated_at)
VALUES ($1, 'OPENING', 'PROCESSED', $2, $3, $4, $5, $4, $6, $6);

-- name: InsertOperation :one
-- ON CONFLICT sem alvo cobre as duas UNIQUEs de idempotência do provedor; quem perde a corrida
-- espera o commit do vencedor e recebe zero linhas, sem abortar a transação. A primeira
-- tentativa de uma pendência é agendada pelo relógio do banco ($18 em milissegundos).
INSERT INTO wager_transactions (
    id, kind, status, wallet_id, player_id, amount, currency,
    provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
    reference_external_transaction_id, reference_transaction_id,
    failure_code, result_balance, next_attempt_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
        CASE WHEN $3::text = 'PENDING_REFERENCE' THEN now() + ($18::bigint * interval '1 millisecond') END,
        $19, $19)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: FindProviderOperations :many
-- Uma linha por colisão possível: a da chave de idempotência e a do ID externo (podem ser distintas).
SELECT id, kind, status, idempotency_key, external_transaction_id, payload_hash,
       failure_code, result_balance, currency
FROM wager_transactions
WHERE provider_id = $1
  AND (idempotency_key = $2 OR external_transaction_id = $3);

-- name: FindReferenceTransaction :one
-- compensated: já existe REFUND ou ROLLBACK processado sobre a transação (DEC-001).
SELECT t.id, t.kind, t.status, t.player_id, t.wallet_id, t.round_id, t.amount, t.currency,
       EXISTS (
           SELECT 1 FROM wager_transactions c
            WHERE c.reference_transaction_id = t.id
              AND c.status = 'PROCESSED' AND c.kind IN ('REFUND', 'ROLLBACK')
       ) AS compensated
FROM wager_transactions t
WHERE t.provider_id = $1 AND t.external_transaction_id = $2;

-- name: ClaimDuePendingReference :one
-- Trava a carteira da pendência, não a transação: a ordem de locks é sempre carteira ->
-- transação, e SKIP LOCKED evita esperar por uma carteira ocupada (e deadlock entre workers).
-- O snapshot deste statement pode estar velho depois do lock; quem chama relê a pendência.
SELECT t.id, t.wallet_id
FROM wager_transactions t
JOIN wallets w ON w.id = t.wallet_id
WHERE t.status = 'PENDING_REFERENCE' AND t.next_attempt_at <= now()
ORDER BY t.next_attempt_at, t.id
LIMIT 1
FOR UPDATE OF w SKIP LOCKED;

-- name: GetDuePendingReference :one
SELECT id, wallet_id, player_id, kind, amount, currency, provider_id, external_transaction_id,
       idempotency_key, round_id, game_id, reference_external_transaction_id, reference_attempts
FROM wager_transactions
WHERE id = $1 AND status = 'PENDING_REFERENCE' AND next_attempt_at <= now();

-- name: ResolvePendingReference :execrows
UPDATE wager_transactions
SET status = $2, failure_code = $3, result_balance = $4, reference_transaction_id = $5,
    next_attempt_at = NULL, updated_at = $6
WHERE id = $1 AND status = 'PENDING_REFERENCE';

-- name: ReschedulePendingReference :execrows
UPDATE wager_transactions
SET reference_attempts = $2, next_attempt_at = now() + ($3::bigint * interval '1 millisecond'), updated_at = $4
WHERE id = $1 AND status = 'PENDING_REFERENCE';
