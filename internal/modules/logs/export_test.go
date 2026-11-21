package logs

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func exportTestService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	return NewService(db), mock
}

func TestExportAccessUsesListVisibilityAndGroupFilters(t *testing.T) {
	s, mock := exportTestService(t)
	actor := Actor{UserID: uuid.New(), WorkspaceID: uuid.New(), Role: "user"}
	link, group := uuid.New(), uuid.New()
	from, to := time.Now().Add(-time.Hour).UTC(), time.Now().UTC()
	filter := AccessFilter{From: from.Format(time.RFC3339Nano), To: to.Format(time.RFC3339Nano), Result: "denied", LinkID: publicID("link", link), GroupID: publicID("grp", group)}
	mock.ExpectQuery(`SELECT l.event_id,l.link_id,.*sl.created_by=\$3.*l.link_id=\$4.*sl.group_id=\$5.*l.result=\$6.*ORDER BY l.accessed_at DESC,l.event_id DESC LIMIT \$7`).
		WithArgs(from, to, actor.UserID, link, group, "denied", exportMaxRows+1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "link", "at", "ip", "ua", "result", "reason", "consumes", "request"}).AddRow(1, link, to, "192.0.2.1/32", "=1+1", "denied", "ip_denied", false, "req"))
	var rows [][]string
	count, err := s.ExportAccessCSV(context.Background(), actor, filter, func(row []string) error { rows = append(rows, row); return nil })
	if err != nil || count != 1 || len(rows) != 2 || rows[1][1] != publicID("link", link) || rows[1][5] != "denied" {
		t.Fatalf("count=%d rows=%v err=%v", count, rows, err)
	}
}

func TestExportAuditKeepsVisibilityAndFiltersSensitiveDetails(t *testing.T) {
	s, mock := exportTestService(t)
	actor := Actor{UserID: uuid.New(), WorkspaceID: uuid.New(), Email: "user@example.com", Role: "user"}
	at := time.Now().UTC()
	group := uuid.New()
	mock.ExpectQuery(`SELECT a.event_id,a.actor_user_id,.*a.actor_user_id=\$3 OR a.workspace_id=\$4.*lower\(a.target_id\)=lower\(\$5\).*ORDER BY a.occurred_at DESC,a.event_id DESC LIMIT \$6`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), actor.UserID, actor.WorkspaceID, actor.Email, exportMaxRows+1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "actor", "action", "kind", "target", "at", "ip", "result", "details", "request"}).AddRow(2, actor.UserID, "group.create", "link_group", group.String(), at, nil, "success", `{"groupId":"grp_test","name":"group","activeFromBefore":null,"activeFromAfter":"2026-12-12T09:00:00Z","password":"secret","token":"private"}`, nil))
	var rows [][]string
	count, err := s.ExportAuditCSV(context.Background(), actor, AuditFilter{}, func(row []string) error { rows = append(rows, row); return nil })
	if err != nil || count != 1 || rows[1][5] != publicID("grp", group) {
		t.Fatalf("count=%d rows=%v err=%v", count, rows, err)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(rows[1][9]), &details); err != nil {
		t.Fatal(err)
	}
	if details["password"] != nil || details["token"] != nil || details["name"] != "group" {
		t.Fatalf("unsafe details: %v", details)
	}
	if before, exists := details["activeFromBefore"]; !exists || before != nil || details["activeFromAfter"] != "2026-12-12T09:00:00Z" {
		t.Fatalf("window audit fields missing: %v", details)
	}
}

func TestExportValidationRejectsBeforeDatabase(t *testing.T) {
	s, _ := exportTestService(t)
	actor := Actor{Role: "user", UserID: uuid.New()}
	write := func([]string) error { t.Error("unexpected row"); return nil }
	for _, filter := range []AccessFilter{
		{WorkspaceID: publicID("ws", uuid.New())}, {Result: "bad"}, {GroupID: "invalid"}, {GroupID: publicID("grp", uuid.New()), Ungrouped: true},
		{From: "2026-01-01T00:00:00Z", To: "2026-05-01T00:00:00Z"}, {From: "bad", To: "bad"},
	} {
		_, err := s.ExportAccessCSV(context.Background(), actor, filter, write)
		var problem *Problem
		if !errors.As(err, &problem) || problem.Status != 422 {
			t.Fatalf("filter=%+v err=%v", filter, err)
		}
	}
	if _, err := s.ExportAuditCSV(context.Background(), actor, AuditFilter{ActorUserID: publicID("usr", uuid.New())}, write); err == nil {
		t.Fatal("audit accepted admin scope")
	}
}

func TestExportAccessFailsOnRowLimitAndCallbackError(t *testing.T) {
	for _, mode := range []string{"limit", "writer", "rows_error"} {
		t.Run(mode, func(t *testing.T) {
			s, mock := exportTestService(t)
			link := uuid.New()
			rows := sqlmock.NewRows([]string{"id", "link", "at", "ip", "ua", "result", "reason", "consumes", "request"})
			count := 1
			if mode == "limit" {
				count = exportMaxRows + 1
			}
			for i := 0; i < count; i++ {
				rows.AddRow(i+1, link, time.Now(), nil, nil, "allowed", nil, true, nil)
			}
			if mode == "rows_error" {
				rows.RowError(0, io.ErrUnexpectedEOF)
			}
			mock.ExpectQuery(`SELECT l.event_id,l.link_id,.*LIMIT \$3`).WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), exportMaxRows+1).WillReturnRows(rows)
			writes := 0
			_, err := s.ExportAccessCSV(context.Background(), Actor{Role: "admin"}, AccessFilter{}, func([]string) error {
				writes++
				if mode == "writer" {
					return io.ErrClosedPipe
				}
				return nil
			})
			if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "writer" && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
			if mode == "limit" {
				var p *Problem
				if !errors.As(err, &p) || p.Code != "export_limit_exceeded" || writes != exportMaxRows+1 {
					t.Fatalf("err=%v writes=%d", err, writes)
				}
			}
		})
	}
}

func TestExportAdminFiltersStayParameterized(t *testing.T) {
	actor := Actor{Role: "admin"}
	filter := AuditFilter{Action: "quote' OR TRUE --", BusinessID: "biz", ResourceType: "link_group", ResourceID: publicID("grp", uuid.New())}
	where, args, err := auditWhere(actor, filter, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(where, filter.Action) || len(args) != 6 {
		t.Fatalf("where=%s args=%v", where, args)
	}
	for _, arg := range args {
		if _, err := driver.DefaultParameterConverter.ConvertValue(arg); err != nil {
			t.Fatal(err)
		}
	}
}
