-- +goose Up
CREATE TABLE link_groups (
    group_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(workspace_id),
    name varchar(120) NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 1000),
    created_by uuid NOT NULL REFERENCES users(user_id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, workspace_id)
);
CREATE UNIQUE INDEX link_groups_workspace_name_uq ON link_groups (workspace_id, lower(name));
CREATE INDEX link_groups_workspace_updated_idx ON link_groups (workspace_id, updated_at DESC, group_id DESC);

ALTER TABLE short_links ADD COLUMN group_id uuid;
-- Membership is cleared explicitly in the deletion transaction. A composite
-- SET NULL would also clear the non-null workspace_id on older PostgreSQL.
ALTER TABLE short_links ADD CONSTRAINT short_links_group_workspace_fk
    FOREIGN KEY (group_id, workspace_id) REFERENCES link_groups(group_id, workspace_id);
CREATE INDEX short_links_workspace_group_created_idx ON short_links (workspace_id, group_id, created_at DESC, link_id DESC);

-- +goose Down
ALTER TABLE short_links DROP CONSTRAINT IF EXISTS short_links_group_workspace_fk;
DROP INDEX IF EXISTS short_links_workspace_group_created_idx;
ALTER TABLE short_links DROP COLUMN IF EXISTS group_id;
DROP TABLE IF EXISTS link_groups;
