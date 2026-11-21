-- +goose Up
-- Follows 00006: the notification worker depends on short_links.active_until
-- and short_links.visits_cycle supplied by that migration.
CREATE TABLE notification_settings (
    user_id uuid PRIMARY KEY REFERENCES users(user_id) ON DELETE CASCADE,
    expiry_enabled boolean NOT NULL DEFAULT true,
    visits_enabled boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notifications (
    notification_id uuid PRIMARY KEY,
    recipient_user_id uuid NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces(workspace_id) ON DELETE CASCADE,
    link_id uuid REFERENCES short_links(link_id) ON DELETE SET NULL,
    kind varchar(24) NOT NULL CHECK (kind IN ('expiry_24h', 'visits_80')),
    title text NOT NULL,
    summary text NOT NULL,
    dedupe_key varchar(255) NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    read_at timestamptz
);
CREATE INDEX notifications_recipient_created_idx ON notifications (recipient_user_id, created_at DESC, notification_id DESC);
CREATE INDEX notifications_recipient_unread_idx ON notifications (recipient_user_id, created_at DESC, notification_id DESC) WHERE read_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS notification_settings;
