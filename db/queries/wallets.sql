-- name: InsertWalletIfAbsent :one
-- ON CONFLICT: sob concorrência a segunda inserção espera o commit da primeira e então não
-- insere nada (sem linha retornada), sem abortar a transação.
INSERT INTO wallets (id, player_id, currency, balance, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, 1, $5, $5)
ON CONFLICT (player_id, currency) DO NOTHING
RETURNING id;

-- name: GetWalletByPlayerAndCurrency :one
SELECT id, player_id, currency, balance, version
FROM wallets
WHERE player_id = $1 AND currency = $2;

-- name: LockWallet :one
-- Serializa todas as escritas sobre a carteira até o fim da transação.
SELECT id, player_id, currency, balance, version
FROM wallets
WHERE id = $1
FOR UPDATE;

-- name: UpdateWalletBalance :exec
UPDATE wallets
SET balance = $2, version = $3, updated_at = $4
WHERE id = $1;

-- name: GetWallet :one
SELECT id, player_id, currency, balance, version
FROM wallets
WHERE id = $1;
