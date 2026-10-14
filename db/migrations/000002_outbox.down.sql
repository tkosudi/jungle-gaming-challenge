-- Reverte apenas a outbox; o schema financeiro de 000001 permanece intacto.
-- Destrutiva: apaga os eventos ainda não publicados.
DROP TABLE outbox_events;
DROP FUNCTION outbox_events_guard_update();
