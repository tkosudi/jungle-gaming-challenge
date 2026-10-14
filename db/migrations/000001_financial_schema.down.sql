-- Reversão completa de 000001: remove as tabelas (e seus triggers) e, depois, as funções.
-- Destrutiva: apaga todos os dados financeiros. Não aplicar em produção com dados.
DROP TABLE wallet_ledger_entries;
DROP TABLE wager_transactions;
DROP TABLE wallets;

DROP FUNCTION check_transaction_matches_ledger();
DROP FUNCTION check_wallet_matches_ledger();
DROP FUNCTION check_ledger_entry_consistency();
DROP FUNCTION wager_transactions_guard_update();
DROP FUNCTION wallets_guard_update();
DROP FUNCTION forbid_mutation();
