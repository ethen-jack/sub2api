ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS output_tokens_estimated BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN usage_logs.output_tokens_estimated IS
    'Output tokens include a local estimate because final upstream usage was unavailable; false on historical rows does not prove complete usage';
