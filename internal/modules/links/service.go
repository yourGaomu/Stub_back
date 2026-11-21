package links

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourGaomu/short_links/back/internal/config"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/platform/objectstore"
)

const accessLogWindow = time.Minute

var codePattern = regexp.MustCompile(`^[a-z0-9-]{4,64}$`)

type presignedGetter interface {
	PresignedGet(context.Context, string, string, time.Duration, string, string) (string, error)
}

type Service struct {
	db        *sql.DB
	cfg       config.Config
	store     presignedGetter
	accessNow func() time.Time
}

type Actor struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	Role        string
	BusinessID  string
	APIKeyID    *uuid.UUID
}

type RequestMeta struct {
	SourceIP     string
	UserAgent    string
	RequestID    string
	AuditDetails map[string]any
}

type linkRecord struct {
	ID              uuid.UUID
	WorkspaceID     uuid.UUID
	Title           string
	BusinessID      string
	Tags            []string
	GroupID         sql.NullString
	GroupName       sql.NullString
	TargetType      string
	TargetURL       sql.NullString
	AssetID         sql.NullString
	AssetSize       sql.NullInt64
	Bucket          sql.NullString
	ObjectKey       sql.NullString
	OriginalName    sql.NullString
	ContentType     sql.NullString
	ExpiresAt       sql.NullTime
	ActiveFrom      sql.NullTime
	ActiveUntil     sql.NullTime
	MaxVisits       int64
	VisitsUsed      int64
	IPCIDRs         string
	RequirePassword bool
	PasswordHash    sql.NullString
	PolicyJSON      []byte
	Status          string
	TemplateID      sql.NullString
	TemplateVersion sql.NullInt64
	Code            sql.NullString
	CodeStatus      sql.NullString
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func NewService(db *sql.DB, cfg config.Config, store *objectstore.Store) *Service {
	service := &Service{db: db, cfg: cfg, accessNow: time.Now}
	// 1. 未配置的具体指针不转入接口，避免 typed nil 被当作可用签名器。
	if store != nil {
		service.store = store
	}
	return service
}

func (s *Service) Create(ctx context.Context, actor Actor, input CreateRequest, meta RequestMeta) (CreateResponse, error) {
	// 1. 校验目标和短码输入；外部 URL 只允许 HTTPS，不由本服务主动抓取。
	title, tags, err := normalizeMetadata(input.Title, input.Tags)
	if err != nil {
		return CreateResponse{}, err
	}
	code := strings.ToLower(strings.TrimSpace(input.Code))
	if code != "" && !codePattern.MatchString(code) {
		return CreateResponse{}, problem(422, "invalid_code", "短码格式无效")
	}
	if input.Target.Type != "url" && input.Target.Type != "minio_asset" {
		return CreateResponse{}, problem(422, "invalid_target", "短链目标类型无效")
	}
	if input.Target.Type == "url" {
		parsed, err := url.ParseRequestURI(strings.TrimSpace(input.Target.URL))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || input.Target.AssetID != "" {
			return CreateResponse{}, problem(422, "invalid_target", "URL 目标必须是有效的 HTTPS 地址")
		}
		input.Target.URL = parsed.String()
	} else if input.Target.URL != "" || input.Target.AssetID == "" {
		return CreateResponse{}, problem(422, "invalid_target", "素材目标必须提供 assetId 且不能提供 URL")
	}
	now := time.Now().UTC()
	expiresAt, activeFrom, activeUntil := nullTime(input.ExpiresAt), nullTime(input.ActiveFrom), nullTime(input.ActiveUntil)
	if expiresAt.Valid && !expiresAt.Time.After(now) {
		return CreateResponse{}, problem(422, "invalid_expiration", "到期时间必须晚于当前时间")
	}
	if activeUntil.Valid && !activeUntil.Time.After(now) {
		return CreateResponse{}, problem(422, "invalid_window", "开放结束时间必须晚于当前时间")
	}
	if err := validateWindow(activeFrom, activeUntil, expiresAt); err != nil {
		return CreateResponse{}, err
	}
	input.ExpiresAt = timePointer(expiresAt)
	input.ActiveFrom = timePointer(activeFrom)
	input.ActiveUntil = timePointer(activeUntil)

	// 2. 解析内联规则或固定到创建时版本的模板规则。
	rules, templateRef, err := s.resolveRules(ctx, actor, input.RuleSource)
	if err != nil {
		return CreateResponse{}, err
	}
	if err := ValidateRules(rules); err != nil {
		return CreateResponse{}, err
	}
	if rules.RequirePassword && strings.TrimSpace(input.AccessPassword) == "" {
		return CreateResponse{}, problem(422, "access_password_required", "启用访问口令时必须提供口令")
	}
	if rules.RequirePassword && !validAccessPassword(input.AccessPassword) {
		return CreateResponse{}, problem(422, "invalid_access_password", "访问口令必须为 1 至 1024 字节且不能全为空白")
	}
	if !rules.RequirePassword && input.AccessPassword != "" {
		return CreateResponse{}, problem(422, "unexpected_access_password", "未启用访问口令时不能提供口令")
	}
	var passwordHash any
	var policyJSON any
	if rules.RequirePassword {
		hash, err := auth.HashPassword(input.AccessPassword)
		if err != nil {
			return CreateResponse{}, err
		}
		passwordHash = hash
		policyJSON, _ = json.Marshal(rules.WrongPasswordPolicy)
	}

	var groupID *uuid.UUID
	if input.GroupID != nil {
		parsed, err := parseID("grp", *input.GroupID)
		if err != nil {
			return CreateResponse{}, problem(404, "group_not_found", "分组不存在或无权访问")
		}
		groupID = &parsed
	}

	// 3. Membership locks precede user/link row locks in every writer.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateResponse{}, err
	}
	defer tx.Rollback()
	if groupID != nil {
		if err := lockGroupMembership(ctx, tx, *groupID, false); err != nil {
			return CreateResponse{}, err
		}
	}
	var urlQuota, assetLinkQuota int
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role, url_link_quota, minio_asset_link_quota FROM users WHERE user_id=$1 FOR NO KEY UPDATE`, actor.UserID).
		Scan(&role, &urlQuota, &assetLinkQuota); err != nil {
		return CreateResponse{}, fmt.Errorf("读取短链创建额度失败: %w", err)
	}
	if actor.Role != role {
		actor.Role = role
	}
	var assetID *uuid.UUID
	assetBusinessID := actor.BusinessID
	target := TargetDTO{Type: input.Target.Type, URL: input.Target.URL}
	if input.Target.Type == "minio_asset" {
		parsedID, err := parseID("ast", input.Target.AssetID)
		if err != nil {
			return CreateResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
		}
		var workspaceID uuid.UUID
		var size int64
		var allowed bool
		err = tx.QueryRowContext(ctx, `
			SELECT a.workspace_id, a.size_bytes, a.business_id,
			       (u.role='admin' OR EXISTS (SELECT 1 FROM user_business_access uba WHERE uba.user_id=u.user_id AND uba.business_id=a.business_id AND uba.revoked_at IS NULL))
			FROM assets a JOIN users u ON u.user_id=$2
			JOIN business_integrations bi ON bi.business_id=a.business_id AND bi.status='active'
			WHERE a.asset_id=$1 AND a.deleted_at IS NULL AND a.purged_at IS NULL`, parsedID, actor.UserID).Scan(&workspaceID, &size, &assetBusinessID, &allowed)
		if errors.Is(err, sql.ErrNoRows) {
			return CreateResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
		}
		if err != nil {
			return CreateResponse{}, err
		}
		if actor.BusinessID != "" && assetBusinessID != actor.BusinessID {
			return CreateResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
		}
		if !allowed || (actor.Role != "admin" && workspaceID != actor.WorkspaceID) {
			return CreateResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
		}
		assetID = &parsedID
		sizeCopy := size
		target.AssetID = input.Target.AssetID
		target.AssetSizeBytes = &sizeCopy
	} else if actor.Role != "admin" {
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM short_links WHERE created_by=$1 AND target_type='url'`, actor.UserID).Scan(&used); err != nil {
			return CreateResponse{}, err
		}
		if used >= urlQuota {
			return CreateResponse{}, problem(409, "url_link_quota_exceeded", "URL 短链创建额度已用尽")
		}
	}
	if templateRef != nil {
		// 3. 锁定模板主记录并复核状态，避免读取规则后模板被并发停用或删除。
		templateID, _ := parseID("tpl", templateRef.TemplateID)
		var templateWorkspace uuid.UUID
		var templateStatus string
		var deletedAt sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT workspace_id,status,deleted_at FROM rule_templates WHERE template_id=$1 FOR SHARE`, templateID).Scan(&templateWorkspace, &templateStatus, &deletedAt); err != nil {
			return CreateResponse{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
		}
		if deletedAt.Valid || templateStatus != "active" || (actor.Role != "admin" && templateWorkspace != actor.WorkspaceID) {
			return CreateResponse{}, problem(409, "template_unavailable", "规则模板已停用或删除")
		}
	}
	if input.Target.Type == "minio_asset" && actor.Role != "admin" {
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM short_links WHERE created_by=$1 AND target_type='minio_asset'`, actor.UserID).Scan(&used); err != nil {
			return CreateResponse{}, err
		}
		if used >= assetLinkQuota {
			return CreateResponse{}, problem(409, "minio_asset_link_quota_exceeded", "素材短链创建额度已用尽")
		}
	}
	if actor.APIKeyID != nil {
		result, err := tx.ExecContext(ctx, `UPDATE api_keys SET created_links_used=created_links_used+1,updated_at=updated_at WHERE api_key_id=$1 AND status='active' AND (expires_at IS NULL OR expires_at>now()) AND (max_created_links IS NULL OR created_links_used < max_created_links)`, *actor.APIKeyID)
		if err != nil {
			return CreateResponse{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return CreateResponse{}, problem(429, "api_key_link_quota_exceeded", "API Key 短链创建额度已用尽")
		}
	}

	workspaceID := actor.WorkspaceID
	if assetID != nil {
		if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM assets WHERE asset_id=$1`, *assetID).Scan(&workspaceID); err != nil {
			return CreateResponse{}, err
		}
	}
	var groupName sql.NullString
	var responseGroupID, responseGroupName *string
	if groupID != nil {
		groupName, err = validateLinkGroup(ctx, tx, *groupID, workspaceID)
		if err != nil {
			return CreateResponse{}, err
		}
		responseGroupID, responseGroupName = publicGroupFields(sql.NullString{String: groupID.String(), Valid: true}, groupName)
	}
	linkID := uuid.New()
	tagsJSON, _ := json.Marshal(tags)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO short_links (link_id,workspace_id,business_id,title,tags,target_type,target_url,asset_id,expires_at,max_visits,allowed_ip_cidrs,require_password,password_hash,wrong_password_policy,status,template_id,template_version,created_by,group_id,active_from,active_until)
		VALUES ($1,$2,NULLIF($3,''),$4,$5::jsonb,$6,NULLIF($7,''),$8,$9,$10,$11::cidr[],$12,$13,$14::jsonb,'enabled',$15,$16,$17,$18,$19,$20)`,
		linkID, workspaceID, assetBusinessID, title, string(tagsJSON), input.Target.Type, input.Target.URL, assetID, input.ExpiresAt, rules.MaxVisits,
		cidrArray(rules.AllowedIPCIDRs), rules.RequirePassword, passwordHash, nullableJSON(policyJSON), nullableTemplateID(templateRef), nullableTemplateVersion(templateRef), actor.UserID, groupID, input.ActiveFrom, input.ActiveUntil)
	if err != nil {
		return CreateResponse{}, fmt.Errorf("创建短链记录失败: %w", err)
	}
	code, err = s.insertCode(ctx, tx, linkID, code)
	if err != nil {
		return CreateResponse{}, err
	}
	if err := audit(ctx, tx, actor.UserID, workspaceID, "link.create", "short_link", linkID.String(), meta, map[string]any{
		"title": title, "tags": tags, "groupId": responseGroupID, "groupName": responseGroupName, "targetType": input.Target.Type, "businessId": assetBusinessID, "code": code, "expiresAt": input.ExpiresAt,
		"activeFrom": input.ActiveFrom, "activeUntil": input.ActiveUntil,
		"maxVisits": rules.MaxVisits, "requirePassword": rules.RequirePassword,
		"templateId": nullablePublicTemplateID(templateRef), "templateVersion": nullablePublicTemplateVersion(templateRef),
	}); err != nil {
		return CreateResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateResponse{}, err
	}
	return CreateResponse{LinkID: publicID("link", linkID), BusinessID: assetBusinessID, Title: title, Tags: tags, GroupID: responseGroupID, GroupName: responseGroupName, Code: code, URL: s.linkURL(code), Target: target, ExpiresAt: input.ExpiresAt, ActiveFrom: input.ActiveFrom, ActiveUntil: input.ActiveUntil, Rules: rules, RuleTemplate: templateRef}, nil
}

func (s *Service) List(ctx context.Context, actor Actor, page, pageSize int, query map[string]string) (Page, error) {
	// 1. 先依据可信身份固定工作空间范围，再附加白名单筛选条件。
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return Page{}, problem(422, "invalid_pagination", "分页参数无效")
	}
	filter, args, err := buildListFilter(actor, query)
	if err != nil {
		return Page{}, err
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM short_links sl LEFT JOIN short_link_codes sc ON sc.link_id=sl.link_id AND sc.status='active' WHERE `+filter, args...).Scan(&total); err != nil {
		return Page{}, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
		SELECT sl.link_id, sc.code, sl.title, sl.tags::text, sl.target_type, sl.target_url, sl.asset_id,
		       a.size_bytes, sl.status, sl.expires_at,
		       sl.visits_used, sl.max_visits, sl.created_at,
		       `+availabilityExpression+`,
		       sl.group_id,g.name,sl.active_from,sl.active_until
		FROM short_links sl LEFT JOIN short_link_codes sc ON sc.link_id=sl.link_id AND sc.status='active'
		LEFT JOIN assets a ON a.asset_id=sl.asset_id
		LEFT JOIN link_groups g ON g.group_id=sl.group_id AND g.workspace_id=sl.workspace_id WHERE `+filter+fmt.Sprintf(" ORDER BY sl.created_at DESC,sl.link_id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	items := make([]ListItem, 0)
	for rows.Next() {
		var id uuid.UUID
		var item ListItem
		var code sql.NullString
		var tagsJSON []byte
		var targetURL, assetID, groupID, groupName sql.NullString
		var size sql.NullInt64
		var expires, activeFrom, activeUntil sql.NullTime
		if err := rows.Scan(&id, &code, &item.Title, &tagsJSON, &item.TargetType, &targetURL, &assetID, &size, &item.Status, &expires, &item.VisitsUsed, &item.MaxVisits, &item.CreatedAt, &item.Availability, &groupID, &groupName, &activeFrom, &activeUntil); err != nil {
			return Page{}, err
		}
		item.Tags = []string{}
		if err := json.Unmarshal(tagsJSON, &item.Tags); err != nil {
			return Page{}, fmt.Errorf("读取短链标签失败: %w", err)
		}
		item.LinkID = publicID("link", id)
		if code.Valid {
			item.Code = code.String
			item.URL = s.linkURL(code.String)
		}
		if size.Valid {
			value := size.Int64
			item.AssetSizeBytes = &value
		}
		if targetURL.Valid {
			item.TargetURL = targetURL.String
		}
		if assetID.Valid {
			if parsedID, err := uuid.Parse(assetID.String); err == nil {
				item.AssetID = publicID("ast", parsedID)
			}
		}
		item.ExpiresAt = timePointer(expires)
		item.ActiveFrom = timePointer(activeFrom)
		item.ActiveUntil = timePointer(activeUntil)
		item.GroupID, item.GroupName = publicGroupFields(groupID, groupName)
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	return Page{Items: items, Pagination: Pagination{Page: page, PageSize: pageSize, Total: total}}, rows.Err()
}

func (s *Service) Get(ctx context.Context, actor Actor, rawID string) (LinkDTO, error) {
	// 1. ID 与工作空间范围同时校验，避免通过错误信息枚举其他用户资源。
	id, err := parseID("link", rawID)
	if err != nil {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	r, err := s.loadLink(ctx, s.db, id, false)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return LinkDTO{}, err
	}
	if !linkInScope(actor, r) {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	return s.dto(r), nil
}

func (s *Service) UpdateMetadata(ctx context.Context, actor Actor, rawID string, input UpdateMetadataRequest, meta RequestMeta) (LinkDTO, error) {
	if (input.Title == nil) != (input.Tags == nil) {
		return LinkDTO{}, problem(422, "invalid_metadata", "title 和 tags 必须同时提供")
	}
	if input.Title == nil && len(input.GroupID) == 0 {
		return LinkDTO{}, problem(422, "empty_update", "至少提供 title/tags 或 groupId")
	}
	var title string
	var tags []string
	var err error
	if input.Title != nil {
		title, tags, err = normalizeMetadata(*input.Title, *input.Tags)
		if err != nil {
			return LinkDTO{}, err
		}
	}
	groupChanged := len(input.GroupID) > 0
	var requestedGroup *uuid.UUID
	if groupChanged && strings.TrimSpace(string(input.GroupID)) != "null" {
		var rawGroup string
		if err := json.Unmarshal(input.GroupID, &rawGroup); err != nil {
			return LinkDTO{}, problem(422, "invalid_group_id", "groupId 必须是分组 ID 或 null")
		}
		parsed, err := parseID("grp", rawGroup)
		if err != nil {
			return LinkDTO{}, problem(404, "group_not_found", "分组不存在或无权访问")
		}
		requestedGroup = &parsed
	}
	id, err := parseID("link", rawID)
	if err != nil {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LinkDTO{}, err
	}
	defer tx.Rollback()
	if requestedGroup != nil {
		if err := lockGroupMembership(ctx, tx, *requestedGroup, false); err != nil {
			return LinkDTO{}, err
		}
	}
	r, err := s.loadLink(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return LinkDTO{}, err
	}
	if !linkInScope(actor, r) {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if input.Title == nil {
		title, tags = r.Title, r.Tags
	}
	groupID, groupName := r.GroupID, r.GroupName
	if groupChanged {
		groupID, groupName = sql.NullString{}, sql.NullString{}
		if requestedGroup != nil {
			groupName, err = validateLinkGroup(ctx, tx, *requestedGroup, r.WorkspaceID)
			if err != nil {
				return LinkDTO{}, err
			}
			groupID = sql.NullString{String: requestedGroup.String(), Valid: true}
		}
	}
	tagsJSON, _ := json.Marshal(tags)
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE short_links SET title=$2,tags=$3::jsonb,updated_at=$4,group_id=$5 WHERE link_id=$1`, id, title, string(tagsJSON), now, groupID); err != nil {
		return LinkDTO{}, err
	}
	groupBefore, nameBefore := publicGroupFields(r.GroupID, r.GroupName)
	groupAfter, nameAfter := publicGroupFields(groupID, groupName)
	if err := audit(ctx, tx, actor.UserID, r.WorkspaceID, "link.metadata.update", "short_link", id.String(), meta, map[string]any{
		"titleBefore": r.Title, "titleAfter": title, "tagsBefore": r.Tags, "tagsAfter": tags,
		"groupIdBefore": groupBefore, "groupIdAfter": groupAfter, "groupNameBefore": nameBefore, "groupNameAfter": nameAfter,
	}); err != nil {
		return LinkDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return LinkDTO{}, err
	}
	r.Title, r.Tags, r.UpdatedAt, r.GroupID, r.GroupName = title, tags, now, groupID, groupName
	return s.dto(r), nil
}

func (s *Service) UpdateRules(ctx context.Context, actor Actor, rawID string, input UpdateRulesRequest, meta RequestMeta) (LinkDTO, error) {
	// 1. 规则按字段合并更新；模板版本只保留为来源记录，规则值仍以短链快照为准。
	if input.MaxVisits == nil && input.AllowedIPCIDRs == nil && input.RequirePassword == nil && input.AccessPassword == nil && input.WrongPasswordPolicy == nil && len(input.ExpiresAt) == 0 && len(input.ActiveFrom) == 0 && len(input.ActiveUntil) == 0 {
		return LinkDTO{}, problem(422, "empty_update", "至少提供一个规则字段")
	}
	id, err := parseID("link", rawID)
	if err != nil {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LinkDTO{}, err
	}
	defer tx.Rollback()
	r, err := s.loadLink(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return LinkDTO{}, err
	}
	if !linkInScope(actor, r) {
		return LinkDTO{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	now := time.Now().UTC()
	expiresAt, err := mergeTimePatch(input.ExpiresAt, r.ExpiresAt)
	if err != nil || (len(input.ExpiresAt) > 0 && expiresAt.Valid && !expiresAt.Time.After(now)) {
		return LinkDTO{}, problem(422, "invalid_expiration", "到期时间必须是合法且晚于当前时间的时间戳，或使用 null 清除到期时间")
	}
	activeFrom, err := mergeTimePatch(input.ActiveFrom, r.ActiveFrom)
	if err != nil {
		return LinkDTO{}, problem(422, "invalid_window", "开放开始时间必须是合法的 RFC3339 时间戳，或使用 null 清除")
	}
	activeUntil, err := mergeTimePatch(input.ActiveUntil, r.ActiveUntil)
	if err != nil || (len(input.ActiveUntil) > 0 && activeUntil.Valid && !activeUntil.Time.After(now)) {
		return LinkDTO{}, problem(422, "invalid_window", "开放结束时间必须是合法且晚于当前时间的时间戳，或使用 null 清除")
	}
	if err := validateWindow(activeFrom, activeUntil, expiresAt); err != nil {
		return LinkDTO{}, err
	}

	rules := Rules{
		MaxVisits: r.MaxVisits, AllowedIPCIDRs: parseCIDRArray(r.IPCIDRs), RequirePassword: r.RequirePassword,
	}
	if len(r.PolicyJSON) > 0 {
		_ = json.Unmarshal(r.PolicyJSON, &rules.WrongPasswordPolicy)
	}
	if input.MaxVisits != nil {
		rules.MaxVisits = *input.MaxVisits
	}
	if input.AllowedIPCIDRs != nil {
		rules.AllowedIPCIDRs = make([]string, 0, len(*input.AllowedIPCIDRs))
		for _, cidr := range *input.AllowedIPCIDRs {
			rules.AllowedIPCIDRs = append(rules.AllowedIPCIDRs, strings.TrimSpace(cidr))
		}
	}
	if input.RequirePassword != nil {
		rules.RequirePassword = *input.RequirePassword
	}
	if input.WrongPasswordPolicy != nil {
		rules.WrongPasswordPolicy = input.WrongPasswordPolicy
	} else if rules.RequirePassword && rules.WrongPasswordPolicy == nil {
		rules.WrongPasswordPolicy = &PasswordPolicy{Strategy: "fixed_cooldown"}
	}
	if !rules.RequirePassword && (input.AccessPassword != nil || input.WrongPasswordPolicy != nil) {
		return LinkDTO{}, problem(422, "unexpected_password_rule", "关闭访问口令时不能更新口令或错误策略")
	}
	if !rules.RequirePassword {
		rules.WrongPasswordPolicy = nil
	}
	if err := ValidateRules(rules); err != nil {
		return LinkDTO{}, err
	}

	passwordHash := any(nil)
	if r.PasswordHash.Valid {
		passwordHash = r.PasswordHash.String
	}
	if input.AccessPassword != nil {
		if !rules.RequirePassword || !validAccessPassword(*input.AccessPassword) {
			return LinkDTO{}, problem(422, "invalid_access_password", "访问口令必须为 1 至 1024 字节且不能全为空白")
		}
		hash, err := auth.HashPassword(*input.AccessPassword)
		if err != nil {
			return LinkDTO{}, err
		}
		passwordHash = hash
	}
	if !rules.RequirePassword {
		passwordHash = nil
	}
	if rules.RequirePassword && passwordHash == nil {
		return LinkDTO{}, problem(422, "access_password_required", "启用口令保护时必须提供访问口令")
	}
	var policyValue any
	var policyJSON []byte
	if rules.RequirePassword {
		policyJSON, err = json.Marshal(rules.WrongPasswordPolicy)
		if err != nil {
			return LinkDTO{}, err
		}
		policyValue = string(policyJSON)
	}
	oldPolicy := ""
	if r.RequirePassword && len(r.PolicyJSON) > 0 {
		var previous PasswordPolicy
		if json.Unmarshal(r.PolicyJSON, &previous) == nil {
			oldPolicy = previous.Strategy
		}
	}
	newPolicy := ""
	if rules.WrongPasswordPolicy != nil {
		newPolicy = rules.WrongPasswordPolicy.Strategy
	}
	expiresBefore, expiresAfter := timePointer(r.ExpiresAt), timePointer(expiresAt)
	if _, err := tx.ExecContext(ctx, `UPDATE short_links SET expires_at=$2,max_visits=$3,allowed_ip_cidrs=$4::cidr[],require_password=$5,password_hash=$6,wrong_password_policy=$7::jsonb,updated_at=$8,active_from=$9,active_until=$10,visits_cycle=CASE WHEN max_visits<>$3 THEN visits_cycle+1 ELSE visits_cycle END WHERE link_id=$1`, id, expiresAfter, rules.MaxVisits, cidrArray(rules.AllowedIPCIDRs), rules.RequirePassword, passwordHash, policyValue, now, timePointer(activeFrom), timePointer(activeUntil)); err != nil {
		return LinkDTO{}, err
	}
	if err := audit(ctx, tx, actor.UserID, r.WorkspaceID, "link.rules.update", "short_link", id.String(), meta, map[string]any{
		"expiresAtBefore": expiresBefore, "expiresAtAfter": expiresAfter,
		"activeFromBefore": timePointer(r.ActiveFrom), "activeFromAfter": timePointer(activeFrom),
		"activeUntilBefore": timePointer(r.ActiveUntil), "activeUntilAfter": timePointer(activeUntil),
		"maxVisitsBefore": r.MaxVisits, "maxVisitsAfter": rules.MaxVisits,
		"allowedIpCidrsBefore": parseCIDRArray(r.IPCIDRs), "allowedIpCidrsAfter": rules.AllowedIPCIDRs,
		"requirePasswordBefore": r.RequirePassword, "requirePasswordAfter": rules.RequirePassword,
		"wrongPasswordStrategyBefore": oldPolicy, "wrongPasswordStrategyAfter": newPolicy,
	}); err != nil {
		return LinkDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return LinkDTO{}, err
	}
	r.ExpiresAt = expiresAt
	r.ActiveFrom = activeFrom
	r.ActiveUntil = activeUntil
	r.MaxVisits = rules.MaxVisits
	r.IPCIDRs = cidrArray(rules.AllowedIPCIDRs)
	r.RequirePassword = rules.RequirePassword
	r.PolicyJSON = policyJSON
	r.UpdatedAt = now
	if rules.RequirePassword {
		r.PasswordHash = sql.NullString{String: passwordHash.(string), Valid: true}
	} else {
		r.PasswordHash = sql.NullString{}
	}
	return s.dto(r), nil
}

func (s *Service) Renew(ctx context.Context, actor Actor, rawID string, input RenewRequest, meta RequestMeta) (map[string]any, error) {
	// 1. 续期只更新到期时间；仅显式选择 generated 时才更换入口码。
	input.ExpiresAt = input.ExpiresAt.UTC().Truncate(time.Microsecond)
	if !input.ExpiresAt.After(time.Now()) {
		return nil, problem(422, "invalid_expiration", "到期时间必须晚于当前时间")
	}
	if input.CodeStrategy == "" {
		input.CodeStrategy = "keep"
	}
	if input.CodeStrategy != "keep" && input.CodeStrategy != "generated" {
		return nil, problem(422, "invalid_code_strategy", "短码策略无效")
	}
	id, err := parseID("link", rawID)
	if err != nil {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.loadLink(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return nil, err
	}
	if !linkInScope(actor, r) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if !input.ExpiresAt.After(time.Now()) {
		return nil, problem(422, "invalid_expiration", "到期时间必须晚于当前时间")
	}
	if err := validateWindow(r.ActiveFrom, r.ActiveUntil, nullTime(&input.ExpiresAt)); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE short_links SET expires_at=$2,updated_at=now() WHERE link_id=$1`, id, input.ExpiresAt); err != nil {
		return nil, err
	}
	code := r.Code.String
	changed := false
	if input.CodeStrategy == "generated" {
		code, err = s.rotate(ctx, tx, id, "", true)
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if err := audit(ctx, tx, actor.UserID, r.WorkspaceID, "link.renew", "short_link", id.String(), meta, map[string]any{
		"oldCode": r.Code.String, "code": code, "codeChanged": changed, "codeStrategy": input.CodeStrategy, "expiresAt": input.ExpiresAt.UTC(),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"linkId": publicID("link", id), "code": code, "url": s.linkURL(code), "expiresAt": input.ExpiresAt.UTC(), "codeChanged": changed, "visitsUsed": r.VisitsUsed, "maxVisits": r.MaxVisits}, nil
}

func (s *Service) Rotate(ctx context.Context, actor Actor, rawID string, input RotateRequest, meta RequestMeta) (map[string]any, error) {
	// 1. 轮换只撤销旧码并分配新码，不改变短链目标和规则。
	id, err := parseID("link", rawID)
	if err != nil {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if input.Strategy != "generated" && input.Strategy != "custom" {
		return nil, problem(422, "invalid_strategy", "短码策略无效")
	}
	custom := strings.ToLower(strings.TrimSpace(input.Code))
	if input.Strategy == "custom" && !codePattern.MatchString(custom) {
		return nil, problem(422, "invalid_code", "自定义短码格式无效")
	}
	if input.Strategy == "generated" && input.Code != "" {
		return nil, problem(422, "invalid_code", "自动生成短码不能同时传入 code")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.loadLink(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return nil, err
	}
	if !linkInScope(actor, r) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	code, err := s.rotate(ctx, tx, id, custom, input.Strategy == "generated")
	if err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, actor.UserID, r.WorkspaceID, "link.rotate_code", "short_link", id.String(), meta, map[string]any{
		"oldCode": r.Code.String, "code": code, "strategy": input.Strategy,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"linkId": publicID("link", id), "oldCode": r.Code.String, "code": code, "url": s.linkURL(code), "codeChanged": true}, nil
}

func (s *Service) SetStatus(ctx context.Context, actor Actor, rawID, status string, meta RequestMeta) (map[string]any, error) {
	// 1. 启停操作按目标短链的工作空间校验，并保持重复请求幂等。
	id, err := parseID("link", rawID)
	if err != nil {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.loadLink(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return nil, err
	}
	if !linkInScope(actor, r) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE short_links SET status=$2,updated_at=now() WHERE link_id=$1`, id, status); err != nil {
		return nil, err
	}
	action := "link.enable"
	if status == "disabled" {
		action = "link.disable"
	}
	if err := audit(ctx, tx, actor.UserID, r.WorkspaceID, action, "short_link", id.String(), meta, map[string]any{"status": status}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r.Status = status
	return map[string]any{"linkId": publicID("link", id), "status": status, "availability": availability(r), "updatedAt": time.Now().UTC()}, nil
}

func (s *Service) ResetVisits(ctx context.Context, actor Actor, rawID string, meta RequestMeta) (map[string]any, error) {
	// 1. 访问计数重置与审计事件在同一事务完成。
	id, err := parseID("link", rawID)
	if err != nil {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.loadLink(ctx, tx, id, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	if err != nil {
		return nil, err
	}
	if !linkInScope(actor, r) {
		return nil, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE short_links SET visits_used=0,visits_cycle=visits_cycle+1,updated_at=$2 WHERE link_id=$1`, id, now); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, actor.UserID, r.WorkspaceID, "link.reset_visits", "short_link", id.String(), meta, map[string]any{"previousVisitsUsed": r.VisitsUsed, "visitsUsed": 0, "maxVisits": r.MaxVisits}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"linkId": publicID("link", id), "visitsUsed": 0, "maxVisits": r.MaxVisits, "resetAt": now}, nil
}

func (s *Service) resolveRules(ctx context.Context, actor Actor, source RuleSourceInput) (Rules, *TemplateRef, error) {
	// 1. 内联规则与模板引用互斥，模板规则在事务里再次校验版本状态。
	if source.Type == "inline" && source.Rules != nil && source.Template == nil {
		return NormalizeRules(*source.Rules), nil, nil
	}
	if source.Type != "template" || source.Template == nil || source.Rules != nil {
		return Rules{}, nil, problem(422, "invalid_rule_source", "规则来源必须是 inline 或 template")
	}
	id, err := parseID("tpl", source.Template.TemplateID)
	if err != nil {
		return Rules{}, nil, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	var workspace uuid.UUID
	var status string
	var deleted sql.NullTime
	var latest int32
	err = s.db.QueryRowContext(ctx, `SELECT workspace_id,status,deleted_at,latest_version FROM rule_templates WHERE template_id=$1`, id).Scan(&workspace, &status, &deleted, &latest)
	if errors.Is(err, sql.ErrNoRows) || (actor.Role != "admin" && workspace != actor.WorkspaceID) {
		return Rules{}, nil, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	if err != nil {
		return Rules{}, nil, err
	}
	if deleted.Valid || status != "active" {
		return Rules{}, nil, problem(409, "template_unavailable", "规则模板已停用或删除")
	}
	version := latest
	if source.Template.Version != nil {
		version = *source.Template.Version
	}
	var raw []byte
	err = s.db.QueryRowContext(ctx, `SELECT rules FROM rule_template_versions WHERE template_id=$1 AND version=$2`, id, version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Rules{}, nil, problem(404, "template_version_not_found", "规则模板版本不存在")
	}
	if err != nil {
		return Rules{}, nil, err
	}
	var rules Rules
	if err := json.Unmarshal(raw, &rules); err != nil {
		return Rules{}, nil, fmt.Errorf("读取模板规则失败: %w", err)
	}
	ref := &TemplateRef{TemplateID: publicID("tpl", id), Version: &version}
	return rules, ref, nil
}

func NormalizeRules(input RulesInput) Rules {
	maxVisits := -int64(1)
	if input.MaxVisits != nil {
		maxVisits = *input.MaxVisits
	}
	policy := input.WrongPasswordPolicy
	if input.RequirePassword {
		if policy == nil {
			policy = &PasswordPolicy{Strategy: "fixed_cooldown"}
		}
	}
	return Rules{MaxVisits: maxVisits, AllowedIPCIDRs: input.AllowedIPCIDRs, RequirePassword: input.RequirePassword, WrongPasswordPolicy: policy}
}

func ValidateRules(rules Rules) error {
	if rules.MaxVisits < -1 {
		return problem(422, "invalid_rules", "maxVisits 只能为 -1、0 或正整数")
	}
	for _, raw := range rules.AllowedIPCIDRs {
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return problem(422, "invalid_rules", "allowedIpCidrs 中包含无效网段")
		}
	}
	if rules.RequirePassword && (rules.WrongPasswordPolicy == nil || (rules.WrongPasswordPolicy.Strategy != "fixed_cooldown" && rules.WrongPasswordPolicy.Strategy != "progressive_cooldown")) {
		return problem(422, "invalid_rules", "口令错误策略无效")
	}
	if !rules.RequirePassword && rules.WrongPasswordPolicy != nil {
		return problem(422, "invalid_rules", "未启用口令时不能设置口令错误策略")
	}
	return nil
}

func (s *Service) insertCode(ctx context.Context, tx *sql.Tx, linkID uuid.UUID, requested string) (string, error) {
	// 1. 自动短码发生唯一冲突时重试；自定义冲突明确返回 409。
	for attempt := 0; attempt < 8; attempt++ {
		code := requested
		if code == "" {
			var err error
			code, err = generateCode()
			if err != nil {
				return "", err
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO short_link_codes(code,link_id,status) VALUES($1,$2,'active') ON CONFLICT(code) DO NOTHING`, code, linkID)
		if err != nil {
			return "", err
		}
		rows, _ := result.RowsAffected()
		if rows == 1 {
			return code, nil
		}
		if requested != "" {
			return "", problem(409, "short_code_conflict", "短码已被使用")
		}
	}
	return "", problem(503, "code_generation_failed", "暂时无法生成唯一短码")
}

func (s *Service) rotate(ctx context.Context, tx *sql.Tx, linkID uuid.UUID, requested string, generated bool) (string, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE short_link_codes SET status='revoked',revoked_at=now() WHERE link_id=$1 AND status='active'`, linkID); err != nil {
		return "", err
	}
	if generated {
		requested = ""
	}
	return s.insertCode(ctx, tx, linkID, requested)
}

func (s *Service) loadLink(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID, lock bool) (linkRecord, error) {
	// 1. 管理视图一次读取目标、当前码、素材大小和不可变规则字段。
	suffix := ""
	if lock {
		suffix = " FOR UPDATE OF sl"
	}
	var r linkRecord
	var tagsJSON []byte
	err := q.QueryRowContext(ctx, `SELECT sl.link_id,sl.workspace_id,COALESCE(sl.business_id,a.business_id,''),sl.target_type,sl.target_url,sl.asset_id,a.size_bytes,a.bucket,a.object_key,a.original_name,a.content_type,sl.expires_at,sl.max_visits,sl.visits_used,sl.allowed_ip_cidrs::text,sl.require_password,sl.password_hash,sl.wrong_password_policy,sl.status,sl.template_id,sl.template_version,sc.code,sc.status,sl.created_at,sl.updated_at,sl.title,sl.tags::text,sl.group_id,g.name,sl.active_from,sl.active_until FROM short_links sl LEFT JOIN assets a ON a.asset_id=sl.asset_id LEFT JOIN short_link_codes sc ON sc.link_id=sl.link_id AND sc.status='active' LEFT JOIN link_groups g ON g.group_id=sl.group_id AND g.workspace_id=sl.workspace_id WHERE sl.link_id=$1`+suffix, id).Scan(&r.ID, &r.WorkspaceID, &r.BusinessID, &r.TargetType, &r.TargetURL, &r.AssetID, &r.AssetSize, &r.Bucket, &r.ObjectKey, &r.OriginalName, &r.ContentType, &r.ExpiresAt, &r.MaxVisits, &r.VisitsUsed, &r.IPCIDRs, &r.RequirePassword, &r.PasswordHash, &r.PolicyJSON, &r.Status, &r.TemplateID, &r.TemplateVersion, &r.Code, &r.CodeStatus, &r.CreatedAt, &r.UpdatedAt, &r.Title, &tagsJSON, &r.GroupID, &r.GroupName, &r.ActiveFrom, &r.ActiveUntil)
	if err == nil {
		r.Tags = []string{}
		err = json.Unmarshal(tagsJSON, &r.Tags)
	}
	return r, err
}

func (s *Service) dto(r linkRecord) LinkDTO {
	// 1. API DTO 只暴露业务数据，不序列化数据库行或 MinIO 内部地址。
	target := TargetDTO{Type: r.TargetType}
	if r.TargetURL.Valid {
		target.URL = r.TargetURL.String
	}
	if r.AssetID.Valid {
		if id, err := uuid.Parse(r.AssetID.String); err == nil {
			target.AssetID = publicID("ast", id)
		}
	}
	if r.AssetSize.Valid {
		value := r.AssetSize.Int64
		target.AssetSizeBytes = &value
	}
	rules := Rules{MaxVisits: r.MaxVisits, AllowedIPCIDRs: parseCIDRArray(r.IPCIDRs), RequirePassword: r.RequirePassword}
	if len(r.PolicyJSON) > 0 {
		_ = json.Unmarshal(r.PolicyJSON, &rules.WrongPasswordPolicy)
	}
	expires := timePointer(r.ExpiresAt)
	var ref *TemplateRef
	if r.TemplateID.Valid {
		if id, err := uuid.Parse(r.TemplateID.String); err == nil {
			version := int32(r.TemplateVersion.Int64)
			ref = &TemplateRef{TemplateID: publicID("tpl", id), Version: &version}
		}
	}
	code := ""
	if r.Code.Valid {
		code = r.Code.String
	}
	groupID, groupName := publicGroupFields(r.GroupID, r.GroupName)
	return LinkDTO{LinkID: publicID("link", r.ID), BusinessID: r.BusinessID, Title: r.Title, Tags: r.Tags, GroupID: groupID, GroupName: groupName, Code: code, URL: s.linkURL(code), Target: target, Status: r.Status, Availability: availability(r), Rules: rules, ExpiresAt: expires, ActiveFrom: timePointer(r.ActiveFrom), ActiveUntil: timePointer(r.ActiveUntil), VisitsUsed: r.VisitsUsed, RuleTemplate: ref, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

func validAccessPassword(password string) bool {
	return len(password) <= 1024 && strings.TrimSpace(password) != ""
}

func linkInScope(actor Actor, record linkRecord) bool {
	if actor.Role != "admin" && record.WorkspaceID != actor.WorkspaceID {
		return false
	}
	return actor.BusinessID == "" || record.BusinessID == actor.BusinessID
}

func (s *Service) Access(ctx context.Context, rawCode string, meta RequestMeta, password string, submit bool) (PublicResult, error) {
	// 1. 通过历史短码记录定位资源；未知码进入独立一分钟聚合安全事件。
	code := strings.ToLower(strings.TrimSpace(rawCode))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublicResult{}, err
	}
	defer tx.Rollback()
	var linkID uuid.UUID
	var codeStatus string
	err = tx.QueryRowContext(ctx, `SELECT link_id FROM short_link_codes WHERE code=$1`, code).Scan(&linkID)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		if err := s.recordUnknown(ctx, code, meta); err != nil {
			return PublicResult{}, err
		}
		return page(404, "短链不存在", "此短链不存在或输入有误。"), nil
	}
	if err != nil {
		return PublicResult{}, err
	}
	r, err := s.loadLink(ctx, tx, linkID, true)
	if err != nil {
		return PublicResult{}, err
	}
	// 1.1. 先锁定短链主记录，再读取当前短码状态，与轮换事务保持统一锁顺序。
	if err := tx.QueryRowContext(ctx, `SELECT status FROM short_link_codes WHERE code=$1 AND link_id=$2`, code, linkID).Scan(&codeStatus); errors.Is(err, sql.ErrNoRows) {
		codeStatus = "revoked"
	} else if err != nil {
		return PublicResult{}, err
	}
	if reason := precheckAccessReason(r, codeStatus, meta.SourceIP, s.accessNow()); reason != "" {
		result, err := s.denyAccess(ctx, tx, r, meta, reason)
		if err != nil {
			return PublicResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PublicResult{}, err
		}
		return result, nil
	}
	if submit && r.RequirePassword {
		result, err := s.passwordAccess(ctx, tx, r, codeStatus, meta, password)
		if err != nil {
			return PublicResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PublicResult{}, err
		}
		return result, nil
	}
	if r.RequirePassword && !submit {
		if _, err := s.logAccess(ctx, tx, r, meta, "challenge", "password_required", false, "link_access"); err != nil {
			return PublicResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PublicResult{}, err
		}
		return PublicResult{StatusCode: 200, PageTitle: "需要访问口令", PageText: "请输入分享者提供的访问口令。", Challenge: true}, nil
	}
	result, err := s.consumeAccess(ctx, tx, r, codeStatus, meta)
	if err != nil {
		return PublicResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublicResult{}, err
	}
	return result, nil
}

func (s *Service) passwordAccess(ctx context.Context, tx *sql.Tx, r linkRecord, codeStatus string, meta RequestMeta, password string) (PublicResult, error) {
	// 1. 口令错误和冷却只写密码尝试日志，不消耗短链 visits_used。
	if reason := precheckAccessReason(r, codeStatus, meta.SourceIP, s.accessNow()); reason != "" {
		return s.denyAccess(ctx, tx, r, meta, reason)
	}
	ip := normalizedIP(meta.SourceIP)
	var window time.Time
	var failures, level int
	var lastFailed time.Time
	var locked sql.NullTime
	_, err := tx.ExecContext(ctx, `INSERT INTO short_link_password_attempt_state(link_id,source_ip,window_started_at,failed_attempts,last_failed_at,cooldown_level,updated_at) VALUES($1,$2::inet,now(),0,now(),0,now()) ON CONFLICT(link_id,source_ip) DO NOTHING`, r.ID, ip)
	if err != nil {
		return PublicResult{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT window_started_at,failed_attempts,last_failed_at,cooldown_level,locked_until FROM short_link_password_attempt_state WHERE link_id=$1 AND source_ip=$2::inet FOR UPDATE`, r.ID, ip).Scan(&window, &failures, &lastFailed, &level, &locked)
	if err != nil {
		return PublicResult{}, err
	}
	now := time.Now().UTC()
	if locked.Valid && locked.Time.After(now) {
		if _, err := s.logAccess(ctx, tx, r, meta, "denied", "password_cooldown", false, "password_attempt"); err != nil {
			return PublicResult{}, err
		}
		return page(429, "请稍后再试", "口令尝试次数过多，请稍后重试。"), nil
	}
	if !locked.Valid && !lastFailed.IsZero() && now.Sub(lastFailed) >= 24*time.Hour {
		level = 0
		failures = 0
		window = now
	}
	if auth.VerifyPassword(password, r.PasswordHash.String) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM short_link_password_attempt_state WHERE link_id=$1 AND source_ip=$2::inet`, r.ID, ip); err != nil {
			return PublicResult{}, err
		}
		return s.consumeAccess(ctx, tx, r, codeStatus, meta)
	}
	if now.Sub(window) >= 15*time.Minute {
		failures = 0
		window = now
	}
	failures++
	var lockedUntil any
	if failures >= 5 {
		cooldown := 15 * time.Minute
		if policyProgressive(r.PolicyJSON) {
			schedule := []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
			if level > 3 {
				level = 3
			}
			cooldown = schedule[level]
			if level < 3 {
				level++
			}
		}
		lockedUntil = now.Add(cooldown)
		failures = 0
		window = now
	}
	if _, err := tx.ExecContext(ctx, `UPDATE short_link_password_attempt_state SET window_started_at=$3,failed_attempts=$4,last_failed_at=$5,cooldown_level=$6,locked_until=$7,updated_at=$5 WHERE link_id=$1 AND source_ip=$2::inet`, r.ID, ip, window, failures, now, level, lockedUntil); err != nil {
		return PublicResult{}, err
	}
	if _, err := s.logAccess(ctx, tx, r, meta, "denied", "password_invalid", false, "password_attempt"); err != nil {
		return PublicResult{}, err
	}
	return PublicResult{StatusCode: 200, PageTitle: "口令错误", PageText: "访问口令不正确，请检查后重试。", Challenge: true}, nil
}

func (s *Service) consumeAccess(ctx context.Context, tx *sql.Tx, r linkRecord, codeStatus string, meta RequestMeta) (PublicResult, error) {
	// 1. 在短链行锁内按原有优先级检查规则；拒绝只写日志，不消耗访问次数。
	if reason := precheckAccessReason(r, codeStatus, meta.SourceIP, s.accessNow()); reason != "" {
		return s.denyAccess(ctx, tx, r, meta, reason)
	}

	// 2. 先准备可用的 302 Location，素材删除、未配置存储或签名失败均不扣次。
	location := ""
	if r.TargetType == "url" && r.TargetURL.Valid {
		location = r.TargetURL.String
	}
	if r.TargetType == "minio_asset" && r.AssetID.Valid {
		var assetAvailable bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE asset_id=$1 AND deleted_at IS NULL AND purged_at IS NULL)`, r.AssetID.String).Scan(&assetAvailable); err != nil {
			return PublicResult{}, err
		}
		if assetAvailable && r.Bucket.Valid && r.ObjectKey.Valid && s.store != nil {
			signed, err := s.store.PresignedGet(ctx, r.Bucket.String, r.ObjectKey.String, 15*time.Minute, r.ContentType.String, r.OriginalName.String)
			if err == nil {
				location = signed
			}
		}
	}
	parsed, err := url.ParseRequestURI(location)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && (r.TargetType != "minio_asset" || parsed.Scheme != "http")) {
		if _, err := s.logAccess(ctx, tx, r, meta, "denied", "asset_unavailable", false, "link_access"); err != nil {
			return PublicResult{}, err
		}
		return page(503, "目标暂不可用", "链接目标当前无法访问，请稍后重试。"), nil
	}

	// 3. 签名可能跨过截止时间，在扣次前重新检查当前可用性。
	if reason := precheckAccessReason(r, codeStatus, meta.SourceIP, s.accessNow()); reason != "" {
		return s.denyAccess(ctx, tx, r, meta, reason)
	}

	// 4. 计数与放行事实共用事务，调用方提交成功后才能返回 302。
	if _, err := tx.ExecContext(ctx, `UPDATE short_links SET visits_used=visits_used+1 WHERE link_id=$1`, r.ID); err != nil {
		return PublicResult{}, err
	}
	if _, err := s.logAccess(ctx, tx, r, meta, "allowed", "", true, "link_access"); err != nil {
		return PublicResult{}, err
	}
	return PublicResult{StatusCode: 302, Location: location}, nil
}

func (s *Service) logAccess(ctx context.Context, tx *sql.Tx, r linkRecord, meta RequestMeta, result, reason string, consumes bool, eventType string) (int64, error) {
	// 1. 访问事实与计数共用事务；source IP 保留原始值，口令明文绝不入库。
	var business any
	if r.AssetID.Valid {
		if err := tx.QueryRowContext(ctx, `SELECT business_id FROM assets WHERE asset_id=$1`, r.AssetID.String).Scan(&business); err != nil {
			return 0, err
		}
	}
	var eventID int64
	err := tx.QueryRowContext(ctx, `INSERT INTO short_link_access_logs(link_id,workspace_id,business_id,event_type,source_ip,user_agent,result,reason,consumes_visit,request_id) VALUES($1,$2,$3,$4,NULLIF($5,'')::inet,$6,$7,NULLIF($8,''),$9,NULLIF($10,'')) RETURNING event_id`, r.ID, r.WorkspaceID, business, eventType, meta.SourceIP, meta.UserAgent, result, reason, consumes, meta.RequestID).Scan(&eventID)
	return eventID, err
}

func (s *Service) recordUnknown(ctx context.Context, code string, meta RequestMeta) error {
	// 1. 未知短码保存 HMAC 指纹，并按来源 IP 与一分钟窗口合并重复事件。
	now := time.Now().UTC().Truncate(accessLogWindow)
	mac := hmac.New(sha256.New, []byte(s.cfg.ShortCodeHMACKey))
	_, _ = mac.Write([]byte(code))
	fingerprint := hex.EncodeToString(mac.Sum(nil))
	ip := normalizedIP(meta.SourceIP)
	_, err := s.db.ExecContext(ctx, `INSERT INTO unknown_short_code_events(code_fingerprint,window_started_at,first_occurred_at,last_occurred_at,event_count,source_ip,user_agent,request_id) VALUES($1,$2,$3,$3,1,$4::inet,$5,NULLIF($6,'')) ON CONFLICT(source_ip,code_fingerprint,window_started_at) DO UPDATE SET event_count=unknown_short_code_events.event_count+1,last_occurred_at=EXCLUDED.last_occurred_at`, fingerprint, now, now, ip, meta.UserAgent, meta.RequestID)
	return err
}

func (s *Service) linkURL(code string) string {
	if code == "" {
		return ""
	}
	return s.cfg.PublicBaseURL + "/r/" + url.PathEscape(code)
}

func availability(r linkRecord) string {
	return availabilityAt(r, time.Now())
}

func availabilityAt(r linkRecord, now time.Time) string {
	if r.Status == "disabled" {
		return "disabled"
	}
	if r.ExpiresAt.Valid && !r.ExpiresAt.Time.After(now) {
		return "expired"
	}
	if r.ActiveUntil.Valid && !r.ActiveUntil.Time.After(now) {
		return "window_ended"
	}
	if r.ActiveFrom.Valid && r.ActiveFrom.Time.After(now) {
		return "not_started"
	}
	if r.MaxVisits >= 0 && r.VisitsUsed >= r.MaxVisits {
		return "visit_limit_reached"
	}
	return "available"
}

func precheckAccessReason(r linkRecord, codeStatus, sourceIP string, now time.Time) string {
	if codeStatus != "active" {
		return "code_revoked"
	}
	if state := availabilityAt(r, now); state != "available" {
		return state
	}
	if !ipAllowed(r.IPCIDRs, sourceIP) {
		return "ip_denied"
	}
	return ""
}

func (s *Service) denyAccess(ctx context.Context, tx *sql.Tx, r linkRecord, meta RequestMeta, reason string) (PublicResult, error) {
	if _, err := s.logAccess(ctx, tx, r, meta, "denied", reason, false, "link_access"); err != nil {
		return PublicResult{}, err
	}
	status := 410
	if reason == "not_started" || reason == "visit_limit_reached" || reason == "ip_denied" {
		status = 403
	}
	return page(status, publicReason(reason), publicMessage(reason)), nil
}

func validateWindow(activeFrom, activeUntil, expiresAt sql.NullTime) error {
	deadline := expiresAt
	if activeUntil.Valid && (!deadline.Valid || activeUntil.Time.Before(deadline.Time)) {
		deadline = activeUntil
	}
	if activeFrom.Valid && deadline.Valid && !activeFrom.Time.Before(deadline.Time) {
		return problem(422, "invalid_window", "开放开始时间必须早于到期时间与开放结束时间中较早者")
	}
	return nil
}

func mergeTimePatch(raw json.RawMessage, current sql.NullTime) (sql.NullTime, error) {
	if len(raw) == 0 {
		return current, nil
	}
	var requested *time.Time
	if err := json.Unmarshal(raw, &requested); err != nil {
		return sql.NullTime{}, err
	}
	return nullTime(requested), nil
}

func nullTime(value *time.Time) sql.NullTime {
	if value == nil {
		return sql.NullTime{}
	}
	// timestamptz 仅保留微秒，按实际写入值校验并返回时间。
	return sql.NullTime{Time: value.UTC().Truncate(time.Microsecond), Valid: true}
}

func timePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	utc := value.Time.UTC()
	return &utc
}

func page(status int, title, text string) PublicResult {
	return PublicResult{StatusCode: status, PageTitle: title, PageText: text}
}

func publicReason(reason string) string {
	switch reason {
	case "disabled":
		return "短链已停用"
	case "expired":
		return "短链已过期"
	case "window_ended":
		return "短链开放窗口已结束"
	case "not_started":
		return "短链尚未开放"
	case "visit_limit_reached":
		return "访问次数已用尽"
	case "ip_denied":
		return "当前网络不允许访问"
	case "code_revoked":
		return "短链入口已失效"
	default:
		return "无法访问此短链"
	}
}
func publicMessage(reason string) string {
	switch reason {
	case "disabled":
		return "分享者已停用此短链。"
	case "expired":
		return "此短链已超过有效期。"
	case "window_ended":
		return "此短链的开放窗口已结束。"
	case "not_started":
		return "此短链尚未到开放时间。"
	case "visit_limit_reached":
		return "此短链已达到访问次数限制。"
	case "ip_denied":
		return "当前网络地址不在允许访问范围内。"
	case "code_revoked":
		return "此短码已被更换或撤销。"
	default:
		return "此短链当前不可用。"
	}
}

func ipAllowed(raw, source string) bool {
	if strings.Trim(raw, "{} ") == "" {
		return true
	}
	ip := net.ParseIP(normalizedIP(source))
	if ip == nil {
		return false
	}
	for _, part := range strings.Split(strings.Trim(raw, "{}"), ",") {
		_, network, err := net.ParseCIDR(strings.Trim(part, " \""))
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
func parseCIDRArray(raw string) []string {
	raw = strings.Trim(raw, "{}")
	if raw == "" {
		return []string{}
	}
	values := strings.Split(raw, ",")
	for i := range values {
		values[i] = strings.Trim(values[i], " \"")
	}
	return values
}

func normalizeMetadata(title string, tags []string) (string, []string, error) {
	// 1. 限制展示字段长度，并按不区分大小写去重标签。
	title = strings.TrimSpace(title)
	if len([]rune(title)) > 120 {
		return "", nil, problem(422, "invalid_metadata", "标题不能超过 120 个字符")
	}
	if len(tags) > 20 {
		return "", nil, problem(422, "invalid_metadata", "每条短链最多添加 20 个标签")
	}
	normalized := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, raw := range tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if len([]rune(tag)) > 32 {
			return "", nil, problem(422, "invalid_metadata", "每个标签不能超过 32 个字符")
		}
		key := strings.ToLower(tag)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, tag)
	}
	return title, normalized, nil
}

func cidrArray(values []string) string {
	if len(values) == 0 {
		return "{}"
	}
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = `"` + value + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}
func nullableJSON(value any) any {
	if value == nil {
		return nil
	}
	return string(value.([]byte))
}
func nullableTemplateID(ref *TemplateRef) any {
	if ref == nil {
		return nil
	}
	id, err := parseID("tpl", ref.TemplateID)
	if err != nil {
		return nil
	}
	return id
}
func nullableTemplateVersion(ref *TemplateRef) any {
	if ref == nil || ref.Version == nil {
		return nil
	}
	return *ref.Version
}
func nullablePublicTemplateID(ref *TemplateRef) any {
	if ref == nil {
		return nil
	}
	return ref.TemplateID
}
func nullablePublicTemplateVersion(ref *TemplateRef) any {
	if ref == nil || ref.Version == nil {
		return nil
	}
	return *ref.Version
}
func policyProgressive(raw []byte) bool {
	var policy PasswordPolicy
	return json.Unmarshal(raw, &policy) == nil && policy.Strategy == "progressive_cooldown"
}
func normalizedIP(raw string) string {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "0.0.0.0"
	}
	return ip.String()
}
func parseID(prefix, value string) (uuid.UUID, error) {
	trimmed, ok := strings.CutPrefix(value, prefix+"_")
	if !ok {
		return uuid.Nil, fmt.Errorf("invalid %s id", prefix)
	}
	return uuid.Parse(trimmed)
}
func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }
func generateCode() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	value := make([]byte, 8)
	for i := range value {
		n, err := rand.Int(rand.Reader, bigInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		value[i] = alphabet[n.Int64()]
	}
	return string(value), nil
}
func bigInt(value int64) *big.Int { return big.NewInt(value) }
func problem(status int, code, message string) error {
	return &Problem{Status: status, Code: code, Message: message}
}
func audit(ctx context.Context, tx *sql.Tx, actor, workspace uuid.UUID, action, resourceType, resourceID string, meta RequestMeta, details ...any) error {
	// 1. 只保存操作需要的白名单元数据；调用方禁止传入口令、凭据或完整敏感 URL。
	raw := []byte(`{}`)
	if len(details) > 0 && details[0] != nil {
		payload := details[0]
		if meta.AuditDetails != nil {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			values := map[string]any{}
			if err := json.Unmarshal(encoded, &values); err != nil {
				return err
			}
			for key, value := range meta.AuditDetails {
				values[key] = value
			}
			payload = values
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::inet,$7,'success',$8::jsonb,NULLIF($9,''))`, actor, workspace, action, resourceType, resourceID, meta.SourceIP, meta.UserAgent, string(raw), meta.RequestID)
	return err
}
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
