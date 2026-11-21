package links

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxLinkGroups = 50

// Membership writers take the shared advisory lock before any user/link row
// locks, then validate the final workspace with FOR SHARE on the group. Deletion
// takes the exclusive advisory lock, then link rows in ID order, then the group.
// This closes the membership set without a group -> link row-lock inversion.
// Quota rows use NO KEY UPDATE so audit/created_by FK KEY SHARE checks cannot
// reverse the user/workspace-to-group row-lock order.
func lockGroupMembership(ctx context.Context, tx *sql.Tx, id uuid.UUID, exclusive bool) error {
	hash := sha256.Sum256([]byte("short_links/link_group/" + id.String()))
	key := int64(binary.BigEndian.Uint64(hash[:8]))
	query := `SELECT pg_advisory_xact_lock_shared($1)`
	if exclusive {
		query = `SELECT pg_advisory_xact_lock($1)`
	}
	_, err := tx.ExecContext(ctx, query, key)
	return err
}

func requireGroupSession(actor Actor) error {
	if actor.APIKeyID != nil || actor.BusinessID != "" {
		return problem(403, "api_key_forbidden_endpoint", "分组管理必须使用人类登录会话")
	}
	return nil
}

func groupWorkspace(actor Actor, raw *string) (uuid.UUID, error) {
	if raw == nil {
		return actor.WorkspaceID, nil
	}
	if actor.Role != "admin" {
		return uuid.Nil, problem(403, "workspace_scope_denied", "只有管理员可以指定工作空间")
	}
	id, err := parseID("ws", *raw)
	if err != nil {
		return uuid.Nil, problem(422, "invalid_workspace_id", "工作空间参数无效")
	}
	return id, nil
}

func normalizeGroup(name, description string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 120 || strings.ContainsRune(name, 0) {
		return "", "", problem(422, "invalid_group_name", "分组名称必须为 1 至 120 个字符")
	}
	if utf8.RuneCountInString(description) > 1000 || strings.ContainsRune(description, 0) {
		return "", "", problem(422, "invalid_group_description", "分组描述不能超过 1000 个字符")
	}
	return name, description, nil
}

func groupWriteError(err error) error {
	if isUniqueViolation(err) {
		return problem(409, "group_name_conflict", "此工作空间已存在同名分组")
	}
	return err
}

func (s *Service) ListGroups(ctx context.Context, actor Actor, page, pageSize int, query map[string]string) (GroupPage, error) {
	if err := requireGroupSession(actor); err != nil {
		return GroupPage{}, err
	}
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return GroupPage{}, problem(422, "invalid_pagination", "分页参数无效")
	}
	var rawWorkspace *string
	if raw, ok := query["workspaceId"]; ok {
		rawWorkspace = &raw
	}
	workspace, err := groupWorkspace(actor, rawWorkspace)
	if err != nil {
		return GroupPage{}, err
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM link_groups WHERE workspace_id=$1`, workspace).Scan(&total); err != nil {
		return GroupPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.group_id,g.workspace_id,g.name,g.description,
		(SELECT count(*) FROM short_links sl WHERE sl.group_id=g.group_id AND sl.workspace_id=g.workspace_id),g.created_at,g.updated_at
		FROM link_groups g WHERE g.workspace_id=$1 ORDER BY g.updated_at DESC,g.group_id DESC LIMIT $2 OFFSET $3`, workspace, pageSize, (page-1)*pageSize)
	if err != nil {
		return GroupPage{}, err
	}
	defer rows.Close()
	items := make([]GroupDTO, 0)
	for rows.Next() {
		var id, workspaceID uuid.UUID
		var item GroupDTO
		if err := rows.Scan(&id, &workspaceID, &item.Name, &item.Description, &item.LinkCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return GroupPage{}, err
		}
		item.GroupID, item.WorkspaceID = publicID("grp", id), publicID("ws", workspaceID)
		item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
		items = append(items, item)
	}
	return GroupPage{Items: items, Pagination: Pagination{Page: page, PageSize: pageSize, Total: total}}, rows.Err()
}

func (s *Service) CreateGroup(ctx context.Context, actor Actor, input CreateGroupRequest, meta RequestMeta) (GroupDTO, error) {
	if err := requireGroupSession(actor); err != nil {
		return GroupDTO{}, err
	}
	name, description, err := normalizeGroup(input.Name, input.Description)
	if err != nil {
		return GroupDTO{}, err
	}
	workspace, err := groupWorkspace(actor, input.WorkspaceID)
	if err != nil {
		return GroupDTO{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GroupDTO{}, err
	}
	defer tx.Rollback()
	var lockedWorkspace uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM workspaces WHERE workspace_id=$1 FOR NO KEY UPDATE`, workspace).Scan(&lockedWorkspace); errors.Is(err, sql.ErrNoRows) {
		return GroupDTO{}, problem(404, "workspace_not_found", "工作空间不存在或无权访问")
	} else if err != nil {
		return GroupDTO{}, err
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM link_groups WHERE workspace_id=$1`, workspace).Scan(&used); err != nil {
		return GroupDTO{}, err
	}
	if used >= maxLinkGroups {
		return GroupDTO{}, problem(409, "group_quota_exceeded", "每个工作空间最多创建 50 个分组")
	}
	id := uuid.New()
	item := GroupDTO{GroupID: publicID("grp", id), WorkspaceID: publicID("ws", workspace), Name: name, Description: description}
	if err := tx.QueryRowContext(ctx, `INSERT INTO link_groups(group_id,workspace_id,name,description,created_by) VALUES($1,$2,$3,$4,$5) RETURNING created_at,updated_at`, id, workspace, name, description, actor.UserID).Scan(&item.CreatedAt, &item.UpdatedAt); err != nil {
		return GroupDTO{}, groupWriteError(err)
	}
	item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
	if err := audit(ctx, tx, actor.UserID, workspace, "link_group.create", "link_group", id.String(), meta, map[string]any{"groupId": item.GroupID, "nameAfter": item.Name, "descriptionChanged": item.Description != ""}); err != nil {
		return GroupDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return GroupDTO{}, err
	}
	return item, nil
}

func loadGroup(ctx context.Context, tx *sql.Tx, actor Actor, id uuid.UUID, lock bool) (GroupDTO, uuid.UUID, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE OF g"
	}
	var item GroupDTO
	var workspace uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT g.workspace_id,g.name,g.description,g.created_at,g.updated_at FROM link_groups g WHERE g.group_id=$1 AND ($2 OR g.workspace_id=$3)`+suffix, id, actor.Role == "admin", actor.WorkspaceID).
		Scan(&workspace, &item.Name, &item.Description, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupDTO{}, uuid.Nil, problem(404, "group_not_found", "分组不存在或无权访问")
	}
	if err != nil {
		return GroupDTO{}, uuid.Nil, err
	}
	item.GroupID, item.WorkspaceID = publicID("grp", id), publicID("ws", workspace)
	item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
	return item, workspace, nil
}

func (s *Service) UpdateGroup(ctx context.Context, actor Actor, rawID string, input UpdateGroupRequest, meta RequestMeta) (GroupDTO, error) {
	if err := requireGroupSession(actor); err != nil {
		return GroupDTO{}, err
	}
	if input.Name == nil && input.Description == nil {
		return GroupDTO{}, problem(422, "empty_update", "至少提供一个分组字段")
	}
	id, err := parseID("grp", rawID)
	if err != nil {
		return GroupDTO{}, problem(404, "group_not_found", "分组不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GroupDTO{}, err
	}
	defer tx.Rollback()
	item, workspace, err := loadGroup(ctx, tx, actor, id, true)
	if err != nil {
		return GroupDTO{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM short_links WHERE group_id=$1 AND workspace_id=$2`, id, workspace).Scan(&item.LinkCount); err != nil {
		return GroupDTO{}, err
	}
	before := item
	if input.Name != nil {
		item.Name = *input.Name
	}
	if input.Description != nil {
		item.Description = *input.Description
	}
	item.Name, item.Description, err = normalizeGroup(item.Name, item.Description)
	if err != nil {
		return GroupDTO{}, err
	}
	item.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE link_groups SET name=$2,description=$3,updated_at=$4 WHERE group_id=$1`, id, item.Name, item.Description, item.UpdatedAt); err != nil {
		return GroupDTO{}, groupWriteError(err)
	}
	if err := audit(ctx, tx, actor.UserID, workspace, "link_group.update", "link_group", id.String(), meta, map[string]any{"groupId": item.GroupID, "nameBefore": before.Name, "nameAfter": item.Name, "descriptionChanged": before.Description != item.Description}); err != nil {
		return GroupDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return GroupDTO{}, err
	}
	return item, nil
}

func (s *Service) DeleteGroup(ctx context.Context, actor Actor, rawID string, meta RequestMeta) (DeleteGroupResponse, error) {
	if err := requireGroupSession(actor); err != nil {
		return DeleteGroupResponse{}, err
	}
	id, err := parseID("grp", rawID)
	if err != nil {
		return DeleteGroupResponse{}, problem(404, "group_not_found", "分组不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteGroupResponse{}, err
	}
	defer tx.Rollback()
	if err := lockGroupMembership(ctx, tx, id, true); err != nil {
		return DeleteGroupResponse{}, err
	}
	_, workspace, err := loadGroup(ctx, tx, actor, id, false)
	if err != nil {
		return DeleteGroupResponse{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT link_id FROM short_links WHERE group_id=$1 AND workspace_id=$2 ORDER BY link_id FOR UPDATE`, id, workspace)
	if err != nil {
		return DeleteGroupResponse{}, err
	}
	for rows.Next() {
		var linkID uuid.UUID
		if err := rows.Scan(&linkID); err != nil {
			rows.Close()
			return DeleteGroupResponse{}, err
		}
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil {
		return DeleteGroupResponse{}, rowErr
	}
	if closeErr != nil {
		return DeleteGroupResponse{}, closeErr
	}
	before, _, err := loadGroup(ctx, tx, actor, id, true)
	if err != nil {
		return DeleteGroupResponse{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE short_links SET group_id=NULL,updated_at=now() WHERE group_id=$1 AND workspace_id=$2`, id, workspace)
	if err != nil {
		return DeleteGroupResponse{}, err
	}
	unlinked, err := result.RowsAffected()
	if err != nil {
		return DeleteGroupResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_groups WHERE group_id=$1 AND workspace_id=$2`, id, workspace); err != nil {
		return DeleteGroupResponse{}, err
	}
	before.LinkCount = unlinked
	if err := audit(ctx, tx, actor.UserID, workspace, "link_group.delete", "link_group", id.String(), meta, map[string]any{"groupId": before.GroupID, "nameBefore": before.Name, "deleted": true, "unlinkedCount": unlinked}); err != nil {
		return DeleteGroupResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteGroupResponse{}, err
	}
	return DeleteGroupResponse{GroupID: publicID("grp", id), UnlinkedCount: unlinked, Deleted: true}, nil
}

func validateLinkGroup(ctx context.Context, tx *sql.Tx, id, workspace uuid.UUID) (sql.NullString, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM link_groups WHERE group_id=$1 AND workspace_id=$2 FOR SHARE`, id, workspace).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, problem(404, "group_not_found", "分组不存在或无权访问")
	}
	return sql.NullString{String: name, Valid: err == nil}, err
}

func publicGroupFields(id, name sql.NullString) (*string, *string) {
	if !id.Valid {
		return nil, nil
	}
	parsed, err := uuid.Parse(id.String)
	if err != nil {
		return nil, nil
	}
	value := publicID("grp", parsed)
	if !name.Valid {
		return &value, nil
	}
	return &value, &name.String
}
