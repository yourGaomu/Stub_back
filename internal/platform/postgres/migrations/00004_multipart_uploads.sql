-- +goose Up
ALTER TABLE users ADD COLUMN storage_reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (storage_reserved_bytes >= 0);

CREATE TABLE asset_upload_sessions (
    upload_session_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(workspace_id),
    owner_user_id uuid NOT NULL REFERENCES users(user_id),
    business_id varchar(64) NOT NULL REFERENCES business_integrations(business_id),
    api_key_id uuid REFERENCES api_keys(api_key_id),
    api_quota_reserved boolean NOT NULL DEFAULT false,
    bucket varchar(63) NOT NULL,
    object_key text NOT NULL,
    minio_upload_id text NOT NULL,
    original_name text NOT NULL,
    content_type varchar(255) NOT NULL,
    logical_path text NOT NULL,
    expected_size_bytes bigint NOT NULL CHECK (expected_size_bytes > 0),
    part_size_bytes bigint NOT NULL CHECK (part_size_bytes > 0),
    part_count integer NOT NULL CHECK (part_count > 0),
    reserved_bytes bigint NOT NULL CHECK (reserved_bytes > 0),
    status varchar(24) NOT NULL CHECK (status IN ('initializing', 'uploading', 'completing', 'completed', 'cleanup_pending', 'cancelled', 'expired', 'failed')),
    idempotency_key varchar(128),
    asset_id uuid REFERENCES assets(asset_id),
    expires_at timestamptz NOT NULL,
    completed_at timestamptz,
    cancelled_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (bucket, object_key)
);
CREATE UNIQUE INDEX asset_upload_sessions_idempotency_uq
    ON asset_upload_sessions(owner_user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status IN ('initializing', 'uploading', 'completing');
CREATE INDEX asset_upload_sessions_owner_active_idx ON asset_upload_sessions(owner_user_id, status, expires_at);

CREATE TABLE asset_upload_parts (
    upload_session_id uuid NOT NULL REFERENCES asset_upload_sessions(upload_session_id) ON DELETE CASCADE,
    part_number integer NOT NULL CHECK (part_number > 0),
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    etag text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (upload_session_id, part_number)
);

-- +goose Down
DROP TABLE asset_upload_parts;
DROP TABLE asset_upload_sessions;
ALTER TABLE users DROP COLUMN storage_reserved_bytes;
