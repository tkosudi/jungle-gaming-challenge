-- Integridade da referência de uma transação PROCESSED. As regras de negócio vivem no domínio;
-- este trigger é a defesa do banco para que nenhum escritor (inclusive um bug) confirme um
-- crédito ou débito sobre uma referência incompatível. Rejeições e pendências não são
-- verificadas: só quem movimentou dinheiro precisa de uma referência válida.
--
-- O mesmo provedor e a mesma carteira já são garantidos pela FK composta de 000001; a
-- exclusividade da compensação (DEC-001), pelo índice parcial. Aqui: tipo da referência,
-- estado PROCESSED, jogador, moeda, rodada e, nas reversões, valor integral.
CREATE FUNCTION check_reference_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    ref wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO ref FROM wager_transactions WHERE id = NEW.reference_transaction_id;

    IF NOT FOUND OR ref.status <> 'PROCESSED' THEN
        RAISE EXCEPTION 'transaction % references % which is not PROCESSED', NEW.id, NEW.reference_transaction_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transaction_reference_consistency';
    END IF;

    -- WIN e REFUND referenciam uma BET; ROLLBACK, uma BET, WIN ou REFUND.
    IF NOT ((NEW.kind IN ('WIN', 'REFUND') AND ref.kind = 'BET')
         OR (NEW.kind = 'ROLLBACK' AND ref.kind IN ('BET', 'WIN', 'REFUND'))) THEN
        RAISE EXCEPTION '% transaction % cannot reference a % transaction', NEW.kind, NEW.id, ref.kind
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transaction_reference_consistency';
    END IF;

    IF ref.player_id <> NEW.player_id OR ref.currency <> NEW.currency
       OR ref.round_id IS DISTINCT FROM NEW.round_id THEN
        RAISE EXCEPTION 'transaction % does not match the player, currency and round of its reference', NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transaction_reference_consistency';
    END IF;

    -- WIN pode ter valor próprio; REFUND e ROLLBACK são sempre integrais.
    IF NEW.kind IN ('REFUND', 'ROLLBACK') AND ref.amount <> NEW.amount THEN
        RAISE EXCEPTION '% transaction % amount differs from the referenced amount', NEW.kind, NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transaction_reference_consistency';
    END IF;

    RETURN NULL;
END;
$$;

-- AFTER: as FKs (triggers RI_ConstraintTrigger_*, que disparam antes por ordem de nome) validam
-- a existência e o provedor/carteira da referência primeiro; este trigger valida a coerência.
CREATE TRIGGER wager_transactions_reference_consistency AFTER INSERT OR UPDATE ON wager_transactions
    FOR EACH ROW
    WHEN (NEW.status = 'PROCESSED' AND NEW.reference_transaction_id IS NOT NULL)
    EXECUTE FUNCTION check_reference_consistency();
