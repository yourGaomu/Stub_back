package links

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func csvTestRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "code", "title", "tags", "type", "asset", "size", "group", "group_name", "status", "availability", "visits", "max", "expires", "created"})
}

func csvFilterQuery(filter string) string {
	return `SELECT sl.link_id,sc.code,sl.title,sl.tags::text,sl.target_type,sl.asset_id,a.size_bytes, .* WHERE ` + regexp.QuoteMeta(filter) + ` ORDER BY sl.created_at DESC,sl.link_id DESC LIMIT 10001$`
}

func TestCSVAndListShareFilters(t *testing.T) {
	s, mock := newLinkTestService(t)
	actor := Actor{Role: "user", WorkspaceID: uuid.New(), BusinessID: "canvas"}
	group, asset := uuid.New(), uuid.New()
	query := map[string]string{"workspaceId": publicID("ws", uuid.New()), "status": "enabled", "targetType": "minio_asset", "code": "ABC-code",
		"maxVisits": "0", "keyword": "100%_literal", "assetId": publicID("ast", asset), "groupId": publicID("grp", group), "availability": "available", "createdFrom": "2026-01-01T00:00:00Z", "createdTo": "2026-12-31T00:00:00Z"}
	filter, args, err := buildListFilter(actor, query)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filter, "sl.workspace_id=$1 AND sl.business_id=$2") || !strings.Contains(filter, "sl.group_id=$9") || !strings.Contains(filter, "strpos(lower(sl.title)") || len(args) != 12 {
		t.Fatalf("incorrect scoped filter = %s %#v", filter, args)
	}
	if args[0] != actor.WorkspaceID || args[1] != "canvas" || args[4] != "abc-code" || args[5] != int64(0) || args[6] != query["keyword"] || args[7] != asset || args[8] != group {
		t.Fatalf("incorrect filter args = %#v", args)
	}
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg
	}
	mock.ExpectQuery(`SELECT count\(\*\) FROM short_links sl .* WHERE ` + regexp.QuoteMeta(filter)).WithArgs(values...).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	listValues := append(append([]driver.Value{}, values...), 20, 0)
	mock.ExpectQuery(`SELECT sl.link_id, sc.code, .* WHERE ` + regexp.QuoteMeta(filter) + ` ORDER BY sl.created_at DESC,sl.link_id DESC LIMIT \$13 OFFSET \$14`).WithArgs(listValues...).
		WillReturnRows(sqlmock.NewRows([]string{"link_id", "code", "title", "tags", "type", "url", "asset", "size", "status", "expires", "visits", "max", "created", "availability", "group", "group_name", "active_from", "active_until"}))
	if _, err := s.List(context.Background(), actor, 1, 20, query); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(csvFilterQuery(filter)).WithArgs(values...).WillReturnRows(csvTestRows())
	calls := 0
	count, err := s.ExportCSV(context.Background(), actor, query, func(row []string) error {
		calls++
		if len(row) != 16 || strings.Contains(strings.Join(row, ","), "targetUrl") {
			t.Fatalf("unexpected header = %#v", row)
		}
		return nil
	})
	if err != nil || count != 0 || calls != 1 {
		t.Fatalf("empty export = %d, %v, callbacks %d", count, err, calls)
	}
}

func TestListAndCSVGroupFilterValidation(t *testing.T) {
	for _, query := range []map[string]string{{"groupId": ""}, {"groupId": "bad"}, {"groupId": publicID("grp", uuid.New()), "ungrouped": "true"}, {"ungrouped": "1"}, {"ungrouped": ""}} {
		s, _ := newLinkTestService(t)
		_, err := s.List(context.Background(), Actor{}, 1, 20, query)
		var p *Problem
		if !errors.As(err, &p) || p.Status != 422 {
			t.Fatalf("invalid filter accepted by List: %v", err)
		}
		_, err = s.ExportCSV(context.Background(), Actor{}, query, func([]string) error { t.Fatal("invalid query called writer"); return nil })
		if !errors.As(err, &p) || p.Status != 422 {
			t.Fatalf("invalid filter accepted by ExportCSV: %v", err)
		}
	}
	actor := Actor{Role: "user", WorkspaceID: uuid.New(), BusinessID: "canvas"}
	filter, args, err := buildListFilter(actor, map[string]string{"ungrouped": "true"})
	if err != nil || filter != "sl.workspace_id=$1 AND sl.business_id=$2 AND sl.group_id IS NULL" || !reflect.DeepEqual(args, []any{actor.WorkspaceID, "canvas"}) {
		t.Fatalf("ungrouped filter = %s %#v, %v", filter, args, err)
	}
}

func TestCSVSafeFieldsAndUTCTimes(t *testing.T) {
	s, mock := newLinkTestService(t)
	s.cfg.PublicBaseURL = "https://stub.example.com"
	actor := Actor{Role: "admin", BusinessID: "canvas"}
	id, asset, group := uuid.New(), uuid.New(), uuid.New()
	created := time.Date(2026, 1, 2, 10, 30, 40, 0, time.FixedZone("offset", 8*60*60))
	expires := created.Add(time.Hour)
	mock.ExpectQuery(csvFilterQuery("sl.business_id=$1")).WithArgs("canvas").WillReturnRows(csvTestRows().
		AddRow(id, "public-code", "Title", `["one","two"]`, "minio_asset", asset.String(), 4096, group.String(), "Group", "enabled", "available", 7, -1, expires, created).
		AddRow(uuid.New(), nil, "Ungrouped", `[]`, "url", nil, nil, nil, nil, "disabled", "disabled", 0, 0, nil, created))
	var written [][]string
	count, err := s.ExportCSV(context.Background(), actor, nil, func(row []string) error { written = append(written, row); return nil })
	if err != nil || count != 2 || len(written) != 3 {
		t.Fatalf("export = %d, %v, rows %#v", count, err, written)
	}
	want := []string{publicID("link", id), "public-code", "https://stub.example.com/r/public-code", "Title", `["one","two"]`, "minio_asset", publicID("ast", asset), "4096", publicID("grp", group), "Group", "enabled", "available", "7", "-1", "2026-01-02T03:30:40Z", "2026-01-02T02:30:40Z"}
	if !reflect.DeepEqual(written[1], want) || written[2][1] != "" || written[2][2] != "" || written[2][8] != "" || written[2][9] != "" || written[2][14] != "" {
		t.Fatalf("safe CSV row = %#v; want %#v", written, want)
	}
}

func TestCSVRowLimitAndCallbackErrors(t *testing.T) {
	for _, total := range []int{10000, 10001} {
		t.Run(map[int]string{10000: "exact_limit", 10001: "over_limit"}[total], func(t *testing.T) {
			s, mock := newLinkTestService(t)
			rows := csvTestRows()
			id, now := uuid.New(), time.Now()
			for i := 0; i < total; i++ {
				rows.AddRow(id, "code", "title", `[]`, "url", nil, nil, nil, nil, "enabled", "available", 0, -1, nil, now)
			}
			mock.ExpectQuery(csvFilterQuery("TRUE")).WillReturnRows(rows)
			calls := 0
			count, err := s.ExportCSV(context.Background(), Actor{Role: "admin"}, nil, func([]string) error { calls++; return nil })
			if total == 10001 {
				assertLinkProblem(t, err, 422, "export_limit_exceeded")
				if count != 0 || calls != 10001 {
					t.Fatalf("overflow count/callbacks = %d/%d", count, calls)
				}
			} else if err != nil || count != 10000 || calls != 10001 {
				t.Fatalf("limit export = %d, %v, callbacks %d", count, err, calls)
			}
		})
	}
	for _, stage := range []string{"header", "row", "database"} {
		t.Run(stage, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			rows := csvTestRows().AddRow(uuid.New(), "code", "title", `[]`, "url", nil, nil, nil, nil, "enabled", "available", 0, -1, nil, time.Now())
			failure := errors.New("writer failed")
			if stage == "database" {
				rows.RowError(0, failure)
			}
			mock.ExpectQuery(csvFilterQuery("TRUE")).WillReturnRows(rows).RowsWillBeClosed()
			calls := 0
			count, err := s.ExportCSV(context.Background(), Actor{Role: "admin"}, nil, func([]string) error {
				calls++
				if stage == "header" || stage == "row" && calls == 2 {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) || count != 0 {
				t.Fatalf("failed export = %d, %v", count, err)
			}
		})
	}
}
