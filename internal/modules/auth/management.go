package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type PermissionCatalogItem struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type PermissionState struct {
	Code      string     `json:"code"`
	Enabled   bool       `json:"enabled"`
	GrantedAt *time.Time `json:"grantedAt"`
}

type PermissionsResponse struct {
	IsAdmin bool              `json:"isAdmin"`
	Items   []PermissionState `json:"items"`
}

type UserPermissionsResponse struct {
	UserID  string            `json:"userId"`
	IsAdmin bool              `json:"isAdmin"`
	Items   []PermissionState `json:"items"`
}

type PermissionUpdateResponse struct {
	UserID    string    `json:"userId"`
	Code      string    `json:"code"`
	Enabled   bool      `json:"enabled"`
	ChangedAt time.Time `json:"changedAt"`
}

type RegistrationSettings struct {
	AllowPublicRegistration bool      `json:"allowPublicRegistration"`
	AllowInviteRegistration bool      `json:"allowInviteRegistration"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

type RegistrationSettingsPatch struct {
	AllowPublicRegistration *bool `json:"allowPublicRegistration,omitempty"`
	AllowInviteRegistration *bool `json:"allowInviteRegistration,omitempty"`
}

type AdminUserItem struct {
	UserID              string    `json:"userId"`
	Email               string    `json:"email"`
	Role                string    `json:"role"`
	InvitedByUserID     *string   `json:"invitedByUserId"`
	EmailVerifiedAt     time.Time `json:"emailVerifiedAt"`
	CreatedAt           time.Time `json:"createdAt"`
	DefaultWorkspaceID  string    `json:"defaultWorkspaceId"`
	StorageQuotaBytes   int64     `json:"storageQuotaBytes"`
	StorageUsedBytes    int64     `json:"storageUsedBytes"`
	APIKeyQuota         int       `json:"apiKeyQuota"`
	URLLinkQuota        int       `json:"urlLinkQuota"`
	URLLinkUsed         int64     `json:"urlLinkUsed"`
	AssetLinkQuota      int       `json:"minioAssetLinkQuota"`
	AssetLinkUsed       int64     `json:"minioAssetLinkUsed"`
	PermissionCount     int       `json:"enabledPermissionCount"`
	BusinessAccessCount int       `json:"businessAccessCount"`
}

type AdminUsersPage struct {
	Items      []AdminUserItem `json:"items"`
	Pagination struct {
		Page     int   `json:"page"`
		PageSize int   `json:"pageSize"`
		Total    int64 `json:"total"`
	} `json:"pagination"`
}

type Quotas struct {
	MaxSingleFileBytes  int64 `json:"maxSingleFileBytes"`
	StorageQuotaBytes   int64 `json:"storageQuotaBytes"`
	StorageUsedBytes    int64 `json:"storageUsedBytes"`
	APIKeyQuota         int   `json:"apiKeyQuota"`
	APIKeyUsed          int64 `json:"apiKeyUsed"`
	URLLinkQuota        *int  `json:"urlLinkQuota"`
	URLLinkUsed         int64 `json:"urlLinkUsed"`
	MinioAssetLinkQuota *int  `json:"minioAssetLinkQuota"`
	MinioAssetLinkUsed  int64 `json:"minioAssetLinkUsed"`
}

type QuotaPatch struct {
	MaxSingleFileBytes  *int64 `json:"maxSingleFileBytes,omitempty"`
	StorageQuotaBytes   *int64 `json:"storageQuotaBytes,omitempty"`
	APIKeyQuota         *int   `json:"apiKeyQuota,omitempty"`
	URLLinkQuota        *int   `json:"urlLinkQuota,omitempty"`
	MinioAssetLinkQuota *int   `json:"minioAssetLinkQuota,omitempty"`
}

type AuditMeta struct {
	SourceIP  string
	UserAgent string
	RequestID string
}

var permissionCatalog = []PermissionCatalogItem{
	{Code: "links.read", Name: "查看短链", Description: "查询本人工作空间内短链列表和详情"},
	{Code: "links.create_url", Name: "创建 URL 短链", Description: "创建 HTTPS URL 目标短链"},
	{Code: "links.create_asset", Name: "创建素材短链", Description: "为本人有权访问的 MinIO 素材创建短链"},
	{Code: "links.manage", Name: "管理短链", Description: "编辑标题、标签和访问规则，续期、轮换短码及启停本人短链"},
	{Code: "links.reset_visits", Name: "重置访问次数", Description: "重置本人短链的访问次数"},
	{Code: "assets.read", Name: "查看素材", Description: "查询、预览或获取本人有权访问的素材"},
	{Code: "assets.upload", Name: "上传素材", Description: "上传 MinIO 素材"},
	{Code: "assets.delete", Name: "删除素材", Description: "将本人素材软删除并移入 7 天恢复期"},
	{Code: "assets.restore", Name: "恢复素材", Description: "在恢复期限内恢复本人已软删除素材"},
	{Code: "templates.read_use", Name: "查看和使用模板", Description: "查看可用模板并在创建短链时引用"},
	{Code: "templates.manage", Name: "管理模板", Description: "创建及维护本人工作空间中的规则模板"},
	{Code: "invites.create_revoke", Name: "管理邀请码", Description: "创建普通用户邀请码及撤销本人未使用的邀请码"},
	{Code: "logs.read_own", Name: "查看本人日志", Description: "查询本人操作或本人资源的访问与操作日志"},
	{Code: "api_keys.manage", Name: "管理 API Key", Description: "创建、查看、编辑、启停和吊销本人的机器调用凭证"},
}

func (s *Service) MyPermissions(ctx context.Context, userID uuid.UUID, role string) (PermissionsResponse, error) {
	// 1. 权限目录由服务端固定维护，普通用户的状态实时从数据库读取。
	items := make([]PermissionState, 0, len(permissionCatalog))
	states := map[string]time.Time{}
	if role != "admin" {
		rows, err := s.db.QueryContext(ctx, `SELECT permission_code,granted_at FROM user_permissions WHERE user_id=$1 AND granted_at IS NOT NULL AND revoked_at IS NULL`, userID)
		if err != nil {
			return PermissionsResponse{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			var granted time.Time
			if err := rows.Scan(&code, &granted); err != nil {
				return PermissionsResponse{}, err
			}
			states[code] = granted
		}
		if err := rows.Err(); err != nil {
			return PermissionsResponse{}, err
		}
	}
	for _, item := range permissionCatalog {
		granted, enabled := states[item.Code]
		state := PermissionState{Code: item.Code, Enabled: role == "admin" || enabled}
		if enabled {
			value := granted.UTC()
			state.GrantedAt = &value
		}
		items = append(items, state)
	}
	return PermissionsResponse{IsAdmin: role == "admin", Items: items}, nil
}

func (s *Service) PermissionCatalog() []PermissionCatalogItem {
	return append([]PermissionCatalogItem(nil), permissionCatalog...)
}

func (s *Service) AdminUserPermissions(ctx context.Context, userID uuid.UUID) (UserPermissionsResponse, error) {
	// 1. 管理端总是返回完整目录，缺失或撤销记录均视为关闭。
	var role string
	if err := s.db.QueryRowContext(ctx, `SELECT role FROM users WHERE user_id=$1`, userID).Scan(&role); errors.Is(err, sql.ErrNoRows) {
		return UserPermissionsResponse{}, problem(404, "user_not_found", "用户不存在")
	} else if err != nil {
		return UserPermissionsResponse{}, err
	}
	if role == "admin" {
		return UserPermissionsResponse{}, problem(409, "permission_target_is_admin", "管理员不参与逐项权限设置")
	}
	my, err := s.MyPermissions(ctx, userID, role)
	if err != nil {
		return UserPermissionsResponse{}, err
	}
	return UserPermissionsResponse{UserID: publicID("usr", userID), Items: my.Items}, nil
}

func (s *Service) SetUserPermission(ctx context.Context, actorID, targetID uuid.UUID, code string, grant bool, meta AuditMeta) (PermissionUpdateResponse, error) {
	// 1. 目标、固定权限代码、权限状态与审计必须在一个事务里完成。
	if !knownPermission(code) {
		return PermissionUpdateResponse{}, problem(422, "unknown_permission_code", "权限代码不在固定目录中")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PermissionUpdateResponse{}, err
	}
	defer tx.Rollback()
	var targetRole string
	var workspace uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT role,default_workspace_id FROM users WHERE user_id=$1 FOR UPDATE`, targetID).Scan(&targetRole, &workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return PermissionUpdateResponse{}, problem(404, "user_not_found", "用户不存在")
	}
	if err != nil {
		return PermissionUpdateResponse{}, err
	}
	if targetRole == "admin" {
		return PermissionUpdateResponse{}, problem(409, "permission_target_is_admin", "管理员不参与逐项权限设置")
	}
	now := time.Now().UTC()
	var query string
	var args []any
	if grant {
		query = `INSERT INTO user_permissions(user_id,permission_code,granted_at,granted_by,revoked_at,revoked_by,updated_at) VALUES($1,$2,$3,$4,NULL,NULL,$3) ON CONFLICT(user_id,permission_code) DO UPDATE SET granted_at=EXCLUDED.granted_at,granted_by=EXCLUDED.granted_by,revoked_at=NULL,revoked_by=NULL,updated_at=EXCLUDED.updated_at`
		args = []any{targetID, code, now, actorID}
	} else {
		query = `INSERT INTO user_permissions(user_id,permission_code,granted_at,granted_by,revoked_at,revoked_by,updated_at) VALUES($1,$2,NULL,NULL,$3,$4,$3) ON CONFLICT(user_id,permission_code) DO UPDATE SET granted_at=NULL,granted_by=NULL,revoked_at=EXCLUDED.revoked_at,revoked_by=EXCLUDED.revoked_by,updated_at=EXCLUDED.updated_at`
		args = []any{targetID, code, now, actorID}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return PermissionUpdateResponse{}, err
	}
	action := "admin.permission.grant"
	if !grant {
		action = "admin.permission.revoke"
	}
	if err := writeAuditMeta(ctx, tx, &actorID, &workspace, action, "user_permission", targetID.String()+":"+code, "success", meta, map[string]any{
		"permissionCode": code,
		"enabled":        grant,
	}); err != nil {
		return PermissionUpdateResponse{}, err
	}
	if !grant && code == "invites.create_revoke" {
		rows, err := tx.QueryContext(ctx, `UPDATE invite_codes SET revoked_at=$2 WHERE created_by_user_id=$1 AND redeemed_at IS NULL AND revoked_at IS NULL RETURNING invite_code_id`, targetID, now)
		if err != nil {
			return PermissionUpdateResponse{}, err
		}
		ids := make([]uuid.UUID, 0)
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return PermissionUpdateResponse{}, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return PermissionUpdateResponse{}, err
		}
		rows.Close()
		for _, id := range ids {
			if err := writeAuditMeta(ctx, tx, &actorID, &workspace, "admin.invite.revoke_by_permission", "invite", id.String(), "success", meta, map[string]any{
				"permissionCode":      code,
				"revokedByPermission": true,
			}); err != nil {
				return PermissionUpdateResponse{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return PermissionUpdateResponse{}, err
	}
	return PermissionUpdateResponse{UserID: publicID("usr", targetID), Code: code, Enabled: grant, ChangedAt: now}, nil
}

func (s *Service) GetRegistrationSettings(ctx context.Context) (RegistrationSettings, error) {
	var value RegistrationSettings
	err := s.db.QueryRowContext(ctx, `SELECT allow_public_registration,allow_invite_registration,updated_at FROM system_settings WHERE settings_id=1`).Scan(&value.AllowPublicRegistration, &value.AllowInviteRegistration, &value.UpdatedAt)
	value.UpdatedAt = value.UpdatedAt.UTC()
	return value, err
}

func (s *Service) UpdateRegistrationSettings(ctx context.Context, actorID uuid.UUID, input RegistrationSettingsPatch, meta AuditMeta) (RegistrationSettings, error) {
	// 1. 至少更新一个开关；两个开关独立变化并统一写审计。
	if input.AllowPublicRegistration == nil && input.AllowInviteRegistration == nil {
		return RegistrationSettings{}, problem(422, "empty_update", "至少提供一个注册开关")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RegistrationSettings{}, err
	}
	defer tx.Rollback()
	var current RegistrationSettings
	if err := tx.QueryRowContext(ctx, `SELECT allow_public_registration,allow_invite_registration,updated_at FROM system_settings WHERE settings_id=1 FOR UPDATE`).Scan(&current.AllowPublicRegistration, &current.AllowInviteRegistration, &current.UpdatedAt); err != nil {
		return RegistrationSettings{}, err
	}
	previousPublicRegistration := current.AllowPublicRegistration
	previousInviteRegistration := current.AllowInviteRegistration
	if input.AllowPublicRegistration != nil {
		current.AllowPublicRegistration = *input.AllowPublicRegistration
	}
	if input.AllowInviteRegistration != nil {
		current.AllowInviteRegistration = *input.AllowInviteRegistration
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE system_settings SET allow_public_registration=$1,allow_invite_registration=$2,updated_at=$3,updated_by=$4 WHERE settings_id=1`, current.AllowPublicRegistration, current.AllowInviteRegistration, now, actorID); err != nil {
		return RegistrationSettings{}, err
	}
	if err := writeAuditMeta(ctx, tx, &actorID, nil, "admin.registration_settings.update", "system_settings", "1", "success", meta, map[string]any{
		"allowPublicRegistrationBefore": previousPublicRegistration,
		"allowPublicRegistrationAfter":  current.AllowPublicRegistration,
		"allowInviteRegistrationBefore": previousInviteRegistration,
		"allowInviteRegistrationAfter":  current.AllowInviteRegistration,
	}); err != nil {
		return RegistrationSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegistrationSettings{}, err
	}
	current.UpdatedAt = now
	return current, nil
}

func (s *Service) AdminUsers(ctx context.Context, query, role string, page, pageSize int) (AdminUsersPage, error) {
	// 1. 搜索条件固定为邮箱与角色，数据范围仅通过管理员路由授权。
	if page < 1 || pageSize < 1 || pageSize > 100 || len(query) > 320 || (role != "" && role != "user" && role != "admin") {
		return AdminUsersPage{}, problem(422, "invalid_filter", "用户查询参数无效")
	}
	where := []string{}
	args := []any{}
	if strings.TrimSpace(query) != "" {
		args = append(args, "%"+strings.TrimSpace(query)+"%")
		where = append(where, fmt.Sprintf("u.email ILIKE $%d", len(args)))
	}
	if role != "" {
		args = append(args, role)
		where = append(where, fmt.Sprintf("u.role=$%d", len(args)))
	}
	filter := "TRUE"
	if len(where) > 0 {
		filter = strings.Join(where, " AND ")
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users u WHERE `+filter, args...).Scan(&total); err != nil {
		return AdminUsersPage{}, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT u.user_id,u.email,u.role,u.invited_by_user_id,u.email_verified_at,u.created_at,u.default_workspace_id,u.storage_quota_bytes,u.storage_used_bytes,u.api_key_quota,u.url_link_quota,u.minio_asset_link_quota,(SELECT count(*) FROM short_links l WHERE l.created_by=u.user_id AND l.target_type='url'),(SELECT count(*) FROM short_links l WHERE l.created_by=u.user_id AND l.target_type='minio_asset'),(SELECT count(*) FROM user_permissions p WHERE p.user_id=u.user_id AND p.granted_at IS NOT NULL AND p.revoked_at IS NULL),(SELECT count(*) FROM user_business_access b WHERE b.user_id=u.user_id AND b.revoked_at IS NULL) FROM users u WHERE `+filter+fmt.Sprintf(" ORDER BY u.created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return AdminUsersPage{}, err
	}
	defer rows.Close()
	items := make([]AdminUserItem, 0)
	for rows.Next() {
		var id, workspace uuid.UUID
		var invited sql.NullString
		var item AdminUserItem
		if err := rows.Scan(&id, &item.Email, &item.Role, &invited, &item.EmailVerifiedAt, &item.CreatedAt, &workspace, &item.StorageQuotaBytes, &item.StorageUsedBytes, &item.APIKeyQuota, &item.URLLinkQuota, &item.AssetLinkQuota, &item.URLLinkUsed, &item.AssetLinkUsed, &item.PermissionCount, &item.BusinessAccessCount); err != nil {
			return AdminUsersPage{}, err
		}
		item.UserID = publicID("usr", id)
		item.DefaultWorkspaceID = publicID("ws", workspace)
		if invited.Valid {
			value := "usr_" + invited.String
			item.InvitedByUserID = &value
		}
		item.EmailVerifiedAt = item.EmailVerifiedAt.UTC()
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	result := AdminUsersPage{Items: items}
	result.Pagination.Page = page
	result.Pagination.PageSize = pageSize
	result.Pagination.Total = total
	return result, rows.Err()
}

func (s *Service) AdminUser(ctx context.Context, id uuid.UUID) (AdminUserItem, error) {
	var item AdminUserItem
	var invited sql.NullString
	var workspace uuid.UUID
	err := s.db.QueryRowContext(ctx, `SELECT u.user_id,u.email,u.role,u.invited_by_user_id,u.email_verified_at,u.created_at,u.default_workspace_id,u.storage_quota_bytes,u.storage_used_bytes,u.api_key_quota,u.url_link_quota,u.minio_asset_link_quota,(SELECT count(*) FROM short_links l WHERE l.created_by=u.user_id AND l.target_type='url'),(SELECT count(*) FROM short_links l WHERE l.created_by=u.user_id AND l.target_type='minio_asset'),(SELECT count(*) FROM user_permissions p WHERE p.user_id=u.user_id AND p.granted_at IS NOT NULL AND p.revoked_at IS NULL),(SELECT count(*) FROM user_business_access b WHERE b.user_id=u.user_id AND b.revoked_at IS NULL) FROM users u WHERE u.user_id=$1`, id).Scan(&id, &item.Email, &item.Role, &invited, &item.EmailVerifiedAt, &item.CreatedAt, &workspace, &item.StorageQuotaBytes, &item.StorageUsedBytes, &item.APIKeyQuota, &item.URLLinkQuota, &item.AssetLinkQuota, &item.URLLinkUsed, &item.AssetLinkUsed, &item.PermissionCount, &item.BusinessAccessCount)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUserItem{}, problem(404, "user_not_found", "用户不存在")
	}
	if err != nil {
		return AdminUserItem{}, err
	}
	item.UserID = publicID("usr", id)
	item.DefaultWorkspaceID = publicID("ws", workspace)
	item.EmailVerifiedAt = item.EmailVerifiedAt.UTC()
	item.CreatedAt = item.CreatedAt.UTC()
	if invited.Valid {
		value := "usr_" + invited.String
		item.InvitedByUserID = &value
	}
	return item, nil
}

func (s *Service) UserQuotas(ctx context.Context, id uuid.UUID) (Quotas, error) {
	var q Quotas
	var urlQuota, assetQuota int
	err := s.db.QueryRowContext(ctx, `SELECT max_single_file_bytes,storage_quota_bytes,storage_used_bytes,api_key_quota,(SELECT count(*) FROM api_keys k WHERE k.user_id=u.user_id AND k.status IN ('active','disabled')),url_link_quota,minio_asset_link_quota,(SELECT count(*) FROM short_links l WHERE l.created_by=u.user_id AND l.target_type='url'),(SELECT count(*) FROM short_links l WHERE l.created_by=u.user_id AND l.target_type='minio_asset') FROM users u WHERE user_id=$1`, id).Scan(&q.MaxSingleFileBytes, &q.StorageQuotaBytes, &q.StorageUsedBytes, &q.APIKeyQuota, &q.APIKeyUsed, &urlQuota, &assetQuota, &q.URLLinkUsed, &q.MinioAssetLinkUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Quotas{}, problem(404, "user_not_found", "用户不存在")
	}
	q.URLLinkQuota = &urlQuota
	q.MinioAssetLinkQuota = &assetQuota
	return q, err
}

func (s *Service) UpdateUserQuotas(ctx context.Context, actorID, targetID uuid.UUID, input QuotaPatch, meta AuditMeta) (Quotas, error) {
	// 1. 额度独立修改；允许下调到已用量以下，但不删除或停用现有资源。
	if input.MaxSingleFileBytes == nil && input.StorageQuotaBytes == nil && input.APIKeyQuota == nil && input.URLLinkQuota == nil && input.MinioAssetLinkQuota == nil {
		return Quotas{}, problem(422, "empty_update", "至少提供一个额度字段")
	}
	if (input.MaxSingleFileBytes != nil && *input.MaxSingleFileBytes < 0) || (input.StorageQuotaBytes != nil && *input.StorageQuotaBytes < 0) || (input.APIKeyQuota != nil && *input.APIKeyQuota < 0) || (input.URLLinkQuota != nil && *input.URLLinkQuota < 0) || (input.MinioAssetLinkQuota != nil && *input.MinioAssetLinkQuota < 0) {
		return Quotas{}, problem(422, "invalid_quota", "额度不能为负数")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quotas{}, err
	}
	defer tx.Rollback()
	var workspace uuid.UUID
	var previousMaxSingleFileBytes, previousStorageQuotaBytes int64
	var previousAPIKeyQuota, previousURLLinkQuota, previousAssetLinkQuota int
	if err := tx.QueryRowContext(ctx, `SELECT default_workspace_id,max_single_file_bytes,storage_quota_bytes,api_key_quota,url_link_quota,minio_asset_link_quota FROM users WHERE user_id=$1 FOR UPDATE`, targetID).Scan(&workspace, &previousMaxSingleFileBytes, &previousStorageQuotaBytes, &previousAPIKeyQuota, &previousURLLinkQuota, &previousAssetLinkQuota); errors.Is(err, sql.ErrNoRows) {
		return Quotas{}, problem(404, "user_not_found", "用户不存在")
	} else if err != nil {
		return Quotas{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET max_single_file_bytes=COALESCE($2,max_single_file_bytes),storage_quota_bytes=COALESCE($3,storage_quota_bytes),api_key_quota=COALESCE($4,api_key_quota),url_link_quota=COALESCE($5,url_link_quota),minio_asset_link_quota=COALESCE($6,minio_asset_link_quota),updated_at=now() WHERE user_id=$1`, targetID, input.MaxSingleFileBytes, input.StorageQuotaBytes, input.APIKeyQuota, input.URLLinkQuota, input.MinioAssetLinkQuota); err != nil {
		return Quotas{}, err
	}
	changes := make(map[string]any)
	if input.MaxSingleFileBytes != nil {
		changes["maxSingleFileBytesBefore"], changes["maxSingleFileBytesAfter"] = previousMaxSingleFileBytes, *input.MaxSingleFileBytes
	}
	if input.StorageQuotaBytes != nil {
		changes["storageQuotaBytesBefore"], changes["storageQuotaBytesAfter"] = previousStorageQuotaBytes, *input.StorageQuotaBytes
	}
	if input.APIKeyQuota != nil {
		changes["apiKeyQuotaBefore"], changes["apiKeyQuotaAfter"] = previousAPIKeyQuota, *input.APIKeyQuota
	}
	if input.URLLinkQuota != nil {
		changes["urlLinkQuotaBefore"], changes["urlLinkQuotaAfter"] = previousURLLinkQuota, *input.URLLinkQuota
	}
	if input.MinioAssetLinkQuota != nil {
		changes["minioAssetLinkQuotaBefore"], changes["minioAssetLinkQuotaAfter"] = previousAssetLinkQuota, *input.MinioAssetLinkQuota
	}
	if err := writeAuditMeta(ctx, tx, &actorID, &workspace, "admin.user_quotas.update", "user", targetID.String(), "success", meta, changes); err != nil {
		return Quotas{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quotas{}, err
	}
	return s.UserQuotas(ctx, targetID)
}

func (s *Service) RecordAdminRead(ctx context.Context, actorID uuid.UUID, event, targetType, targetID string, meta AuditMeta, details map[string]any) error {
	// 1. 管理员读取用户资料也属于敏感操作，记录筛选摘要而不复制用户数据。
	raw := []byte(`{}`)
	if details != nil {
		encoded, err := json.Marshal(details)
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,$4,NULLIF($5,'')::inet,$6,'success',$7::jsonb,NULLIF($8,''))`, actorID, event, targetType, targetID, meta.SourceIP, meta.UserAgent, string(raw), meta.RequestID)
	return err
}

func knownPermission(code string) bool {
	for _, item := range permissionCatalog {
		if item.Code == code {
			return true
		}
	}
	return false
}

func writeAuditMeta(ctx context.Context, tx *sql.Tx, actor, workspace *uuid.UUID, event, targetType, targetID, result string, meta AuditMeta, details ...map[string]any) error {
	// 1. 审计详情只接受结构化白名单字段；邀请码明文、口令和认证令牌不传入这里。
	raw := []byte(`{}`)
	if len(details) > 0 && details[0] != nil {
		encoded, err := json.Marshal(details[0])
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::inet,$7,$8,$9::jsonb,NULLIF($10,''))`, actor, workspace, event, targetType, targetID, meta.SourceIP, meta.UserAgent, result, string(raw), meta.RequestID)
	return err
}
