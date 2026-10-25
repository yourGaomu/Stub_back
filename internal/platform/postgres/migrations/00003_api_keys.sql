-- +goose Up
-- 1. 为每个用户保存 API Key 数量上限；管理员可通过用户额度接口调整。
ALTER TABLE users
    ADD COLUMN api_key_quota integer NOT NULL DEFAULT 20 CHECK (api_key_quota >= 0);

-- 1.1 已存在的普通用户补齐 API Key 管理权限；新用户仍由注册事务初始化。
INSERT INTO user_permissions(user_id, permission_code, granted_at)
SELECT user_id, 'api_keys.manage', now()
FROM users
WHERE role='user'
ON CONFLICT (user_id, permission_code) DO NOTHING;

-- 1.1 URL 短链也保存业务上下文；人类会话创建的旧短链允许为空。
ALTER TABLE short_links
    ADD COLUMN business_id varchar(64) REFERENCES business_integrations(business_id);
CREATE INDEX short_links_business_idx ON short_links(business_id, created_at DESC);

-- 2. 机器调用凭证主表；每把 Key 强制绑定一个已登记业务。
CREATE TABLE api_keys (
    api_key_id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces(workspace_id) ON DELETE CASCADE,
    business_id varchar(64) NOT NULL REFERENCES business_integrations(business_id),
    name varchar(80) NOT NULL,
    description varchar(255) NOT NULL DEFAULT '',
    key_prefix varchar(32) NOT NULL,
    key_hash char(64) NOT NULL UNIQUE,
    status varchar(16) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked')),
    allow_account_access boolean NOT NULL DEFAULT false,
    allowed_ip_cidrs cidr[] NOT NULL DEFAULT '{}',
    rate_limit_per_min integer NOT NULL DEFAULT 60 CHECK (rate_limit_per_min BETWEEN 1 AND 600),
    max_upload_bytes bigint CHECK (max_upload_bytes IS NULL OR max_upload_bytes >= 0),
    uploaded_bytes_used bigint NOT NULL DEFAULT 0 CHECK (uploaded_bytes_used >= 0),
    max_created_links integer CHECK (max_created_links IS NULL OR max_created_links >= 0),
    created_links_used integer NOT NULL DEFAULT 0 CHECK (created_links_used >= 0),
    max_requests bigint CHECK (max_requests IS NULL OR max_requests >= 0),
    requests_used bigint NOT NULL DEFAULT 0 CHECK (requests_used >= 0),
    expires_at timestamptz,
    last_used_at timestamptz,
    last_used_ip inet,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_user_status_idx ON api_keys(user_id, status);
CREATE INDEX api_keys_business_idx ON api_keys(business_id, status);

-- 3. API Key 可用功能权限；实际生效权限还要与用户当前权限取交集。
CREATE TABLE api_key_permissions (
    api_key_id uuid NOT NULL REFERENCES api_keys(api_key_id) ON DELETE CASCADE,
    permission_code varchar(64) NOT NULL,
    PRIMARY KEY (api_key_id, permission_code)
);

-- +goose Down
DELETE FROM user_permissions WHERE permission_code='api_keys.manage' AND granted_by IS NULL;
DROP TABLE IF EXISTS api_key_permissions;
DROP TABLE IF EXISTS api_keys;
DROP INDEX IF EXISTS short_links_business_idx;
ALTER TABLE short_links DROP COLUMN IF EXISTS business_id;
ALTER TABLE users DROP COLUMN IF EXISTS api_key_quota;
