package templates

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

type Actor struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	Role        string
}

type Problem struct {
	Status  int
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

type Service struct{ db *sql.DB }

func NewService(db *sql.DB) *Service { return &Service{db: db} }

type CreateResponse struct {
	TemplateID    string     `json:"templateId"`
	WorkspaceID   string     `json:"workspaceId"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	LatestVersion int32      `json:"latestVersion"`
	Version       VersionDTO `json:"version"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type StateResponse struct {
	TemplateID string     `json:"templateId"`
	Status     string     `json:"status"`
	DeletedAt  *time.Time `json:"deletedAt"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	EnabledAt  *time.Time `json:"enabledAt,omitempty"`
	RestoredAt *time.Time `json:"restoredAt,omitempty"`
}

func (s *Service) Create(ctx context.Context, actor Actor, input CreateRequest, meta Meta) (CreateResponse, error) {
	// 1. 校验模板 DTO、规则和工作空间，拒绝客户端越权指定归属。
	name := strings.TrimSpace(input.Name)
	description := strings.TrimSpace(input.Description)
	if name == "" || len([]rune(name)) > 120 || len(description) > 4000 {
		return CreateResponse{}, problem(422, "invalid_template", "模板名称或说明格式无效")
	}
	workspaceID := actor.WorkspaceID
	if input.WorkspaceID == "" {
		return CreateResponse{}, problem(422, "workspace_required", "必须指定模板所属工作空间")
	}
	if input.WorkspaceID != "" {
		parsed, err := parseID("ws", input.WorkspaceID)
		if err != nil {
			return CreateResponse{}, problem(422, "invalid_workspace_id", "工作空间参数无效")
		}
		workspaceID = parsed
	}
	if actor.Role != "admin" && workspaceID != actor.WorkspaceID {
		return CreateResponse{}, problem(403, "workspace_forbidden", "无权管理此工作空间")
	}
	rules := links.NormalizeRules(input.Rules)
	if err := links.ValidateRules(rules); err != nil {
		return CreateResponse{}, err
	}
	rawRules, err := json.Marshal(rules)
	if err != nil {
		return CreateResponse{}, err
	}

	// 2. 在同一事务创建模板、不可变的第一个版本和审计记录。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateResponse{}, err
	}
	defer tx.Rollback()
	var workspaceExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE workspace_id=$1)`, workspaceID).Scan(&workspaceExists); err != nil {
		return CreateResponse{}, err
	}
	if !workspaceExists {
		return CreateResponse{}, problem(404, "workspace_not_found", "工作空间不存在")
	}
	now, templateID := time.Now().UTC(), uuid.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO rule_templates(template_id,workspace_id,name,description,status,latest_version,created_by,updated_by,created_at,updated_at) VALUES($1,$2,$3,$4,'active',1,$5,$5,$6,$6)`, templateID, workspaceID, name, description, actor.UserID, now); err != nil {
		if isUniqueViolation(err) {
			return CreateResponse{}, problem(409, "template_name_conflict", "同一工作空间下已存在同名模板")
		}
		return CreateResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rule_template_versions(template_id,version,rules,created_by,created_at) VALUES($1,1,$2::jsonb,$3,$4)`, templateID, string(rawRules), actor.UserID, now); err != nil {
		return CreateResponse{}, err
	}
	if err := writeAudit(ctx, tx, actor.UserID, workspaceID, "template.create", templateID, meta, map[string]any{
		"templateId": publicID("tpl", templateID), "name": name, "version": 1, "status": "active",
	}); err != nil {
		return CreateResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateResponse{}, err
	}
	return CreateResponse{TemplateID: publicID("tpl", templateID), WorkspaceID: publicID("ws", workspaceID), Name: name, Description: description, Status: "active", LatestVersion: 1, Version: VersionDTO{Version: 1, Rules: rules, CreatedAt: now}, CreatedAt: now}, nil
}

func (s *Service) List(ctx context.Context, actor Actor, page, pageSize int, workspace, status, keyword string, includeDeleted bool) (Page, error) {
	// 1. 普通用户被固定在自己的工作空间；管理员可按工作空间筛选。
	if page < 1 || pageSize < 1 || pageSize > 100 || (status != "" && status != "active" && status != "disabled") {
		return Page{}, problem(422, "invalid_filter", "分页或状态筛选参数无效")
	}
	where := []string{}
	args := []any{}
	add := func(sql string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(sql, len(args)))
	}
	if actor.Role != "admin" {
		if workspace != "" {
			id, err := parseID("ws", workspace)
			if err != nil || id != actor.WorkspaceID {
				return Page{}, problem(404, "workspace_not_found", "工作空间不存在")
			}
		}
		add("t.workspace_id=$%d", actor.WorkspaceID)
	} else if workspace != "" {
		id, err := parseID("ws", workspace)
		if err != nil {
			return Page{}, problem(422, "invalid_workspace_id", "工作空间参数无效")
		}
		add("t.workspace_id=$%d", id)
	}
	if status != "" {
		add("t.status=$%d", status)
	}
	if !includeDeleted {
		where = append(where, "t.deleted_at IS NULL")
	}
	if keyword != "" {
		args = append(args, "%"+strings.TrimSpace(keyword)+"%")
		where = append(where, fmt.Sprintf("(t.name ILIKE $%d OR t.description ILIKE $%d)", len(args), len(args)))
	}
	if len(where) == 0 {
		where = append(where, "TRUE")
	}
	filter := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM rule_templates t WHERE `+filter, args...).Scan(&total); err != nil {
		return Page{}, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	query := `SELECT t.template_id,t.workspace_id,t.name,t.status,t.latest_version,
		(SELECT count(*) FROM short_links sl WHERE sl.template_id=t.template_id),t.updated_at
		FROM rule_templates t WHERE ` + filter + fmt.Sprintf(" ORDER BY t.updated_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	items := make([]ListItem, 0)
	for rows.Next() {
		var templateID, workspaceID uuid.UUID
		var item ListItem
		if err := rows.Scan(&templateID, &workspaceID, &item.Name, &item.Status, &item.LatestVersion, &item.ReferencedLinkCount, &item.UpdatedAt); err != nil {
			return Page{}, err
		}
		item.TemplateID, item.WorkspaceID = publicID("tpl", templateID), publicID("ws", workspaceID)
		item.UpdatedAt = item.UpdatedAt.UTC()
		items = append(items, item)
	}
	return Page{Items: items, Pagination: links.Pagination{Page: page, PageSize: pageSize, Total: total}}, rows.Err()
}

func (s *Service) Get(ctx context.Context, actor Actor, rawID string) (TemplateDTO, error) {
	// 1. 读取模板与最新版规则；模板版本只读，不通过详情接口修改。
	id, err := parseID("tpl", rawID)
	if err != nil {
		return TemplateDTO{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	var dto TemplateDTO
	var templateID, workspaceID uuid.UUID
	var deleted sql.NullTime
	var raw []byte
	err = s.db.QueryRowContext(ctx, `SELECT t.template_id,t.workspace_id,t.name,t.description,t.status,t.latest_version,(SELECT count(*) FROM short_links sl WHERE sl.template_id=t.template_id),t.deleted_at,t.created_at,t.updated_at,v.rules FROM rule_templates t JOIN rule_template_versions v ON v.template_id=t.template_id AND v.version=t.latest_version WHERE t.template_id=$1`, id).
		Scan(&templateID, &workspaceID, &dto.Name, &dto.Description, &dto.Status, &dto.LatestVersion, &dto.ReferencedLinkCount, &deleted, &dto.CreatedAt, &dto.UpdatedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return TemplateDTO{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	if err != nil {
		return TemplateDTO{}, err
	}
	if actor.Role != "admin" && workspaceID != actor.WorkspaceID {
		return TemplateDTO{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	if err := json.Unmarshal(raw, &dto.LatestRules); err != nil {
		return TemplateDTO{}, fmt.Errorf("解析模板规则失败: %w", err)
	}
	dto.TemplateID, dto.WorkspaceID = publicID("tpl", templateID), publicID("ws", workspaceID)
	dto.CreatedAt, dto.UpdatedAt = dto.CreatedAt.UTC(), dto.UpdatedAt.UTC()
	if deleted.Valid {
		value := deleted.Time.UTC()
		dto.DeletedAt = &value
	}
	return dto, nil
}

func (s *Service) Update(ctx context.Context, actor Actor, rawID string, input UpdateRequest, meta Meta) (map[string]any, error) {
	// 1. 只更新展示信息，不改规则版本；至少需要提供一个字段。
	if input.Name == nil && input.Description == nil {
		return nil, problem(422, "empty_update", "至少提供一个可修改字段")
	}
	if input.Name != nil && (strings.TrimSpace(*input.Name) == "" || len([]rune(strings.TrimSpace(*input.Name))) > 120) {
		return nil, problem(422, "invalid_name", "模板名称格式无效")
	}
	if input.Description != nil && len(*input.Description) > 4000 {
		return nil, problem(422, "invalid_description", "模板说明过长")
	}
	id, err := parseID("tpl", rawID)
	if err != nil {
		return nil, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	template, err := lockTemplate(ctx, tx, actor, id)
	if err != nil {
		return nil, err
	}
	if template.deleted.Valid {
		return nil, problem(409, "template_deleted", "模板已删除，不能修改")
	}
	name, description := template.name, template.description
	previousName := name
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
	}
	if input.Description != nil {
		description = *input.Description
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rule_templates SET name=$2,description=$3,updated_by=$4,updated_at=now() WHERE template_id=$1`, id, name, description, actor.UserID); err != nil {
		if isUniqueViolation(err) {
			return nil, problem(409, "template_name_conflict", "同一工作空间下已存在同名模板")
		}
		return nil, err
	}
	if err := writeAudit(ctx, tx, actor.UserID, template.workspaceID, "template.update", id, meta, map[string]any{
		"nameBefore": previousName, "nameAfter": name, "descriptionChanged": description != template.description,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"templateId": publicID("tpl", id), "name": name, "description": description, "status": template.status, "updatedAt": time.Now().UTC()}, nil
}

func (s *Service) CreateVersion(ctx context.Context, actor Actor, rawID string, input VersionRequest, meta Meta) (VersionDTO, error) {
	// 1. 锁定模板主记录后递增版本号，历史版本永不覆盖。
	id, err := parseID("tpl", rawID)
	if err != nil {
		return VersionDTO{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	rules := links.NormalizeRules(input.Rules)
	if err := links.ValidateRules(rules); err != nil {
		return VersionDTO{}, err
	}
	raw, _ := json.Marshal(rules)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return VersionDTO{}, err
	}
	defer tx.Rollback()
	template, err := lockTemplate(ctx, tx, actor, id)
	if err != nil {
		return VersionDTO{}, err
	}
	if template.deleted.Valid {
		return VersionDTO{}, problem(409, "template_deleted", "模板已删除，不能创建新版本")
	}
	version, now := template.latest+1, time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO rule_template_versions(template_id,version,rules,change_note,created_by,created_at) VALUES($1,$2,$3::jsonb,$4,$5,$6)`, id, version, string(raw), strings.TrimSpace(input.ChangeNote), actor.UserID, now); err != nil {
		return VersionDTO{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rule_templates SET latest_version=$2,updated_by=$3,updated_at=$4 WHERE template_id=$1`, id, version, actor.UserID, now); err != nil {
		return VersionDTO{}, err
	}
	if err := writeAudit(ctx, tx, actor.UserID, template.workspaceID, "template.create_version", id, meta, map[string]any{
		"version": version,
	}); err != nil {
		return VersionDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return VersionDTO{}, err
	}
	return VersionDTO{Version: version, Rules: rules, ChangeNote: strings.TrimSpace(input.ChangeNote), CreatedAt: now}, nil
}

func (s *Service) Versions(ctx context.Context, actor Actor, rawID string) (VersionList, error) {
	// 1. 返回不可变版本的元数据，按版本号倒序供用户比较。
	id, err := parseID("tpl", rawID)
	if err != nil {
		return VersionList{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	var workspaceID uuid.UUID
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM rule_templates WHERE template_id=$1`, id).Scan(&workspaceID); errors.Is(err, sql.ErrNoRows) || (err == nil && actor.Role != "admin" && workspaceID != actor.WorkspaceID) {
		return VersionList{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	} else if err != nil {
		return VersionList{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version,change_note,created_at FROM rule_template_versions WHERE template_id=$1 ORDER BY version DESC`, id)
	if err != nil {
		return VersionList{}, err
	}
	defer rows.Close()
	items := make([]VersionListItemDTO, 0)
	for rows.Next() {
		var item VersionListItemDTO
		if err := rows.Scan(&item.Version, &item.ChangeNote, &item.CreatedAt); err != nil {
			return VersionList{}, err
		}
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	return VersionList{TemplateID: publicID("tpl", id), Versions: items}, rows.Err()
}

func (s *Service) Version(ctx context.Context, actor Actor, rawID, rawVersion string) (map[string]any, error) {
	// 1. 版本内容查询仍校验模板工作空间归属。
	id, err := parseID("tpl", rawID)
	if err != nil {
		return nil, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	var version int32
	parsedVersion, err := strconv.Atoi(rawVersion)
	if err != nil || parsedVersion < 1 || parsedVersion > int(^uint32(0)>>1) {
		return nil, problem(422, "invalid_version", "模板版本参数无效")
	}
	version = int32(parsedVersion)
	var workspaceID uuid.UUID
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM rule_templates WHERE template_id=$1`, id).Scan(&workspaceID); errors.Is(err, sql.ErrNoRows) || (err == nil && actor.Role != "admin" && workspaceID != actor.WorkspaceID) {
		return nil, problem(404, "template_not_found", "规则模板不存在或无权访问")
	} else if err != nil {
		return nil, err
	}
	var raw []byte
	var note string
	var created time.Time
	err = s.db.QueryRowContext(ctx, `SELECT rules,change_note,created_at FROM rule_template_versions WHERE template_id=$1 AND version=$2`, id, version).Scan(&raw, &note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "version_not_found", "模板版本不存在")
	}
	if err != nil {
		return nil, err
	}
	var rules links.Rules
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, err
	}
	return map[string]any{"templateId": publicID("tpl", id), "version": version, "rules": rules, "changeNote": note, "createdAt": created.UTC()}, nil
}

func (s *Service) SetState(ctx context.Context, actor Actor, rawID, action string, meta Meta) (StateResponse, error) {
	// 1. 启停、软删除和恢复分别更新独立字段，不引入第三种状态。
	id, err := parseID("tpl", rawID)
	if err != nil {
		return StateResponse{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StateResponse{}, err
	}
	defer tx.Rollback()
	template, err := lockTemplate(ctx, tx, actor, id)
	if err != nil {
		return StateResponse{}, err
	}
	now := time.Now().UTC()
	query, args, event := "", []any{id}, "template."+action
	switch action {
	case "disable":
		query = `UPDATE rule_templates SET status='disabled',updated_by=$2,updated_at=$3 WHERE template_id=$1`
		args = append(args, actor.UserID, now)
	case "enable":
		if template.deleted.Valid {
			return StateResponse{}, problem(409, "template_deleted", "已删除模板必须先恢复")
		}
		query = `UPDATE rule_templates SET status='active',updated_by=$2,updated_at=$3 WHERE template_id=$1`
		args = append(args, actor.UserID, now)
	case "delete":
		query = `UPDATE rule_templates SET deleted_at=COALESCE(deleted_at,$2),updated_by=$3,updated_at=$2 WHERE template_id=$1`
		args = append(args, now, actor.UserID)
	case "restore":
		query = `UPDATE rule_templates SET deleted_at=NULL,updated_by=$2,updated_at=$3 WHERE template_id=$1`
		args = append(args, actor.UserID, now)
	default:
		return StateResponse{}, problem(422, "invalid_action", "模板操作无效")
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return StateResponse{}, err
	}
	status := template.status
	if action == "disable" {
		status = "disabled"
	} else if action == "enable" {
		status = "active"
	}
	if err := writeAudit(ctx, tx, actor.UserID, template.workspaceID, event, id, meta, map[string]any{
		"action": action, "status": status, "deleted": action == "delete" || (action != "restore" && template.deleted.Valid),
	}); err != nil {
		return StateResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return StateResponse{}, err
	}
	result, err := s.Get(ctx, actor, rawID)
	if err != nil {
		return StateResponse{}, err
	}
	response := StateResponse{TemplateID: result.TemplateID, Status: result.Status, DeletedAt: result.DeletedAt}
	switch action {
	case "disable":
		response.DisabledAt = &now
	case "enable":
		response.EnabledAt = &now
	case "restore":
		response.RestoredAt = &now
	}
	return response, nil
}

type lockedTemplate struct {
	workspaceID uuid.UUID
	name        string
	description string
	status      string
	latest      int32
	deleted     sql.NullTime
}

func lockTemplate(ctx context.Context, tx *sql.Tx, actor Actor, id uuid.UUID) (lockedTemplate, error) {
	// 1. 事务锁保证版本号递增、状态变更与删除恢复不会相互覆盖。
	var t lockedTemplate
	err := tx.QueryRowContext(ctx, `SELECT workspace_id,name,description,status,latest_version,deleted_at FROM rule_templates WHERE template_id=$1 FOR UPDATE`, id).Scan(&t.workspaceID, &t.name, &t.description, &t.status, &t.latest, &t.deleted)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && actor.Role != "admin" && t.workspaceID != actor.WorkspaceID) {
		return lockedTemplate{}, problem(404, "template_not_found", "规则模板不存在或无权访问")
	}
	return t, err
}

func writeAudit(ctx context.Context, tx *sql.Tx, actor, workspace uuid.UUID, action string, resource uuid.UUID, meta Meta, details ...map[string]any) error {
	// 1. 模板审计只保存名称、版本与状态摘要，不保存规则正文。
	raw := []byte(`{}`)
	if len(details) > 0 && details[0] != nil {
		payload := details[0]
		if meta.AuditDetails != nil {
			if values, ok := payload.(map[string]any); ok {
				for key, value := range meta.AuditDetails {
					values[key] = value
				}
			}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,'rule_template',$4,NULLIF($5,'')::inet,$6,'success',$7::jsonb,NULLIF($8,''))`, actor, workspace, action, resource.String(), meta.SourceIP, meta.UserAgent, string(raw), meta.RequestID)
	return err
}

func parseID(prefix, value string) (uuid.UUID, error) {
	trimmed, ok := strings.CutPrefix(value, prefix+"_")
	if !ok {
		return uuid.Nil, fmt.Errorf("invalid %s id", prefix)
	}
	return uuid.Parse(trimmed)
}
func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }
func problem(status int, code, message string) error {
	return &Problem{Status: status, Code: code, Message: message}
}
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
