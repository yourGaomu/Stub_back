package business

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourGaomu/short_links/back/internal/platform/objectstore"
)

var businessIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

type Problem struct {
	Status  int
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

type Actor struct {
	UserID     uuid.UUID
	Role       string
	BusinessID string
}
type Meta struct {
	SourceIP     string
	UserAgent    string
	RequestID    string
	AuditDetails map[string]any
}

type BusinessDTO struct {
	BusinessID  string    `json:"businessId"`
	DisplayName string    `json:"displayName"`
	Status      string    `json:"status"`
	BucketName  string    `json:"bucketName"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type ListResponse struct {
	Items      []BusinessDTO `json:"items"`
	Pagination struct {
		Page     int   `json:"page"`
		PageSize int   `json:"pageSize"`
		Total    int64 `json:"total"`
	} `json:"pagination"`
}
type UserBusinessItem struct {
	BusinessID  string    `json:"businessId"`
	DisplayName string    `json:"displayName"`
	GrantedAt   time.Time `json:"grantedAt"`
}
type UserBusinessesResponse struct {
	Items []UserBusinessItem `json:"items"`
}
type CreateRequest struct {
	BusinessID  string `json:"businessId"`
	DisplayName string `json:"displayName"`
	BucketName  string `json:"bucketName"`
}
type UpdateRequest struct {
	DisplayName *string `json:"displayName,omitempty"`
	Status      *string `json:"status,omitempty"`
}
type GrantResponse struct {
	UserID     string    `json:"userId"`
	BusinessID string    `json:"businessId"`
	GrantedAt  time.Time `json:"grantedAt"`
	GrantedBy  string    `json:"grantedBy"`
}

type Service struct {
	db    *sql.DB
	store *objectstore.Store
}

func NewService(db *sql.DB, store *objectstore.Store) *Service { return &Service{db: db, store: store} }

func (s *Service) Me(ctx context.Context, actor Actor) ([]map[string]string, error) {
	// 1. 当前用户只能获得管理员授予的已启用业务；管理员可访问全部业务。
	query := `SELECT bi.business_id,bi.display_name FROM business_integrations bi WHERE bi.status='active' AND ($2='' OR bi.business_id=$2) AND (EXISTS(SELECT 1 FROM users u WHERE u.user_id=$1 AND u.role='admin') OR EXISTS(SELECT 1 FROM user_business_access uba WHERE uba.user_id=$1 AND uba.business_id=bi.business_id AND uba.revoked_at IS NULL)) ORDER BY bi.business_id`
	rows, err := s.db.QueryContext(ctx, query, actor.UserID, actor.BusinessID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]string, 0)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		items = append(items, map[string]string{"businessId": id, "displayName": name})
	}
	return items, rows.Err()
}

func (s *Service) List(ctx context.Context, page, pageSize int, status string) (ListResponse, error) {
	// 1. 管理员维护已登记业务目录，不暴露任何 MinIO 凭据。
	if page < 1 || pageSize < 1 || pageSize > 100 || (status != "" && status != "active" && status != "disabled") {
		return ListResponse{}, problem(422, "invalid_filter", "业务查询参数无效")
	}
	where := "TRUE"
	args := []any{}
	if status != "" {
		args = append(args, status)
		where = "status=$1"
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM business_integrations WHERE `+where, args...).Scan(&total); err != nil {
		return ListResponse{}, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT business_id,display_name,status,bucket_name,created_at,updated_at FROM business_integrations WHERE `+where+fmt.Sprintf(" ORDER BY business_id LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return ListResponse{}, err
	}
	defer rows.Close()
	items := make([]BusinessDTO, 0)
	for rows.Next() {
		var item BusinessDTO
		if err := rows.Scan(&item.BusinessID, &item.DisplayName, &item.Status, &item.BucketName, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return ListResponse{}, err
		}
		item.CreatedAt = item.CreatedAt.UTC()
		item.UpdatedAt = item.UpdatedAt.UTC()
		items = append(items, item)
	}
	result := ListResponse{Items: items}
	result.Pagination.Page = page
	result.Pagination.PageSize = pageSize
	result.Pagination.Total = total
	return result, rows.Err()
}

func (s *Service) Create(ctx context.Context, actor Actor, input CreateRequest, meta Meta) (BusinessDTO, error) {
	// 1. 业务桶必须由运维预先建立；服务只登记，不自动创建桶。
	id := strings.TrimSpace(input.BusinessID)
	name := strings.TrimSpace(input.DisplayName)
	bucket := strings.TrimSpace(input.BucketName)
	if !businessIDPattern.MatchString(id) || name == "" || len([]rune(name)) > 120 || bucket != id {
		return BusinessDTO{}, problem(422, "invalid_business", "业务标识、名称或桶名无效；首期桶名必须等于业务标识")
	}
	if s.store == nil {
		return BusinessDTO{}, problem(503, "storage_unavailable", "MinIO 尚未配置")
	}
	exists, err := s.store.BucketExists(ctx, bucket)
	if err != nil {
		return BusinessDTO{}, fmt.Errorf("检查 MinIO 业务桶失败: %w", err)
	}
	if !exists {
		return BusinessDTO{}, problem(422, "bucket_not_found", "对应 MinIO 桶尚未创建")
	}
	// 2. 登记业务和审计记录在同一事务提交，避免成功返回前留下半套状态。
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BusinessDTO{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO business_integrations(business_id,display_name,status,bucket_name,created_at,updated_at) VALUES($1,$2,'active',$1,$3,$3)`, id, name, now)
	if isUnique(err) {
		return BusinessDTO{}, problem(409, "business_id_or_bucket_conflict", "业务标识或桶名已登记")
	}
	if err != nil {
		return BusinessDTO{}, err
	}
	if err := s.auditTx(ctx, tx, actor.UserID, nil, "admin.business.create", id, meta, map[string]any{
		"businessId": id, "displayName": name, "bucketName": bucket, "status": "active",
	}); err != nil {
		return BusinessDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return BusinessDTO{}, err
	}
	return BusinessDTO{BusinessID: id, DisplayName: name, Status: "active", BucketName: id, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Service) Update(ctx context.Context, actor Actor, id string, input UpdateRequest, meta Meta) (BusinessDTO, error) {
	// 1. 业务标识和桶名创建后固定，只允许调整名称和启停状态。
	if input.DisplayName == nil && input.Status == nil {
		return BusinessDTO{}, problem(422, "empty_update", "至少提供一个可修改字段")
	}
	if input.DisplayName != nil && (strings.TrimSpace(*input.DisplayName) == "" || len([]rune(strings.TrimSpace(*input.DisplayName))) > 120) {
		return BusinessDTO{}, problem(422, "invalid_business", "展示名称无效")
	}
	if input.Status != nil && *input.Status != "active" && *input.Status != "disabled" {
		return BusinessDTO{}, problem(422, "invalid_business_status", "业务状态只能是 active 或 disabled")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BusinessDTO{}, err
	}
	defer tx.Rollback()
	var previousName, previousStatus string
	if err := tx.QueryRowContext(ctx, `SELECT display_name,status FROM business_integrations WHERE business_id=$1 FOR UPDATE`, id).Scan(&previousName, &previousStatus); errors.Is(err, sql.ErrNoRows) {
		return BusinessDTO{}, problem(404, "business_not_found", "业务不存在")
	} else if err != nil {
		return BusinessDTO{}, err
	}
	var item BusinessDTO
	err = tx.QueryRowContext(ctx, `UPDATE business_integrations SET display_name=COALESCE($2,display_name),status=COALESCE($3,status),updated_at=now() WHERE business_id=$1 RETURNING business_id,display_name,status,bucket_name,created_at,updated_at`, id, input.DisplayName, input.Status).Scan(&item.BusinessID, &item.DisplayName, &item.Status, &item.BucketName, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BusinessDTO{}, problem(404, "business_not_found", "业务不存在")
	}
	if err != nil {
		return BusinessDTO{}, err
	}
	if err := s.auditTx(ctx, tx, actor.UserID, nil, "admin.business.update", id, meta, map[string]any{
		"businessId": id, "displayNameBefore": previousName, "displayNameAfter": item.DisplayName,
		"statusBefore": previousStatus, "statusAfter": item.Status,
	}); err != nil {
		return BusinessDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return BusinessDTO{}, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return item, nil
}

func (s *Service) UserAccess(ctx context.Context, userID uuid.UUID) (UserBusinessesResponse, error) {
	// 1. 只列出仍有效且已启用的业务授权。
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE user_id=$1)`, userID).Scan(&exists); err != nil {
		return UserBusinessesResponse{}, err
	}
	if !exists {
		return UserBusinessesResponse{}, problem(404, "user_not_found", "用户不存在")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT bi.business_id,bi.display_name,uba.granted_at FROM user_business_access uba JOIN business_integrations bi ON bi.business_id=uba.business_id WHERE uba.user_id=$1 AND uba.revoked_at IS NULL AND bi.status='active' ORDER BY bi.business_id`, userID)
	if err != nil {
		return UserBusinessesResponse{}, err
	}
	defer rows.Close()
	items := make([]UserBusinessItem, 0)
	for rows.Next() {
		var item UserBusinessItem
		if err := rows.Scan(&item.BusinessID, &item.DisplayName, &item.GrantedAt); err != nil {
			return UserBusinessesResponse{}, err
		}
		item.GrantedAt = item.GrantedAt.UTC()
		items = append(items, item)
	}
	return UserBusinessesResponse{Items: items}, rows.Err()
}

func (s *Service) Grant(ctx context.Context, actor Actor, userID uuid.UUID, businessID string, meta Meta) (GrantResponse, error) {
	// 1. 授权操作同时锁定用户与业务，重复授权通过 upsert 幂等恢复。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GrantResponse{}, err
	}
	defer tx.Rollback()
	var businessActive bool
	var workspace uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT default_workspace_id FROM users WHERE user_id=$1 FOR UPDATE`, userID).Scan(&workspace); errors.Is(err, sql.ErrNoRows) {
		return GrantResponse{}, problem(404, "user_not_found", "用户不存在")
	} else if err != nil {
		return GrantResponse{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM business_integrations WHERE business_id=$1 AND status='active')`, businessID).Scan(&businessActive); err != nil {
		return GrantResponse{}, err
	}
	if !businessActive {
		return GrantResponse{}, problem(404, "business_not_found", "启用中的业务不存在")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_business_access(user_id,business_id,granted_by,granted_at,revoked_at) VALUES($1,$2,$3,$4,NULL) ON CONFLICT(user_id,business_id) DO UPDATE SET granted_by=EXCLUDED.granted_by,granted_at=EXCLUDED.granted_at,revoked_at=NULL`, userID, businessID, actor.UserID, now); err != nil {
		return GrantResponse{}, err
	}
	if err := s.auditTx(ctx, tx, actor.UserID, &workspace, "admin.business_access.grant", businessID+":"+userID.String(), meta, map[string]any{
		"businessId": businessID, "targetUserId": publicID("usr", userID), "granted": true,
	}); err != nil {
		return GrantResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return GrantResponse{}, err
	}
	return GrantResponse{UserID: publicID("usr", userID), BusinessID: businessID, GrantedAt: now, GrantedBy: publicID("usr", actor.UserID)}, nil
}

func (s *Service) Revoke(ctx context.Context, actor Actor, userID uuid.UUID, businessID string, meta Meta) error {
	// 1. 撤销立即生效；不存在的历史授权按幂等成功处理。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workspace uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT default_workspace_id FROM users WHERE user_id=$1`, userID).Scan(&workspace); errors.Is(err, sql.ErrNoRows) {
		return problem(404, "user_not_found", "用户不存在")
	} else if err != nil {
		return err
	}
	var businessExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM business_integrations WHERE business_id=$1)`, businessID).Scan(&businessExists); err != nil {
		return err
	}
	if !businessExists {
		return problem(404, "business_not_found", "业务不存在")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_business_access SET revoked_at=COALESCE(revoked_at,now()) WHERE user_id=$1 AND business_id=$2`, userID, businessID); err != nil {
		return err
	}
	if err := s.auditTx(ctx, tx, actor.UserID, &workspace, "admin.business_access.revoke", businessID+":"+userID.String(), meta, map[string]any{
		"businessId": businessID, "targetUserId": publicID("usr", userID), "granted": false,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) auditTx(ctx context.Context, tx *sql.Tx, actor uuid.UUID, workspace *uuid.UUID, event, target string, meta Meta, details ...map[string]any) error {
	// 1. 审计详情只保存业务标识与变更摘要，不保存 MinIO 凭据或对象 URL。
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
	resourceType := "business_access"
	if strings.HasPrefix(event, "admin.business.") && !strings.HasPrefix(event, "admin.business_access.") {
		resourceType = "business"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::inet,$7,'success',$8::jsonb,NULLIF($9,''))`, actor, workspace, event, resourceType, target, meta.SourceIP, meta.UserAgent, string(raw), meta.RequestID)
	return err
}
func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
func problem(status int, code, message string) error {
	return &Problem{Status: status, Code: code, Message: message}
}
func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }
