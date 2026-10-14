-- Esquema financeiro: carteiras, transações de apostas e ledger append-only.
-- Valores monetários são BIGINT em unidades mínimas (centavos); a moeda é sempre explícita.
--
-- CHECKs aceitam resultado UNKNOWN (NULL). As expressões com colunas anuláveis usam
-- IS [NOT] NULL e COALESCE(..., false) para que NULL nunca valide uma combinação inválida.
-- O PostgreSQL avalia CHECKs em ordem alfabética de nome; os nomes abaixo são estáveis
-- porque os testes verificam qual regra rejeitou a linha.

-- Imutabilidade: bloqueia o comando, independentemente do papel da aplicação.
CREATE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;

-- Carteira ---------------------------------------------------------------------

CREATE TABLE wallets (
    id         uuid        PRIMARY KEY,
    player_id  uuid        NOT NULL,
    currency   text        NOT NULL,
    balance    bigint      NOT NULL,
    version    bigint      NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT wallets_player_currency_key    UNIQUE (player_id, currency),
    CONSTRAINT wallets_currency_format        CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_balance_non_negative   CHECK (balance >= 0),
    CONSTRAINT wallets_version_positive       CHECK (version >= 1),
    -- Alvo da FK composta de wager_transactions: transação, carteira, jogador e moeda concordam.
    CONSTRAINT wallets_id_player_currency_key UNIQUE (id, player_id, currency)
);

-- Identidade imutável; a versão sobe exatamente em 1 quando (e somente quando) o saldo muda.
-- A segunda regra também recusa um escritor que leu uma versão antiga e gravou depois de outro
-- commit; a garantia principal contra lost update continua sendo SELECT ... FOR UPDATE.
CREATE FUNCTION wallets_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.player_id <> OLD.player_id
       OR NEW.currency <> OLD.currency OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'wallet identity is immutable'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wallets_identity_immutable';
    END IF;
    IF NOT ((NEW.balance <> OLD.balance AND NEW.version = OLD.version + 1)
         OR (NEW.balance = OLD.balance AND NEW.version = OLD.version)) THEN
        RAISE EXCEPTION 'wallet version must advance by one with each balance change'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_version_follows_balance';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallets_guard_update BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard_update();
CREATE TRIGGER wallets_forbid_delete BEFORE DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER wallets_forbid_truncate BEFORE TRUNCATE ON wallets
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Transações -------------------------------------------------------------------

-- Operações internas (OPENING) e externas na mesma tabela. A idempotência é isolada por
-- provedor pelas UNIQUEs; a abertura sem metadados externos é imposta por wager_tx_origin_fields.
CREATE TABLE wager_transactions (
    id                                uuid        PRIMARY KEY,
    kind                              text        NOT NULL,
    status                            text        NOT NULL,
    wallet_id                         uuid,
    player_id                         uuid        NOT NULL,
    amount                            bigint      NOT NULL,
    currency                          text        NOT NULL,
    provider_id                       text,
    external_transaction_id           text,
    idempotency_key                   text,
    payload_hash                      bytea,
    round_id                          text,
    game_id                           text,
    reference_external_transaction_id text,
    reference_transaction_id          uuid,
    failure_code                      text,
    result_balance                    bigint,
    reference_attempts                integer     NOT NULL DEFAULT 0,
    next_attempt_at                   timestamptz,
    created_at                        timestamptz NOT NULL,
    updated_at                        timestamptz NOT NULL,

    CONSTRAINT wager_tx_kind_valid CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wager_tx_state_allowed CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    CONSTRAINT wager_tx_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    -- LOSS não movimenta (zero); todo o resto exige valor positivo.
    CONSTRAINT wager_tx_amount_policy CHECK (
        (kind = 'LOSS' AND amount = 0) OR (kind <> 'LOSS' AND amount > 0)
    ),
    CONSTRAINT wager_tx_payload_hash_size CHECK (payload_hash IS NULL OR octet_length(payload_hash) = 32),
    CONSTRAINT wager_tx_result_balance_non_negative CHECK (result_balance IS NULL OR result_balance >= 0),
    CONSTRAINT wager_tx_attempts_non_negative CHECK (reference_attempts >= 0),
    -- Abertura crédito positivo só existe processada; saldo zero dispensa OPENING.
    CONSTRAINT wager_tx_opening_processed CHECK (kind <> 'OPENING' OR status = 'PROCESSED'),
    -- OPENING não carrega metadados externos; operação externa carrega todos, não vazios.
    CONSTRAINT wager_tx_origin_fields CHECK (COALESCE(
        CASE WHEN kind = 'OPENING' THEN
            provider_id IS NULL AND external_transaction_id IS NULL AND idempotency_key IS NULL
            AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
        ELSE
            provider_id IS NOT NULL AND provider_id <> ''
            AND external_transaction_id IS NOT NULL AND external_transaction_id <> ''
            AND idempotency_key IS NOT NULL AND idempotency_key <> ''
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND round_id <> ''
            AND game_id IS NOT NULL AND game_id <> ''
        END, false)),
    CONSTRAINT wager_tx_reference_policy CHECK (COALESCE(
        CASE kind
            WHEN 'REFUND'   THEN reference_external_transaction_id IS NOT NULL AND reference_external_transaction_id <> ''
            WHEN 'ROLLBACK' THEN reference_external_transaction_id IS NOT NULL AND reference_external_transaction_id <> ''
            WHEN 'WIN'      THEN reference_external_transaction_id IS NULL OR reference_external_transaction_id <> ''
            WHEN 'BET'      THEN reference_external_transaction_id IS NULL
            WHEN 'LOSS'     THEN reference_external_transaction_id IS NULL
            WHEN 'OPENING'  THEN reference_external_transaction_id IS NULL
            ELSE false
        END, false)),
    -- Campos obrigatórios de cada estado. PENDING_REFERENCE só para quem tem referência externa (D6).
    CONSTRAINT wager_tx_state_fields CHECK (COALESCE(
        CASE status
            WHEN 'PENDING' THEN
                wallet_id IS NOT NULL AND failure_code IS NULL AND result_balance IS NULL
                AND reference_transaction_id IS NULL AND next_attempt_at IS NULL
            WHEN 'PENDING_REFERENCE' THEN
                kind IN ('WIN', 'REFUND', 'ROLLBACK') AND wallet_id IS NOT NULL
                AND reference_external_transaction_id IS NOT NULL AND reference_transaction_id IS NULL
                AND failure_code IS NULL AND result_balance IS NULL AND next_attempt_at IS NOT NULL
            WHEN 'PROCESSED' THEN
                wallet_id IS NOT NULL AND failure_code IS NULL AND result_balance IS NOT NULL
                AND next_attempt_at IS NULL
                AND (reference_external_transaction_id IS NULL) = (reference_transaction_id IS NULL)
            ELSE
                failure_code IS NOT NULL AND failure_code <> '' AND next_attempt_at IS NULL
        END, false)),

    CONSTRAINT wager_tx_provider_external_key    UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_tx_provider_idempotency_key UNIQUE (provider_id, idempotency_key),
    -- Alvo da FK abaixo: a referência resolvida precisa ser do mesmo provedor e da mesma carteira.
    CONSTRAINT wager_tx_id_provider_wallet_key   UNIQUE (id, provider_id, wallet_id),
    CONSTRAINT wager_tx_reference_same_provider_wallet_fkey
        FOREIGN KEY (reference_transaction_id, provider_id, wallet_id)
        REFERENCES wager_transactions (id, provider_id, wallet_id)
,
    -- Quando a carteira existe, jogador e moeda da transação são os dela. wallet_id nulo
    -- (rejeição por carteira inexistente) não é verificado: MATCH SIMPLE.
    CONSTRAINT wager_tx_wallet_owner_currency_fkey
        FOREIGN KEY (wallet_id, player_id, currency)
        REFERENCES wallets (id, player_id, currency)
);

-- Uma abertura por carteira.
CREATE UNIQUE INDEX wager_tx_one_opening_per_wallet
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- DEC-001: no máximo uma compensação direta bem-sucedida por transação referenciada.
-- A exclusividade é histórica: ROLLBACK(REFUND) referencia o REFUND, não a BET, e o REFUND
-- permanece PROCESSED; logo ROLLBACK(BET) continua bloqueado. Compensações rejeitadas ou
-- pendentes não ocupam a vaga.
CREATE UNIQUE INDEX wager_tx_one_compensation_per_reference
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Fila durável do worker de referências pendentes.
CREATE INDEX wager_tx_pending_reference_due
    ON wager_transactions (next_attempt_at) WHERE status = 'PENDING_REFERENCE';

-- Estados terminais e a identidade da operação não mudam; transações nunca são removidas.
CREATE FUNCTION wager_transactions_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'terminal transaction % is immutable', OLD.id
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_terminal_immutable';
    END IF;
    IF NEW.id <> OLD.id
       OR NEW.kind <> OLD.kind
       OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
       OR NEW.player_id <> OLD.player_id
       OR NEW.amount <> OLD.amount
       OR NEW.currency <> OLD.currency
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.round_id IS DISTINCT FROM OLD.round_id
       OR NEW.game_id IS DISTINCT FROM OLD.game_id
       OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'transaction identity is immutable'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_identity_immutable';
    END IF;
    IF NEW.status = 'PENDING' AND OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'transaction cannot return to PENDING'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_invalid_transition';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transactions_guard_update BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard_update();
CREATE TRIGGER wager_transactions_forbid_delete BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER wager_transactions_forbid_truncate BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Ledger -----------------------------------------------------------------------

CREATE TABLE wallet_ledger_entries (
    id             uuid        PRIMARY KEY,
    wallet_id      uuid        NOT NULL REFERENCES wallets (id),
    transaction_id uuid        NOT NULL REFERENCES wager_transactions (id),
    direction      text        NOT NULL,
    amount         bigint      NOT NULL,
    balance_before bigint      NOT NULL,
    balance_after  bigint      NOT NULL,
    wallet_version bigint      NOT NULL,
    created_at     timestamptz NOT NULL,

    CONSTRAINT ledger_direction_valid CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_amount_positive CHECK (amount > 0),
    CONSTRAINT ledger_balances_non_negative CHECK (balance_before >= 0 AND balance_after >= 0),
    CONSTRAINT ledger_version_positive CHECK (wallet_version >= 1),
    -- Overflow de BIGINT aborta com 22003 em vez de dar a volta.
    CONSTRAINT ledger_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after = balance_before + amount)
        OR (direction = 'DEBIT' AND balance_after = balance_before - amount)
    ),
    CONSTRAINT ledger_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    -- Defesa em profundidade e cursor estável de paginação; não substitui o lock da carteira.
    CONSTRAINT ledger_wallet_version_key UNIQUE (wallet_id, wallet_version)
);

CREATE TRIGGER ledger_forbid_update BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_forbid_delete BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_forbid_truncate BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Correspondência carteira <-> transação <-> ledger, verificada no COMMIT (DEFERRED) para que
-- a ordem dos comandos dentro da transação não importe e vários movimentos possam coexistir.
-- Cada trigger cobre um vínculo; a regra de uma carteira vale quando qualquer lado muda.

-- (1) Lançamento -> transação e encadeamento. A transação existe, está PROCESSED, não é LOSS,
-- é da mesma carteira e valor, a direção é a do tipo, e o lançamento encadeia no saldo posterior
-- do anterior. A versão 1 é exclusivamente a OPENING; uma carteira aberta com saldo zero não
-- tem OPENING e seu primeiro movimento é a versão 2. Moeda e jogador já são garantidos pela FK
-- composta de wager_transactions.
CREATE FUNCTION check_ledger_entry_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx               wager_transactions%ROWTYPE;
    reference_kind   text;
    want_direction   text;
    previous_after   bigint;
    previous_version bigint;
BEGIN
    SELECT * INTO tx FROM wager_transactions WHERE id = NEW.transaction_id;

    IF tx.status <> 'PROCESSED' OR tx.kind = 'LOSS'
       OR tx.wallet_id <> NEW.wallet_id OR tx.amount <> NEW.amount THEN
        RAISE EXCEPTION 'ledger entry % does not match its transaction', NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_consistency';
    END IF;

    IF (NEW.wallet_version = 1) <> (tx.kind = 'OPENING') THEN
        RAISE EXCEPTION 'ledger entry % (version %) for a % transaction: only OPENING is version 1',
            NEW.id, NEW.wallet_version, tx.kind
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_consistency';
    END IF;

    IF tx.kind = 'BET' THEN
        want_direction := 'DEBIT';
    ELSIF tx.kind = 'ROLLBACK' THEN
        -- Movimento contrário ao original: desfaz débito de BET com crédito; desfaz WIN/REFUND com débito.
        SELECT kind INTO reference_kind FROM wager_transactions WHERE id = tx.reference_transaction_id;
        want_direction := CASE WHEN reference_kind = 'BET' THEN 'CREDIT' ELSE 'DEBIT' END;
    ELSE
        want_direction := 'CREDIT';
    END IF;
    IF NEW.direction <> want_direction THEN
        RAISE EXCEPTION 'ledger entry % has direction % but % requires %', NEW.id, NEW.direction, tx.kind, want_direction
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_consistency';
    END IF;

    SELECT balance_after, wallet_version INTO previous_after, previous_version
      FROM wallet_ledger_entries
     WHERE wallet_id = NEW.wallet_id AND wallet_version < NEW.wallet_version
     ORDER BY wallet_version DESC
     LIMIT 1;
    IF NOT FOUND THEN
        IF NEW.balance_before <> 0 OR NEW.wallet_version > 2 THEN
            RAISE EXCEPTION 'first ledger entry of wallet % must start from zero at version 1 or 2', NEW.wallet_id
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_consistency';
        END IF;
    ELSIF NEW.balance_before <> previous_after OR NEW.wallet_version <> previous_version + 1 THEN
        RAISE EXCEPTION 'ledger entry % does not follow the previous balance and version', NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_consistency';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_consistency AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_ledger_entry_consistency();

-- (2) Carteira <-> ledger, nos dois sentidos: ao final da transação, saldo e versão da carteira
-- são exatamente os do último lançamento (ou zero/versão 1 sem lançamentos). Disparado por
-- INSERT/UPDATE da carteira e por INSERT de lançamento, de modo que nem alterar o saldo sem
-- lançamento nem inserir só o lançamento é confirmável.
CREATE FUNCTION check_wallet_matches_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_wallet   uuid;
    current_balance bigint;
    current_version bigint;
    last_after      bigint;
    last_version    bigint;
BEGIN
    IF TG_TABLE_NAME = 'wallets' THEN
        target_wallet := NEW.id;
    ELSE
        target_wallet := NEW.wallet_id;
    END IF;

    SELECT balance, version INTO current_balance, current_version FROM wallets WHERE id = target_wallet;

    SELECT balance_after, wallet_version INTO last_after, last_version
      FROM wallet_ledger_entries
     WHERE wallet_id = target_wallet
     ORDER BY wallet_version DESC
     LIMIT 1;
    IF NOT FOUND THEN
        last_after := 0;
        last_version := 1;
    END IF;

    IF current_balance <> last_after OR current_version <> last_version THEN
        RAISE EXCEPTION 'wallet % (balance %, version %) does not match its ledger (balance %, version %)',
            target_wallet, current_balance, current_version, last_after, last_version
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallet_matches_ledger';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wallets_match_ledger AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_wallet_matches_ledger();
CREATE CONSTRAINT TRIGGER ledger_entries_match_wallet AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_wallet_matches_ledger();

-- (3) Transação -> lançamento. Toda transação PROCESSED que movimenta dinheiro (tudo menos LOSS)
-- tem o lançamento correspondente, e o result_balance devolvido ao provedor é o balance_after
-- dele. Há no máximo um lançamento por (carteira, transação) pela UNIQUE do ledger. Como a
-- transação terminal é imutável, o result_balance não diverge depois do commit (replay histórico).
-- O WHEN dispensa o disparo de estados não finais, que ainda não movimentaram nada.
CREATE FUNCTION check_transaction_matches_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entry_after bigint;
BEGIN
    SELECT balance_after INTO entry_after
      FROM wallet_ledger_entries
     WHERE wallet_id = NEW.wallet_id AND transaction_id = NEW.id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'processed % transaction % has no ledger entry', NEW.kind, NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transaction_matches_ledger';
    END IF;
    IF entry_after <> NEW.result_balance THEN
        RAISE EXCEPTION 'transaction % result balance % differs from ledger balance %',
            NEW.id, NEW.result_balance, entry_after
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transaction_matches_ledger';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wager_transactions_match_ledger AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.status = 'PROCESSED' AND NEW.kind <> 'LOSS')
    EXECUTE FUNCTION check_transaction_matches_ledger();
