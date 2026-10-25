package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

const apiKeySecretPrefix = "stub_sk_"

var apiKeyScopeAllowlist = map[string]struct{}{
	"links.read": {}, "links.create_url": {}, "links.create_asset": {}, "links.manage": {}, "links.reset_visits": {},
	"assets.read": {}, "assets.upload": {}, "assets.delete": {}, "assets.restore": {},
	"templates.read_use": {}, "templates.manage": {},
}

type APIKeyIdentity struct {
	User               User
	ID                 uuid.UUID
	PublicID           string
	Prefix             string
	Name               string
	BusinessID         string
	Scopes             map[string]bool
	AllowAccountAccess bool
}

type APIKeyQuota struct {
	MaxUploadBytes    *int64 `json:"maxUploadBytes"`
	UploadedBytesUsed int64  `json:"uploadedBytesUsed"`
	MaxCreatedLinks   *int   `json:"maxCreatedLinks"`
	CreatedLinksUsed  int64  `json:"createdLinksUsed"`
	MaxRequests       *int64 `json:"maxRequests"`
	RequestsUsed      int64  `json:"requestsUsed"`
}

type APIKeyDTO struct {
	APIKeyID           string      `json:"apiKeyId"`
	Name               string      `json:"name"`
	Description        string      `json:"description"`
	KeyPrefix          string      `json:"keyPrefix"`
	Status             string      `json:"status"`
	BusinessID         string      `json:"businessId"`
	AllowAccountAccess bool        `json:"allowAccountAccess"`
	Scopes             []string    `json:"scopes"`
	AllowedIPCIDRs     []string    `json:"allowedIpCidrs"`
	RateLimitPerMin    int         `json:"rateLimitPerMin"`
	Quotas             APIKeyQuota `json:"quotas"`
	ExpiresAt          *time.Time  `json:"expiresAt"`
	LastUsedAt         *time.Time  `json:"lastUsedAt"`
	LastUsedIP         *string     `json:"lastUsedIp"`
	RevokedAt          *time.Time  `json:"revokedAt"`
	CreatedAt          time.Time   `json:"createdAt"`
	UpdatedAt          time.Time   `json:"updatedAt"`
}

type CreateAPIKeyRequest struct {
	Name               string     `json:"name"`
	Description        string     `json:"description,omitempty"`
	BusinessID         string     `json:"businessId"`
	Scopes             []string   `json:"scopes"`
	AllowedIPCIDRs     []string   `json:"allowedIpCidrs,omitempty"`
	RateLimitPerMin    int        `json:"rateLimitPerMin,omitempty"`
	MaxUploadBytes     *int64     `json:"maxUploadBytes"`
	MaxCreatedLinks    *int       `json:"maxCreatedLinks"`
	MaxRequests        *int64     `json:"maxRequests"`
	ExpiresAt          *time.Time `json:"expiresAt"`
	AllowAccountAccess bool       `json:"allowAccountAccess"`
}

type CreateAPIKeyResponse struct {
	APIKeyDTO
	SecretKey string `json:"secretKey"`
}

type OptionalInt64 struct {
	Set   bool
	Valid bool
	Value int64
}

func (v *OptionalInt64) UnmarshalJSON(data []byte) error {
	v.Set = true
	if strings.TrimSpace(string(data)) == "null" {
		v.Valid = false
		return nil
	}
	if err := json.Unmarshal(data, &v.Value); err != nil || v.Value < 0 {
		return fmt.Errorf("必须是非负整数或 null")
	}
	v.Valid = true
	return nil
}

type OptionalInt struct {
	Set   bool
	Valid bool
	Value int
}

func (v *OptionalInt) UnmarshalJSON(data []byte) error {
	v.Set = true
	if strings.TrimSpace(string(data)) == "null" {
		v.Valid = false
		return nil
	}
	if err := json.Unmarshal(data, &v.Value); err != nil || v.Value < 0 {
		return fmt.Errorf("必须是非负整数或 null")
	}
	v.Valid = true
	return nil
}

type OptionalTime struct {
	Set   bool
	Valid bool
	Value time.Time
}

func (v *OptionalTime) UnmarshalJSON(data []byte) error {
	v.Set = true
	if strings.TrimSpace(string(data)) == "null" {
		v.Valid = false
		return nil
	}
	if err := json.Unmarshal(data, &v.Value); err != nil {
		return fmt.Errorf("时间格式无效")
	}
	v.Valid = !v.Value.IsZero()
	if !v.Valid {
		return fmt.Errorf("时间格式无效")
	}
	return nil
}

type UpdateAPIKeyRequest struct {
	Name               *string       `json:"name,omitempty"`
	Description        *string       `json:"description,omitempty"`
	BusinessID         *string       `json:"businessId,omitempty"`
	Scopes             *[]string     `json:"scopes,omitempty"`
	AllowedIPCIDRs     *[]string     `json:"allowedIpCidrs,omitempty"`
	RateLimitPerMin    *int          `json:"rateLimitPerMin,omitempty"`
	MaxUploadBytes     OptionalInt64 `json:"maxUploadBytes"`
	MaxCreatedLinks    OptionalInt   `json:"maxCreatedLinks"`
	MaxRequests        OptionalInt64 `json:"maxRequests"`
	ExpiresAt          OptionalTime  `json:"expiresAt"`
	AllowAccountAccess *bool         `json:"allowAccountAccess,omitempty"`
}

type APIKeyPage struct {
	Items      []APIKeyDTO `json:"items"`
	Pagination struct {
		Page     int   `json:"page"`
		PageSize int   `json:"pageSize"`
		Total    int64 `json:"total"`
	} `json:"pagination"`
}

type apiKeyRecord struct {
	ID                 uuid.UUID
	UserID             uuid.UUID
	WorkspaceID        uuid.UUID
	BusinessID         string
	Name               string
	Description        string
	Prefix             string
	Status             string
	AllowAccountAccess bool
	AllowedIPCIDRs     string
	RateLimitPerMin    int
	MaxUploadBytes     sql.NullInt64
	UploadedBytesUsed  int64
	MaxCreatedLinks    sql.NullInt64
	CreatedLinksUsed   int64
	MaxRequests        sql.NullInt64
	RequestsUsed       int64
	ExpiresAt          sql.NullTime
	LastUsedAt         sql.NullTime
	LastUsedIP         sql.NullString
	RevokedAt          sql.NullTime
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (s *Service) CreateAPIKey(ctx context.Context, actor User, input CreateAPIKeyRequest, meta AuditMeta) (CreateAPIKeyResponse, error) {
	// 1. 创建只能由人类会话完成；Key 自身永远不能创建另一把 Key。
	if !permissionEnabled(ctx, s.db, actor.ID, actor.Role, "api_keys.manage") {
		return CreateAPIKeyResponse{}, problem(403, "permission_denied", "当前账号没有管理 API Key 的权限")
	}
	name := strings.TrimSpace(input.Name)
	description := strings.TrimSpace(input.Description)
	if name == "" || len([]rune(name)) > 80 || len([]rune(description)) > 255 || strings.TrimSpace(input.BusinessID) == "" {
		return CreateAPIKeyResponse{}, problem(422, "invalid_api_key", "API Key 名称、描述或业务标识无效")
	}
	if input.RateLimitPerMin == 0 {
		input.RateLimitPerMin = 60
	}
	if input.RateLimitPerMin < 1 || input.RateLimitPerMin > 600 || input.MaxUploadBytes != nil && *input.MaxUploadBytes < 0 || input.MaxCreatedLinks != nil && *input.MaxCreatedLinks < 0 || input.MaxRequests != nil && *input.MaxRequests < 0 {
		return CreateAPIKeyResponse{}, problem(422, "invalid_api_key_limits", "API Key 限制参数无效")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return CreateAPIKeyResponse{}, problem(422, "invalid_api_key_expiration", "API Key 到期时间必须晚于当前时间")
	}
	cidrs, err := normalizeAPIKeyCIDRs(input.AllowedIPCIDRs)
	if err != nil {
		return CreateAPIKeyResponse{}, err
	}
	scopes, err := s.validateAPIKeyScopes(ctx, actor.ID, actor.Role, input.Scopes)
	if err != nil {
		return CreateAPIKeyResponse{}, err
	}
	if err := s.validateBusinessAccess(ctx, actor, input.BusinessID); err != nil {
		return CreateAPIKeyResponse{}, err
	}

	secret, err := generateAPIKeySecret()
	if err != nil {
		return CreateAPIKeyResponse{}, err
	}
	id := uuid.New()
	workspaceID, err := parseAuthPublicID("ws", actor.DefaultWorkspaceID)
	if err != nil {
		return CreateAPIKeyResponse{}, fmt.Errorf("用户默认工作空间无效: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateAPIKeyResponse{}, err
	}
	defer tx.Rollback()
	var quota int
	var activeCount int
	if err := tx.QueryRowContext(ctx, `SELECT api_key_quota FROM users WHERE user_id=$1 FOR UPDATE`, actor.ID).Scan(&quota); err != nil {
		return CreateAPIKeyResponse{}, err
	}
	if actor.Role != "admin" {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE user_id=$1 AND status IN ('active','disabled')`, actor.ID).Scan(&activeCount); err != nil {
			return CreateAPIKeyResponse{}, err
		}
		if activeCount >= quota {
			return CreateAPIKeyResponse{}, problem(409, "api_key_limit_reached", "API Key 数量已达到管理员设置的上限")
		}
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_keys(api_key_id,user_id,workspace_id,business_id,name,description,key_prefix,key_hash,allow_account_access,allowed_ip_cidrs,rate_limit_per_min,max_upload_bytes,max_created_links,max_requests,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::cidr[],$11,$12,$13,$14,$15,$16,$16)`, id, actor.ID, workspaceID, input.BusinessID, name, description, apiKeyPrefix(secret), sha256Hex(secret), input.AllowAccountAccess, cidrArrayForAPIKey(cidrs), input.RateLimitPerMin, input.MaxUploadBytes, input.MaxCreatedLinks, input.MaxRequests, input.ExpiresAt, now); err != nil {
		return CreateAPIKeyResponse{}, err
	}
	for _, scope := range scopes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_permissions(api_key_id,permission_code) VALUES($1,$2)`, id, scope); err != nil {
			return CreateAPIKeyResponse{}, err
		}
	}
	if err := writeAuditMeta(ctx, tx, &actor.ID, &workspaceID, "api_key.create", "api_key", id.String(), "success", meta, map[string]any{"apiKeyId": publicID("ak", id), "apiKeyPrefix": apiKeyPrefix(secret), "apiKeyName": name, "businessId": input.BusinessID, "scopes": scopes}); err != nil {
		return CreateAPIKeyResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateAPIKeyResponse{}, err
	}
	dto, err := s.apiKeyDTO(ctx, id)
	if err != nil {
		return CreateAPIKeyResponse{}, err
	}
	return CreateAPIKeyResponse{APIKeyDTO: dto, SecretKey: secret}, nil
}

func (s *Service) ListAPIKeys(ctx context.Context, userID uuid.UUID, status string, page, pageSize int) (APIKeyPage, error) {
	return s.listAPIKeys(ctx, userID, status, page, pageSize)
}

func (s *Service) AdminListAPIKeys(ctx context.Context, userID uuid.UUID, status string, page, pageSize int) (APIKeyPage, error) {
	return s.listAPIKeys(ctx, userID, status, page, pageSize)
}

func (s *Service) listAPIKeys(ctx context.Context, userID uuid.UUID, status string, page, pageSize int) (APIKeyPage, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 || status != "" && status != "active" && status != "disabled" && status != "revoked" {
		return APIKeyPage{}, problem(422, "invalid_filter", "API Key 查询参数无效")
	}
	where := "user_id=$1"
	args := []any{userID}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status=$%d", len(args))
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE `+where, args...).Scan(&total); err != nil {
		return APIKeyPage{}, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT api_key_id,name,description,business_id,key_prefix,status,allow_account_access,allowed_ip_cidrs::text,rate_limit_per_min,max_upload_bytes,uploaded_bytes_used,max_created_links,created_links_used,max_requests,requests_used,expires_at,last_used_at,last_used_ip::text,revoked_at,created_at,updated_at FROM api_keys WHERE `+where+fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return APIKeyPage{}, err
	}
	defer rows.Close()
	items := make([]APIKeyDTO, 0)
	for rows.Next() {
		var record apiKeyRecord
		if err := rows.Scan(&record.ID, &record.Name, &record.Description, &record.BusinessID, &record.Prefix, &record.Status, &record.AllowAccountAccess, &record.AllowedIPCIDRs, &record.RateLimitPerMin, &record.MaxUploadBytes, &record.UploadedBytesUsed, &record.MaxCreatedLinks, &record.CreatedLinksUsed, &record.MaxRequests, &record.RequestsUsed, &record.ExpiresAt, &record.LastUsedAt, &record.LastUsedIP, &record.RevokedAt, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return APIKeyPage{}, err
		}
		dto, err := s.recordDTO(ctx, record)
		if err != nil {
			return APIKeyPage{}, err
		}
		items = append(items, dto)
	}
	return APIKeyPage{Items: items, Pagination: struct {
		Page     int   `json:"page"`
		PageSize int   `json:"pageSize"`
		Total    int64 `json:"total"`
	}{Page: page, PageSize: pageSize, Total: total}}, rows.Err()
}

func (s *Service) UpdateAPIKey(ctx context.Context, actor User, rawID string, input UpdateAPIKeyRequest, meta AuditMeta) (APIKeyDTO, error) {
	id, err := parseAPIKeyID(rawID)
	if err != nil {
		return APIKeyDTO{}, problem(404, "api_key_not_found", "API Key 不存在")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIKeyDTO{}, err
	}
	defer tx.Rollback()
	record, err := loadAPIKeyRecord(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) || record.UserID != actor.ID {
		return APIKeyDTO{}, problem(404, "api_key_not_found", "API Key 不存在")
	}
	if err != nil {
		return APIKeyDTO{}, err
	}
	if record.Status == "revoked" {
		return APIKeyDTO{}, problem(409, "api_key_already_revoked", "API Key 已永久吊销，不能修改")
	}
	name, description, businessID := record.Name, record.Description, record.BusinessID
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
	}
	if input.Description != nil {
		description = strings.TrimSpace(*input.Description)
	}
	if name == "" || len([]rune(name)) > 80 || len([]rune(description)) > 255 {
		return APIKeyDTO{}, problem(422, "invalid_api_key", "API Key 名称或描述无效")
	}
	if input.BusinessID != nil {
		businessID = strings.TrimSpace(*input.BusinessID)
	}
	if err := s.validateBusinessAccess(ctx, actor, businessID); err != nil {
		return APIKeyDTO{}, err
	}
	allowed := parseAPIKeyCIDRArray(record.AllowedIPCIDRs)
	if input.AllowedIPCIDRs != nil {
		allowed, err = normalizeAPIKeyCIDRs(*input.AllowedIPCIDRs)
		if err != nil {
			return APIKeyDTO{}, err
		}
	}
	rate := record.RateLimitPerMin
	if input.RateLimitPerMin != nil {
		rate = *input.RateLimitPerMin
	}
	if rate < 1 || rate > 600 {
		return APIKeyDTO{}, problem(422, "invalid_api_key_limits", "API Key 频率限制无效")
	}
	var scopes []string
	if input.Scopes != nil {
		scopes, err = s.validateAPIKeyScopes(ctx, actor.ID, actor.Role, *input.Scopes)
	} else {
		scopes, err = s.apiKeyScopes(ctx, id)
	}
	if err != nil {
		return APIKeyDTO{}, err
	}
	maxUpload, maxLinks, maxRequests := nullableInt64Value(record.MaxUploadBytes), nullableIntValue(record.MaxCreatedLinks), nullableInt64Value(record.MaxRequests)
	if input.MaxUploadBytes.Set {
		maxUpload = optionalInt64Value(input.MaxUploadBytes)
	}
	if input.MaxCreatedLinks.Set {
		maxLinks = optionalIntValue(input.MaxCreatedLinks)
	}
	if input.MaxRequests.Set {
		maxRequests = optionalInt64Value(input.MaxRequests)
	}
	if maxUpload != nil && *maxUpload < record.UploadedBytesUsed || maxLinks != nil && int64(*maxLinks) < record.CreatedLinksUsed || maxRequests != nil && *maxRequests < record.RequestsUsed {
		return APIKeyDTO{}, problem(422, "api_key_limit_below_usage", "新的 API Key 上限不能小于已使用额度")
	}
	expires := any(nil)
	if record.ExpiresAt.Valid {
		expires = record.ExpiresAt.Time
	}
	if input.ExpiresAt.Set {
		if input.ExpiresAt.Valid {
			if !input.ExpiresAt.Value.After(time.Now()) {
				return APIKeyDTO{}, problem(422, "invalid_api_key_expiration", "API Key 到期时间必须晚于当前时间")
			}
			expires = input.ExpiresAt.Value
		} else {
			expires = nil
		}
	}
	allowAccount := record.AllowAccountAccess
	if input.AllowAccountAccess != nil {
		allowAccount = *input.AllowAccountAccess
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET name=$2,description=$3,business_id=$4,allow_account_access=$5,allowed_ip_cidrs=$6::cidr[],rate_limit_per_min=$7,max_upload_bytes=$8,max_created_links=$9,max_requests=$10,expires_at=$11,updated_at=$12 WHERE api_key_id=$1`, id, name, description, businessID, allowAccount, cidrArrayForAPIKey(allowed), rate, maxUpload, maxLinks, maxRequests, expires, now); err != nil {
		return APIKeyDTO{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_permissions WHERE api_key_id=$1`, id); err != nil {
		return APIKeyDTO{}, err
	}
	for _, scope := range scopes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_permissions(api_key_id,permission_code) VALUES($1,$2)`, id, scope); err != nil {
			return APIKeyDTO{}, err
		}
	}
	if err := writeAuditMeta(ctx, tx, &actor.ID, &record.WorkspaceID, "api_key.update", "api_key", id.String(), "success", meta, map[string]any{"apiKeyId": publicID("ak", id), "apiKeyPrefix": record.Prefix, "apiKeyName": name, "businessId": businessID, "scopes": scopes}); err != nil {
		return APIKeyDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return APIKeyDTO{}, err
	}
	return s.apiKeyDTO(ctx, id)
}

func (s *Service) SetAPIKeyStatus(ctx context.Context, actor User, rawID, status string, meta AuditMeta) (APIKeyDTO, error) {
	if status != "active" && status != "disabled" && status != "revoked" {
		return APIKeyDTO{}, problem(422, "invalid_api_key_status", "API Key 状态无效")
	}
	id, err := parseAPIKeyID(rawID)
	if err != nil {
		return APIKeyDTO{}, problem(404, "api_key_not_found", "API Key 不存在")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIKeyDTO{}, err
	}
	defer tx.Rollback()
	record, err := loadAPIKeyRecord(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) || record.UserID != actor.ID {
		return APIKeyDTO{}, problem(404, "api_key_not_found", "API Key 不存在")
	}
	if err != nil {
		return APIKeyDTO{}, err
	}
	if record.Status == "revoked" && status != "revoked" {
		return APIKeyDTO{}, problem(409, "api_key_already_revoked", "API Key 已永久吊销，不能恢复")
	}
	now := time.Now().UTC()
	revoked := any(nil)
	if status == "revoked" {
		revoked = now
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET status=$2,revoked_at=COALESCE(revoked_at,$3),updated_at=$4 WHERE api_key_id=$1`, id, status, revoked, now); err != nil {
		return APIKeyDTO{}, err
	}
	if err := writeAuditMeta(ctx, tx, &actor.ID, &record.WorkspaceID, "api_key."+status, "api_key", id.String(), "success", meta, map[string]any{"apiKeyId": publicID("ak", id), "apiKeyPrefix": record.Prefix, "apiKeyName": record.Name}); err != nil {
		return APIKeyDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return APIKeyDTO{}, err
	}
	return s.apiKeyDTO(ctx, id)
}

func (s *Service) AdminRevokeAPIKey(ctx context.Context, actorID uuid.UUID, rawID string, meta AuditMeta) error {
	id, err := parseAPIKeyID(rawID)
	if err != nil {
		return problem(404, "api_key_not_found", "API Key 不存在")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	record, err := loadAPIKeyRecord(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "api_key_not_found", "API Key 不存在")
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET status='revoked',revoked_at=COALESCE(revoked_at,$2),updated_at=$2 WHERE api_key_id=$1`, id, now); err != nil {
		return err
	}
	if err := writeAuditMeta(ctx, tx, &actorID, &record.WorkspaceID, "admin.api_key.revoke", "api_key", id.String(), "success", meta, map[string]any{"apiKeyId": publicID("ak", id), "apiKeyPrefix": record.Prefix, "apiKeyName": record.Name, "targetUserId": publicID("usr", record.UserID)}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) AuthenticateAPIKey(ctx context.Context, secret, sourceIP string) (APIKeyIdentity, error) {
	if !strings.HasPrefix(secret, apiKeySecretPrefix) {
		return APIKeyIdentity{}, problem(401, "invalid_api_key", "API Key 无效")
	}
	var record apiKeyRecord
	var email, role string
	var verifiedAt, userCreated time.Time
	var workspace uuid.UUID
	var invitedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT k.api_key_id,k.user_id,k.workspace_id,k.business_id,k.name,k.description,k.key_prefix,k.status,k.allow_account_access,k.allowed_ip_cidrs::text,k.rate_limit_per_min,k.max_upload_bytes,k.uploaded_bytes_used,k.max_created_links,k.created_links_used,k.max_requests,k.requests_used,k.expires_at,k.last_used_at,k.last_used_ip::text,k.revoked_at,k.created_at,k.updated_at,u.email,u.role,u.email_verified_at,u.created_at,u.default_workspace_id,u.invited_by_user_id::text FROM api_keys k JOIN users u ON u.user_id=k.user_id WHERE k.key_hash=$1`, sha256Hex(secret)).Scan(&record.ID, &record.UserID, &record.WorkspaceID, &record.BusinessID, &record.Name, &record.Description, &record.Prefix, &record.Status, &record.AllowAccountAccess, &record.AllowedIPCIDRs, &record.RateLimitPerMin, &record.MaxUploadBytes, &record.UploadedBytesUsed, &record.MaxCreatedLinks, &record.CreatedLinksUsed, &record.MaxRequests, &record.RequestsUsed, &record.ExpiresAt, &record.LastUsedAt, &record.LastUsedIP, &record.RevokedAt, &record.CreatedAt, &record.UpdatedAt, &email, &role, &verifiedAt, &userCreated, &workspace, &invitedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKeyIdentity{}, problem(401, "invalid_api_key", "API Key 无效")
	}
	if err != nil {
		return APIKeyIdentity{}, fmt.Errorf("查询 API Key 失败: %w", err)
	}
	if record.Status == "revoked" {
		return APIKeyIdentity{}, problem(401, "api_key_revoked", "API Key 已吊销")
	}
	if record.Status == "disabled" {
		return APIKeyIdentity{}, problem(403, "api_key_disabled", "API Key 已停用")
	}
	if record.ExpiresAt.Valid && !record.ExpiresAt.Time.After(time.Now()) {
		return APIKeyIdentity{}, problem(401, "api_key_expired", "API Key 已过期")
	}
	if !ipAllowed(sourceIP, parseAPIKeyCIDRArray(record.AllowedIPCIDRs)) {
		return APIKeyIdentity{}, problem(403, "api_key_ip_denied", "请求来源 IP 不在 API Key 白名单内")
	}
	if !permissionEnabled(ctx, s.db, record.UserID, role, "api_keys.manage") {
		return APIKeyIdentity{}, problem(403, "api_key_owner_permission_denied", "API Key 所属用户已被禁止使用 API Key")
	}
	if record.RateLimitPerMin < 1 {
		return APIKeyIdentity{}, problem(403, "api_key_disabled", "API Key 配置无效")
	}
	window := time.Now().UTC().Unix() / 60
	rateKey := fmt.Sprintf("api-key:rate:%s:%d", record.ID.String(), window)
	count, err := s.redis.Incr(ctx, rateKey).Result()
	if err != nil {
		return APIKeyIdentity{}, fmt.Errorf("API Key 限流失败: %w", err)
	}
	if count == 1 {
		_ = s.redis.Expire(ctx, rateKey, 70*time.Second).Err()
	}
	if count > int64(record.RateLimitPerMin) {
		return APIKeyIdentity{}, problem(429, "api_key_rate_limited", "API Key 请求频率超限")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE api_keys SET requests_used=requests_used+1,last_used_at=now(),last_used_ip=NULLIF($2,'')::inet,updated_at=updated_at WHERE api_key_id=$1 AND status='active' AND (expires_at IS NULL OR expires_at>now()) AND (max_requests IS NULL OR requests_used<max_requests)`, record.ID, sourceIP)
	if err != nil {
		return APIKeyIdentity{}, err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return APIKeyIdentity{}, problem(429, "api_key_request_quota_exceeded", "API Key 累计请求额度已用尽")
	}
	scopes, err := s.apiKeyScopes(ctx, record.ID)
	if err != nil {
		return APIKeyIdentity{}, err
	}
	scopeMap := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		scopeMap[scope] = true
	}
	user := User{ID: record.UserID, UserID: publicID("usr", record.UserID), Email: email, Role: role, EmailVerifiedAt: verifiedAt.UTC(), DefaultWorkspaceID: publicID("ws", workspace), CreatedAt: userCreated.UTC()}
	if invitedBy.Valid {
		value := "usr_" + invitedBy.String
		user.InvitedByUserID = &value
	}
	return APIKeyIdentity{User: user, ID: record.ID, PublicID: publicID("ak", record.ID), Prefix: record.Prefix, Name: record.Name, BusinessID: record.BusinessID, Scopes: scopeMap, AllowAccountAccess: record.AllowAccountAccess}, nil
}

func permissionEnabled(ctx context.Context, db *sql.DB, userID uuid.UUID, role, code string) bool {
	if role == "admin" {
		return true
	}
	var enabled bool
	return db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_permissions WHERE user_id=$1 AND permission_code=$2 AND granted_at IS NOT NULL AND revoked_at IS NULL)`, userID, code).Scan(&enabled) == nil && enabled
}

func (s *Service) validateAPIKeyScopes(ctx context.Context, userID uuid.UUID, role string, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, problem(422, "api_key_scopes_required", "至少选择一个 API Key 权限")
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(requested))
	for _, raw := range requested {
		scope := strings.TrimSpace(raw)
		if _, ok := apiKeyScopeAllowlist[scope]; !ok || seen[scope] {
			return nil, problem(422, "invalid_api_key_scope", "API Key 权限范围无效")
		}
		if !permissionEnabled(ctx, s.db, userID, role, scope) {
			return nil, problem(403, "permission_denied", "当前账号没有所选 API Key 权限")
		}
		seen[scope] = true
		result = append(result, scope)
	}
	return result, nil
}

func (s *Service) validateBusinessAccess(ctx context.Context, actor User, businessID string) error {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM business_integrations bi WHERE bi.business_id=$1 AND bi.status='active' AND ($2='admin' OR EXISTS(SELECT 1 FROM user_business_access uba WHERE uba.user_id=$3 AND uba.business_id=$1 AND uba.revoked_at IS NULL)))`, strings.TrimSpace(businessID), actor.Role, actor.ID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return problem(404, "business_not_found", "业务不存在、已停用或无权访问")
	}
	return nil
}

func (s *Service) apiKeyScopes(ctx context.Context, id uuid.UUID) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT permission_code FROM api_key_permissions WHERE api_key_id=$1 ORDER BY permission_code`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Service) apiKeyDTO(ctx context.Context, id uuid.UUID) (APIKeyDTO, error) {
	record, err := loadAPIKeyRecord(ctx, s.db, id, false)
	if err != nil {
		return APIKeyDTO{}, err
	}
	return s.recordDTO(ctx, record)
}

func (s *Service) recordDTO(ctx context.Context, r apiKeyRecord) (APIKeyDTO, error) {
	scopes, err := s.apiKeyScopes(ctx, r.ID)
	if err != nil {
		return APIKeyDTO{}, err
	}
	dto := APIKeyDTO{APIKeyID: publicID("ak", r.ID), Name: r.Name, Description: r.Description, KeyPrefix: r.Prefix, Status: r.Status, BusinessID: r.BusinessID, AllowAccountAccess: r.AllowAccountAccess, Scopes: scopes, AllowedIPCIDRs: parseAPIKeyCIDRArray(r.AllowedIPCIDRs), RateLimitPerMin: r.RateLimitPerMin, Quotas: APIKeyQuota{MaxUploadBytes: nullableInt64Value(r.MaxUploadBytes), UploadedBytesUsed: r.UploadedBytesUsed, MaxCreatedLinks: nullableIntValue(r.MaxCreatedLinks), CreatedLinksUsed: r.CreatedLinksUsed, MaxRequests: nullableInt64Value(r.MaxRequests), RequestsUsed: r.RequestsUsed}, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
	if r.ExpiresAt.Valid {
		value := r.ExpiresAt.Time.UTC()
		dto.ExpiresAt = &value
	}
	if r.LastUsedAt.Valid {
		value := r.LastUsedAt.Time.UTC()
		dto.LastUsedAt = &value
	}
	if r.LastUsedIP.Valid {
		value := r.LastUsedIP.String
		dto.LastUsedIP = &value
	}
	if r.RevokedAt.Valid {
		value := r.RevokedAt.Time.UTC()
		dto.RevokedAt = &value
	}
	return dto, nil
}

func loadAPIKeyRecord(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID, lock bool) (apiKeyRecord, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var r apiKeyRecord
	err := q.QueryRowContext(ctx, `SELECT api_key_id,user_id,workspace_id,business_id,name,description,key_prefix,status,allow_account_access,allowed_ip_cidrs::text,rate_limit_per_min,max_upload_bytes,uploaded_bytes_used,max_created_links,created_links_used,max_requests,requests_used,expires_at,last_used_at,last_used_ip::text,revoked_at,created_at,updated_at FROM api_keys WHERE api_key_id=$1`+suffix, id).Scan(&r.ID, &r.UserID, &r.WorkspaceID, &r.BusinessID, &r.Name, &r.Description, &r.Prefix, &r.Status, &r.AllowAccountAccess, &r.AllowedIPCIDRs, &r.RateLimitPerMin, &r.MaxUploadBytes, &r.UploadedBytesUsed, &r.MaxCreatedLinks, &r.CreatedLinksUsed, &r.MaxRequests, &r.RequestsUsed, &r.ExpiresAt, &r.LastUsedAt, &r.LastUsedIP, &r.RevokedAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func generateAPIKeySecret() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 40)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		result[i] = alphabet[n.Int64()]
	}
	return apiKeySecretPrefix + string(result), nil
}
func apiKeyPrefix(secret string) string {
	if len(secret) <= 16 {
		return secret
	}
	return secret[:12] + "..." + secret[len(secret)-4:]
}
func normalizeAPIKeyCIDRs(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			if ip.To4() != nil {
				raw = ip.String() + "/32"
			} else {
				raw = ip.String() + "/128"
			}
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, problem(422, "invalid_api_key_ip", "API Key IP 白名单无效")
		}
		value := network.String()
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result, nil
}
func cidrArrayForAPIKey(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}
func parseAPIKeyCIDRArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return []string{}
	}
	raw = strings.TrimPrefix(strings.TrimSuffix(raw, "}"), "{")
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, `" `)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
func nullableInt64Value(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}
func nullableIntValue(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}
func optionalInt64Value(value OptionalInt64) *int64 {
	if !value.Set || !value.Valid {
		return nil
	}
	result := value.Value
	return &result
}
func optionalIntValue(value OptionalInt) *int {
	if !value.Set || !value.Valid {
		return nil
	}
	result := value.Value
	return &result
}
func ipAllowed(source string, cidrs []string) bool {
	if len(cidrs) == 0 {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(source))
	if ip == nil {
		return false
	}
	for _, raw := range cidrs {
		_, network, err := net.ParseCIDR(raw)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
func parseAPIKeyID(value string) (uuid.UUID, error) {
	trimmed, ok := strings.CutPrefix(strings.TrimSpace(value), "ak_")
	if !ok {
		return uuid.Nil, fmt.Errorf("invalid api key id")
	}
	return uuid.Parse(trimmed)
}
func parseAuthPublicID(prefix, value string) (uuid.UUID, error) {
	trimmed, ok := strings.CutPrefix(strings.TrimSpace(value), prefix+"_")
	if !ok {
		return uuid.Nil, fmt.Errorf("invalid %s id", prefix)
	}
	return uuid.Parse(trimmed)
}
