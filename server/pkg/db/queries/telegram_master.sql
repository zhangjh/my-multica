-- Deployment-wide Telegram master key (single row, see migration 468).
-- The value is the base64-encoded 32-byte secretbox at-rest key that
-- encrypts every per-agent bot token. It is operator-managed through the
-- admin settings UI; the boot path falls back to MULTICA_TELEGRAM_SECRET_KEY
-- when no row exists.

-- name: GetTelegramMasterConfig :one
SELECT secret_key_base64, updated_by, updated_at
FROM telegram_master_config
WHERE id = TRUE;

-- name: UpsertTelegramMasterConfig :one
INSERT INTO telegram_master_config (id, secret_key_base64, updated_by)
VALUES (TRUE, $1, $2)
ON CONFLICT (id) DO UPDATE SET
    secret_key_base64 = EXCLUDED.secret_key_base64,
    updated_by        = EXCLUDED.updated_by,
    updated_at        = now()
RETURNING *;

-- name: DeleteTelegramMasterConfig :exec
DELETE FROM telegram_master_config
WHERE id = TRUE;