package links

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type auditFields map[string]any

func (fields auditFields) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return false
	}
	for key, want := range fields {
		got, present := decoded[key]
		if !present || !reflect.DeepEqual(got, want) {
			return false
		}
	}
	return true
}

func assertLinkProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var p *Problem
	if !errors.As(err, &p) || p.Status != status || p.Code != code {
		t.Fatalf("error = %v; want %d %s", err, status, code)
	}
}

func expectMembershipLock(mock sqlmock.Sqlmock, exclusive bool) {
	query := `SELECT pg_advisory_xact_lock_shared\(\$1\)`
	if exclusive {
		query = `SELECT pg_advisory_xact_lock\(\$1\)`
	}
	mock.ExpectExec(query).WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectGroupAudit(mock sqlmock.Sqlmock, actor Actor, workspace uuid.UUID, action string, id uuid.UUID, fields auditFields) {
	mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(actor.UserID, workspace, action, "link_group", id.String(), "", "", fields, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestMetadataGroupThreeStates(t *testing.T) {
	for _, state := range []string{"omitted", "null", "string", "group_and_metadata"} {
		t.Run(state, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.Title, r.Tags = "original", []string{"keep"}
			oldGroup, newGroup := uuid.New(), uuid.New()
			r.GroupID, r.GroupName = sql.NullString{String: oldGroup.String(), Valid: true}, sql.NullString{String: "Old", Valid: true}
			actor := Actor{UserID: uuid.New(), WorkspaceID: r.WorkspaceID, Role: "user", BusinessID: "canvas", APIKeyID: &newGroup}
			r.BusinessID = "canvas"
			var input UpdateMetadataRequest
			payload := `{"groupId":null}`
			if state == "omitted" {
				payload = `{"title":"changed","tags":["new"]}`
			} else if state == "string" {
				payload = `{"groupId":"` + publicID("grp", newGroup) + `"}`
			} else if state == "group_and_metadata" {
				payload = `{"title":"changed","tags":["new"],"groupId":"` + publicID("grp", newGroup) + `"}`
			}
			if err := json.Unmarshal([]byte(payload), &input); err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			if state == "string" || state == "group_and_metadata" {
				expectMembershipLock(mock, false)
			}
			expectLoadLink(mock, r)
			wantGroup, wantName, wantDBGroup := publicID("grp", oldGroup), "Old", any(oldGroup.String())
			if state == "null" {
				wantGroup, wantName, wantDBGroup = "", "", nil
			} else if state == "string" || state == "group_and_metadata" {
				wantGroup, wantName, wantDBGroup = publicID("grp", newGroup), "New", newGroup.String()
				mock.ExpectQuery(`SELECT name FROM link_groups WHERE group_id=\$1 AND workspace_id=\$2 FOR SHARE`).
					WithArgs(newGroup, r.WorkspaceID).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("New"))
			}
			wantTitle, wantTags := r.Title, `["keep"]`
			if state == "omitted" || state == "group_and_metadata" {
				wantTitle, wantTags = "changed", `["new"]`
			}
			mock.ExpectExec(`UPDATE short_links SET title=\$2,tags=\$3::jsonb,updated_at=\$4,group_id=\$5 WHERE link_id=\$1`).
				WithArgs(r.ID, wantTitle, wantTags, sqlmock.AnyArg(), wantDBGroup).WillReturnResult(sqlmock.NewResult(0, 1))
			var auditAfter any
			if wantGroup != "" {
				auditAfter = wantGroup
			}
			mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(actor.UserID, r.WorkspaceID, "link.metadata.update", "short_link", r.ID.String(), "", "",
				auditFields{"groupIdBefore": publicID("grp", oldGroup), "groupIdAfter": auditAfter, "titleBefore": r.Title, "titleAfter": wantTitle}, "").
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			got, err := s.UpdateMetadata(context.Background(), actor, publicID("link", r.ID), input, RequestMeta{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != wantTitle || got.Target.URL != r.TargetURL.String || got.Rules.MaxVisits != r.MaxVisits {
				t.Fatalf("metadata changed unrelated fields: %+v", got)
			}
			if wantGroup == "" {
				if got.GroupID != nil || got.GroupName != nil {
					t.Fatalf("null update = %+v", got)
				}
			} else if got.GroupID == nil || *got.GroupID != wantGroup || got.GroupName == nil || *got.GroupName != wantName {
				t.Fatalf("group update = %+v", got)
			}
		})
	}
}

func TestMetadataGroupScopeAndValidation(t *testing.T) {
	for _, payload := range []string{`{}`, `{"title":"one"}`, `{"tags":[]}`, `{"groupId":42}`, `{"groupId":{}}`} {
		t.Run(payload, func(t *testing.T) {
			s, _ := newLinkTestService(t)
			var input UpdateMetadataRequest
			if err := json.Unmarshal([]byte(payload), &input); err != nil {
				t.Fatal(err)
			}
			_, err := s.UpdateMetadata(context.Background(), Actor{}, publicID("link", uuid.New()), input, RequestMeta{})
			var p *Problem
			if !errors.As(err, &p) || p.Status != 422 {
				t.Fatalf("invalid update accepted: %v", err)
			}
		})
	}
	for _, scope := range []string{"workspace", "business", "group_workspace"} {
		t.Run(scope, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.BusinessID = "private"
			group := uuid.New()
			actor := Actor{Role: "user", WorkspaceID: r.WorkspaceID, BusinessID: "private"}
			if scope == "workspace" {
				actor.WorkspaceID = uuid.New()
			} else if scope == "business" {
				actor.BusinessID = "canvas"
			}
			mock.ExpectBegin()
			expectMembershipLock(mock, false)
			expectLoadLink(mock, r)
			if scope == "group_workspace" {
				mock.ExpectQuery(`SELECT name FROM link_groups WHERE group_id=\$1 AND workspace_id=\$2 FOR SHARE`).
					WithArgs(group, r.WorkspaceID).WillReturnError(sql.ErrNoRows)
			}
			mock.ExpectRollback()
			_, err := s.UpdateMetadata(context.Background(), actor, publicID("link", r.ID), UpdateMetadataRequest{GroupID: json.RawMessage(`"` + publicID("grp", group) + `"`)}, RequestMeta{})
			code := "link_not_found"
			if scope == "group_workspace" {
				code = "group_not_found"
			}
			assertLinkProblem(t, err, 404, code)
		})
	}
}

func TestListGroupsScopeAndAPIKeyDenial(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		t.Run(role, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			actor := Actor{Role: role, WorkspaceID: uuid.New()}
			group, created := uuid.New(), time.Now().UTC()
			mock.ExpectQuery(`SELECT count\(\*\) FROM link_groups WHERE workspace_id=\$1`).WithArgs(actor.WorkspaceID).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			mock.ExpectQuery(`SELECT g.group_id,g.workspace_id,g.name,g.description,.* FROM link_groups g WHERE g.workspace_id=\$1 ORDER BY g.updated_at DESC,g.group_id DESC LIMIT \$2 OFFSET \$3`).
				WithArgs(actor.WorkspaceID, 20, 0).WillReturnRows(sqlmock.NewRows([]string{"group", "workspace", "name", "description", "count", "created", "updated"}).AddRow(group, actor.WorkspaceID, "Group", "", 2, created, created))
			page, err := s.ListGroups(context.Background(), actor, 1, 20, nil)
			if err != nil || page.Pagination.Total != 1 || len(page.Items) != 1 || page.Items[0].LinkCount != 2 || page.Items[0].GroupID != publicID("grp", group) {
				t.Fatalf("groups = %+v, %v", page, err)
			}
		})
	}
	s, _ := newLinkTestService(t)
	workspace, key := publicID("ws", uuid.New()), uuid.New()
	_, err := s.ListGroups(context.Background(), Actor{Role: "user"}, 1, 20, map[string]string{"workspaceId": workspace})
	assertLinkProblem(t, err, 403, "workspace_scope_denied")
	_, err = s.CreateGroup(context.Background(), Actor{Role: "user"}, CreateGroupRequest{Name: "Group", WorkspaceID: &workspace}, RequestMeta{})
	assertLinkProblem(t, err, 403, "workspace_scope_denied")
	actor := Actor{Role: "admin", APIKeyID: &key}
	_, err = s.ListGroups(context.Background(), actor, 1, 20, nil)
	assertLinkProblem(t, err, 403, "api_key_forbidden_endpoint")
	_, err = s.CreateGroup(context.Background(), actor, CreateGroupRequest{Name: "Group"}, RequestMeta{})
	assertLinkProblem(t, err, 403, "api_key_forbidden_endpoint")
	_, err = s.UpdateGroup(context.Background(), actor, publicID("grp", uuid.New()), UpdateGroupRequest{}, RequestMeta{})
	assertLinkProblem(t, err, 403, "api_key_forbidden_endpoint")
	_, err = s.DeleteGroup(context.Background(), actor, publicID("grp", uuid.New()), RequestMeta{})
	assertLinkProblem(t, err, 403, "api_key_forbidden_endpoint")
}

func TestCreateGroupQuotaDuplicateAndAudit(t *testing.T) {
	for _, outcome := range []string{"created", "duplicate", "quota"} {
		t.Run(outcome, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			actor := Actor{UserID: uuid.New(), WorkspaceID: uuid.New(), Role: "admin"}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT workspace_id FROM workspaces WHERE workspace_id=\$1 FOR NO KEY UPDATE`).WithArgs(actor.WorkspaceID).
				WillReturnRows(sqlmock.NewRows([]string{"workspace"}).AddRow(actor.WorkspaceID))
			used := 49
			if outcome == "quota" {
				used = 50
			}
			mock.ExpectQuery(`SELECT count\(\*\) FROM link_groups WHERE workspace_id=\$1`).WithArgs(actor.WorkspaceID).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(used))
			if outcome != "quota" {
				insert := mock.ExpectQuery(`INSERT INTO link_groups.*RETURNING created_at,updated_at`).WithArgs(sqlmock.AnyArg(), actor.WorkspaceID, "Group", "description", actor.UserID)
				if outcome == "duplicate" {
					insert.WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "link_groups_workspace_name_uq"})
				} else {
					insert.WillReturnRows(sqlmock.NewRows([]string{"created", "updated"}).AddRow(time.Now(), time.Now()))
					mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(actor.UserID, actor.WorkspaceID, "link_group.create", "link_group", sqlmock.AnyArg(), "", "", auditFields{"nameAfter": "Group", "descriptionChanged": true}, "").
						WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if outcome == "created" {
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			got, err := s.CreateGroup(context.Background(), actor, CreateGroupRequest{Name: " Group ", Description: "description"}, RequestMeta{})
			if outcome == "created" {
				if err != nil || got.Name != "Group" || got.LinkCount != 0 || !strings.HasPrefix(got.GroupID, "grp_") || got.WorkspaceID != publicID("ws", actor.WorkspaceID) {
					t.Fatalf("create = %+v, %v", got, err)
				}
			} else if outcome == "quota" {
				assertLinkProblem(t, err, 409, "group_quota_exceeded")
			} else {
				assertLinkProblem(t, err, 409, "group_name_conflict")
			}
		})
	}
	for _, input := range []CreateGroupRequest{{Name: " "}, {Name: strings.Repeat("名", 121)}, {Name: "ok", Description: strings.Repeat("描", 1001)}} {
		s, _ := newLinkTestService(t)
		_, err := s.CreateGroup(context.Background(), Actor{}, input, RequestMeta{})
		var p *Problem
		if !errors.As(err, &p) || p.Status != 422 {
			t.Fatalf("invalid group accepted: %v", err)
		}
	}
}

func expectLoadGroup(mock sqlmock.Sqlmock, actor Actor, id, workspace uuid.UUID, lock bool) {
	query := `SELECT g.workspace_id,g.name,g.description,g.created_at,g.updated_at FROM link_groups g WHERE g.group_id=\$1 AND \(\$2 OR g.workspace_id=\$3\)`
	if lock {
		query += ` FOR UPDATE OF g`
	}
	mock.ExpectQuery(query).WithArgs(id, actor.Role == "admin", actor.WorkspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"workspace", "name", "description", "created", "updated"}).AddRow(workspace, "Group", "old description", time.Now(), time.Now()))
}

func TestUpdateGroupDuplicateAndAudit(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "updated", true: "duplicate"}[duplicate], func(t *testing.T) {
			s, mock := newLinkTestService(t)
			actor := Actor{UserID: uuid.New(), WorkspaceID: uuid.New(), Role: "user"}
			id := uuid.New()
			mock.ExpectBegin()
			expectLoadGroup(mock, actor, id, actor.WorkspaceID, true)
			mock.ExpectQuery(`SELECT count\(\*\) FROM short_links WHERE group_id=\$1 AND workspace_id=\$2`).WithArgs(id, actor.WorkspaceID).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
			update := mock.ExpectExec(`UPDATE link_groups SET name=\$2,description=\$3,updated_at=\$4 WHERE group_id=\$1`).WithArgs(id, "Renamed", "old description", sqlmock.AnyArg())
			if duplicate {
				update.WillReturnError(&pgconn.PgError{Code: "23505"})
				mock.ExpectRollback()
			} else {
				update.WillReturnResult(sqlmock.NewResult(0, 1))
				expectGroupAudit(mock, actor, actor.WorkspaceID, "link_group.update", id, auditFields{"groupId": publicID("grp", id), "nameBefore": "Group", "nameAfter": "Renamed", "descriptionChanged": false})
				mock.ExpectCommit()
			}
			name := "Renamed"
			got, err := s.UpdateGroup(context.Background(), actor, publicID("grp", id), UpdateGroupRequest{Name: &name}, RequestMeta{})
			if duplicate {
				assertLinkProblem(t, err, 409, "group_name_conflict")
			} else if err != nil || got.Name != name || got.Description != "old description" || got.LinkCount != 3 {
				t.Fatalf("update = %+v, %v", got, err)
			}
		})
	}
}

func TestDeleteGroupOnlyUnlinksAndAuditsAtomically(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "audit_failure"}[failAudit], func(t *testing.T) {
			s, mock := newLinkTestService(t)
			actor := Actor{UserID: uuid.New(), WorkspaceID: uuid.New(), Role: "user"}
			id := uuid.New()
			mock.ExpectBegin()
			expectMembershipLock(mock, true)
			expectLoadGroup(mock, actor, id, actor.WorkspaceID, false)
			mock.ExpectQuery(`SELECT link_id FROM short_links WHERE group_id=\$1 AND workspace_id=\$2 ORDER BY link_id FOR UPDATE`).WithArgs(id, actor.WorkspaceID).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.New()).AddRow(uuid.New()))
			expectLoadGroup(mock, actor, id, actor.WorkspaceID, true)
			mock.ExpectExec(`UPDATE short_links SET group_id=NULL,updated_at=now\(\) WHERE group_id=\$1 AND workspace_id=\$2`).WithArgs(id, actor.WorkspaceID).
				WillReturnResult(sqlmock.NewResult(0, 2))
			mock.ExpectExec(`DELETE FROM link_groups WHERE group_id=\$1 AND workspace_id=\$2`).WithArgs(id, actor.WorkspaceID).
				WillReturnResult(sqlmock.NewResult(0, 1))
			failure := errors.New("audit unavailable")
			if failAudit {
				mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnError(failure)
				mock.ExpectRollback()
			} else {
				expectGroupAudit(mock, actor, actor.WorkspaceID, "link_group.delete", id, auditFields{"groupId": publicID("grp", id), "nameBefore": "Group", "unlinkedCount": float64(2), "deleted": true})
				mock.ExpectCommit()
			}
			got, err := s.DeleteGroup(context.Background(), actor, publicID("grp", id), RequestMeta{})
			if failAudit {
				if !errors.Is(err, failure) {
					t.Fatalf("audit error = %v", err)
				}
			} else if err != nil || !got.Deleted || got.GroupID != publicID("grp", id) || got.UnlinkedCount != 2 {
				t.Fatalf("delete = %+v, %v", got, err)
			}
		})
	}
}

func TestGroupMutationCrossWorkspaceIsNotFound(t *testing.T) {
	for _, method := range []string{"update", "delete"} {
		t.Run(method, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			actor := Actor{Role: "user", WorkspaceID: uuid.New()}
			id := uuid.New()
			mock.ExpectBegin()
			if method == "delete" {
				expectMembershipLock(mock, true)
			}
			mock.ExpectQuery(`SELECT g.workspace_id,g.name,g.description,g.created_at,g.updated_at FROM link_groups g WHERE g.group_id=\$1 AND \(\$2 OR g.workspace_id=\$3\)`).
				WithArgs(id, false, actor.WorkspaceID).WillReturnError(sql.ErrNoRows)
			mock.ExpectRollback()
			var err error
			if method == "delete" {
				_, err = s.DeleteGroup(context.Background(), actor, publicID("grp", id), RequestMeta{})
			} else {
				name := "rename"
				_, err = s.UpdateGroup(context.Background(), actor, publicID("grp", id), UpdateGroupRequest{Name: &name}, RequestMeta{})
			}
			assertLinkProblem(t, err, 404, "group_not_found")
		})
	}
}

func TestCreateAssetLinkGroupUsesFinalWorkspace(t *testing.T) {
	for _, groupAllowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong_workspace", true: "asset_workspace"}[groupAllowed], func(t *testing.T) {
			s, mock := newLinkTestService(t)
			actor := Actor{Role: "admin", UserID: uuid.New(), WorkspaceID: uuid.New()}
			asset, group, assetWorkspace := uuid.New(), uuid.New(), uuid.New()
			groupID := publicID("grp", group)
			mock.ExpectBegin()
			expectMembershipLock(mock, false)
			mock.ExpectQuery(`SELECT role, url_link_quota, minio_asset_link_quota FROM users WHERE user_id=\$1 FOR NO KEY UPDATE`).WithArgs(actor.UserID).
				WillReturnRows(sqlmock.NewRows([]string{"role", "url", "asset"}).AddRow("admin", 100, 100))
			mock.ExpectQuery(`SELECT a.workspace_id, a.size_bytes, a.business_id, .*`).WithArgs(asset, actor.UserID).
				WillReturnRows(sqlmock.NewRows([]string{"workspace", "size", "business", "allowed"}).AddRow(assetWorkspace, 1024, "canvas", true))
			mock.ExpectQuery(`SELECT workspace_id FROM assets WHERE asset_id=\$1`).WithArgs(asset).
				WillReturnRows(sqlmock.NewRows([]string{"workspace"}).AddRow(assetWorkspace))
			validation := mock.ExpectQuery(`SELECT name FROM link_groups WHERE group_id=\$1 AND workspace_id=\$2 FOR SHARE`).WithArgs(group, assetWorkspace)
			if groupAllowed {
				validation.WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Asset group"))
				mock.ExpectExec(`INSERT INTO short_links .*group_id,active_from,active_until\).*`).WithArgs(sqlmock.AnyArg(), assetWorkspace, "canvas", "", "[]", "minio_asset", "", asset, nil, int64(-1), "{}", false, nil, nil, nil, nil, actor.UserID, group, nil, nil).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO short_link_codes`).WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			} else {
				validation.WillReturnError(sql.ErrNoRows)
				mock.ExpectRollback()
			}
			got, err := s.Create(context.Background(), actor, CreateRequest{GroupID: &groupID, Target: TargetInput{Type: "minio_asset", AssetID: publicID("ast", asset)}, RuleSource: RuleSourceInput{Type: "inline", Rules: &RulesInput{}}}, RequestMeta{})
			if !groupAllowed {
				assertLinkProblem(t, err, 404, "group_not_found")
			} else if err != nil || got.GroupID == nil || *got.GroupID != groupID || got.GroupName == nil || *got.GroupName != "Asset group" {
				t.Fatalf("create asset group = %+v, %v", got, err)
			}
		})
	}
}
