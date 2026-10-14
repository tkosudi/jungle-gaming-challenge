-- Transactional outbox: o evento é gravado no mesmo commit da operação financeira; um publicador
-- futuro lê as linhas não publicadas. Nada é enviado ao broker dentro da transação de negócio.
--
-- payload é o envelope completo do evento (eventId, eventType, aggregateId, correlationId,
-- occurredAt, version, data), um snapshot imutável. message_group_id e event_id são o
-- MessageGroupId e o MessageDeduplicationId da fila FIFO.
CREATE TABLE outbox_events (
    event_id         uuid        PRIMARY KEY,
    event_type       text        NOT NULL,
    schema_version   integer     NOT NULL,
    aggregate_id     uuid        NOT NULL,
    message_group_id text        NOT NULL,
    transaction_id   uuid        NOT NULL REFERENCES wager_transactions (id),
    payload          jsonb       NOT NULL,
    occurred_at      timestamptz NOT NULL,
    -- Ordem de publicação dentro do grupo: atribuída com o lock da carteira, logo monotônica por carteira.
    sequence         bigint      GENERATED ALWAYS AS IDENTITY,
    attempts         integer     NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL,
    published_at     timestamptz,
    created_at       timestamptz NOT NULL,

    CONSTRAINT outbox_events_type_valid CHECK (event_type IN (
        'WagerTransactionProcessed', 'WagerTransactionRejected',
        'WalletBalanceChanged', 'WagerTransactionPendingReference')),
    CONSTRAINT outbox_events_schema_version_positive CHECK (schema_version >= 1),
    CONSTRAINT outbox_events_group_not_empty CHECK (message_group_id <> ''),
    CONSTRAINT outbox_events_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_events_attempts_non_negative CHECK (attempts >= 0),
    -- No máximo um evento de cada tipo por transação: um replay não consegue duplicá-lo.
    CONSTRAINT outbox_events_transaction_type_key UNIQUE (transaction_id, event_type)
);

-- Fila de publicação: apenas o que ainda não foi publicado, na ordem de tentativa e sequência.
CREATE INDEX outbox_events_unpublished
    ON outbox_events (next_attempt_at, sequence) WHERE published_at IS NULL;

-- Só o estado de publicação muda; identidade e conteúdo do evento são imutáveis.
CREATE FUNCTION outbox_events_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.event_id <> OLD.event_id
       OR NEW.event_type <> OLD.event_type
       OR NEW.schema_version <> OLD.schema_version
       OR NEW.aggregate_id <> OLD.aggregate_id
       OR NEW.message_group_id <> OLD.message_group_id
       OR NEW.transaction_id <> OLD.transaction_id
       OR NEW.payload <> OLD.payload
       OR NEW.occurred_at <> OLD.occurred_at
       OR NEW.sequence <> OLD.sequence
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'outbox event % content is immutable', OLD.event_id
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'outbox_events_content_immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_events_guard_update BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard_update();
