-- Reversão de 000004: remove o índice da outbox e a inbox (destrutivo para a inbox).
DROP INDEX outbox_events_unpublished_by_group;
DROP TABLE inbox_messages;
