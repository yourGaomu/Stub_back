package auth

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

var defaultPermissionCodes = []string{
	"links.read", "links.create_url", "links.create_asset", "links.manage", "links.reset_visits",
	"assets.read", "assets.upload", "assets.delete", "assets.restore",
	"templates.read_use", "templates.manage", "invites.create_revoke", "logs.read_own", "api_keys.manage",
}

func createDefaultUserPermissions(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	// 1. 注册事务中为新普通用户写入固定权限目录，缺失记录不会被视为授权。
	for _, code := range defaultPermissionCodes {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_permissions (user_id, permission_code, granted_at)
			VALUES ($1, $2, now())`, userID, code); err != nil {
			return fmt.Errorf("初始化用户权限 %s 失败: %w", code, err)
		}
	}
	return nil
}
