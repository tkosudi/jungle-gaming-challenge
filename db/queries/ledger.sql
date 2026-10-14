-- name: InsertLedgerEntry :exec
INSERT INTO wallet_ledger_entries (
    id, wallet_id, transaction_id, direction, amount, balance_before, balance_after, wallet_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
