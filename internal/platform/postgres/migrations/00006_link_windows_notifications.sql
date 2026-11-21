-- +goose Up
ALTER TABLE short_links
    ADD COLUMN active_from timestamptz,
    ADD COLUMN active_until timestamptz,
    ADD COLUMN visits_cycle bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT short_links_window_order_check CHECK (
        active_from IS NULL OR (
            (active_until IS NULL OR active_from < active_until) AND
            (expires_at IS NULL OR active_from < expires_at)
        )
    );

-- +goose Down
ALTER TABLE short_links
    DROP CONSTRAINT IF EXISTS short_links_window_order_check,
    DROP COLUMN IF EXISTS visits_cycle,
    DROP COLUMN IF EXISTS active_until,
    DROP COLUMN IF EXISTS active_from;
