-- Inbox do consumidor SQS. A linha é gravada na MESMA transação dos efeitos financeiros da
-- mensagem: ou existem as duas coisas, ou nenhuma. Por isso não há estado "recebido"
-- confirmado: a existência da linha é a conclusão durável do tratamento. Uma reentrega com o
-- mesmo (consumer_name, message_id) e o mesmo hash é duplicata; com outro hash, conflito.
CREATE TABLE inbox_messages (
    consumer_name  text        NOT NULL,
    message_id     text        NOT NULL,
    -- SHA-256 do conteúdo canônico da mensagem (tipo, chave de idempotência e operação).
    payload_hash   bytea       NOT NULL,
    transaction_id uuid        NOT NULL REFERENCES wager_transactions (id),
    -- Estado da operação quando a mensagem foi concluída; uma pendência pode terminar depois.
    outcome        text        NOT NULL,
    received_at    timestamptz NOT NULL,
    completed_at   timestamptz NOT NULL,

    CONSTRAINT inbox_messages_pkey PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_consumer_not_empty CHECK (consumer_name <> ''),
    CONSTRAINT inbox_message_id_not_empty CHECK (message_id <> ''),
    CONSTRAINT inbox_payload_hash_size CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT inbox_outcome_valid CHECK (outcome IN ('PROCESSED', 'REJECTED', 'PENDING_REFERENCE'))
);

CREATE TRIGGER inbox_messages_forbid_update BEFORE UPDATE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER inbox_messages_forbid_delete BEFORE DELETE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER inbox_messages_forbid_truncate BEFORE TRUNCATE ON inbox_messages
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Publicador da outbox: um evento só sai depois de todos os anteriores do mesmo grupo
-- (carteira). Este índice responde "há predecessor ainda não publicado?" sem varrer a tabela.
CREATE INDEX outbox_events_unpublished_by_group
    ON outbox_events (message_group_id, sequence) WHERE published_at IS NULL;
