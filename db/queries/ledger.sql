-- name: InsertLedgerEntry :exec
INSERT INTO wallet_ledger_entries (
    id, wallet_id, transaction_id, direction, amount, balance_before, balance_after, wallet_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListLedgerEntries :many
-- Ordem estável pela versão da carteira (única por carteira); o cursor é a última versão lida.
SELECT id, transaction_id, direction, amount, balance_before, balance_after, wallet_version, created_at
FROM wallet_ledger_entries
WHERE wallet_id = $1 AND wallet_version > $2
ORDER BY wallet_version
LIMIT $3;

-- name: GetReconciliationSnapshot :one
-- Um único statement: saldo armazenado e saldo reconstruído pelo ledger vêm do mesmo snapshot,
-- sem lock e sem bloquear escritores. SUM(bigint) é numeric (exato): os totais históricos de
-- cada direção podem passar de int64, então só o líquido (créditos - débitos) vira bigint.
SELECT w.currency, w.balance,
       COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount ELSE -l.amount END), 0)::bigint AS ledger_balance,
       count(l.id) AS entries
FROM wallets w
LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
WHERE w.id = $1
GROUP BY w.id;
