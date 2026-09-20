-- Deployment-wide Telegram master key, settable by an owner/admin from the
-- web UI instead of only via the MULTICA_TELEGRAM_SECRET_KEY env var. It is a
-- base64-encoded 32-byte secretbox key that encrypts each per-agent BotFather
-- bot token at rest (telegram_master_config mirrors the "at-rest key" role of
-- MULTICA_TELEGRAM_SECRET_KEY, but is operator-managed through the app instead
-- of the process environment).
--
-- Single-row table: id is always TRUE, enforced by the PK so at most one row
-- can ever exist. No secondary indexes are needed, so no CONCURRENTLY index
-- builds. No FKs / cascades (repo hard rule) — updated_by is informational and
-- ownership is validated in the application layer.
CREATE TABLE telegram_master_config (
    id                BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    secret_key_base64 TEXT NOT NULL CHECK (secret_key_base64 <> ''),
    updated_by        UUID NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);