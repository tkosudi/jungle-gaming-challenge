-- Reversão de 000003: remove o trigger e a função; as tabelas e os dados permanecem.
DROP TRIGGER wager_transactions_reference_consistency ON wager_transactions;
DROP FUNCTION check_reference_consistency();
