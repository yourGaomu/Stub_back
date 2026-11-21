package links

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListMaxVisitsAndStableOrder(t *testing.T) {
	for _, value := range []int64{-1, 0, 1, 9223372036854775807} {
		t.Run(strconv.FormatInt(value, 10), func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			mock.ExpectQuery(`SELECT count\(\*\) FROM short_links sl .* WHERE sl.workspace_id=\$1 AND sl.business_id=\$2 AND sl.max_visits=\$3`).
				WithArgs(r.WorkspaceID, "canvas", value).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			mock.ExpectQuery(`SELECT sl.link_id, sc.code, .* WHERE sl.workspace_id=\$1 AND sl.business_id=\$2 AND sl.max_visits=\$3 ORDER BY sl.created_at DESC,sl.link_id DESC LIMIT \$4 OFFSET \$5`).
				WithArgs(r.WorkspaceID, "canvas", value, 10, 10).WillReturnRows(sqlmock.NewRows([]string{"link_id", "code", "title", "tags", "type", "url", "asset", "size", "status", "expires", "visits", "max", "created", "availability", "group_id", "group_name", "active_from", "active_until"}))
			_, err := s.List(context.Background(), Actor{Role: "user", WorkspaceID: r.WorkspaceID, BusinessID: "canvas"}, 2, 10, map[string]string{"maxVisits": strconv.FormatInt(value, 10)})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, raw := range []string{"", "-2", "1.1", "9223372036854775808", "text", " 1 "} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			s, _ := newLinkTestService(t)
			_, err := s.List(context.Background(), Actor{Role: "admin"}, 1, 20, map[string]string{"maxVisits": raw})
			var p *Problem
			if !errors.As(err, &p) || p.Status != 422 {
				t.Fatalf("invalid maxVisits %q = %v", raw, err)
			}
		})
	}
}

func TestAccessPasswordBoundariesInCreateAndUpdateRules(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"empty", "", false}, {"whitespace", " \t\n", false},
		{"one_byte", "a", true}, {"max_bytes", strings.Repeat("a", 1024), true},
		{"over_max", strings.Repeat("a", 1025), false},
		{"unicode_max", strings.Repeat("密", 341) + "a", true},
		{"unicode_over_max", strings.Repeat("密", 342), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if validAccessPassword(tc.password) != tc.valid {
				t.Fatalf("password length %d boundary mismatch", len(tc.password))
			}
			t.Run("create", func(t *testing.T) {
				s, mock := newLinkTestService(t)
				stop := errors.New("stop before business writes")
				if tc.valid {
					mock.ExpectBegin().WillReturnError(stop)
				}
				_, err := s.Create(context.Background(), Actor{}, CreateRequest{
					Target: TargetInput{Type: "url", URL: "https://example.com"}, AccessPassword: tc.password,
					RuleSource: RuleSourceInput{Type: "inline", Rules: &RulesInput{RequirePassword: true}},
				}, RequestMeta{})
				assertPasswordBoundaryError(t, err, tc.valid, stop)
			})
			t.Run("update", func(t *testing.T) {
				s, mock := newLinkTestService(t)
				r := testLink()
				mock.ExpectBegin()
				expectLoadLink(mock, r)
				stop := errors.New("stop before business writes")
				if tc.valid {
					mock.ExpectExec(`UPDATE short_links SET expires_at=`).WillReturnError(stop)
				}
				mock.ExpectRollback()
				requirePassword := true
				_, err := s.UpdateRules(context.Background(), Actor{Role: "admin"}, publicID("link", r.ID), UpdateRulesRequest{RequirePassword: &requirePassword, AccessPassword: &tc.password}, RequestMeta{})
				assertPasswordBoundaryError(t, err, tc.valid, stop)
			})
		})
	}
}

func assertPasswordBoundaryError(t *testing.T, err error, valid bool, stop error) {
	t.Helper()
	if valid {
		if !errors.Is(err, stop) {
			t.Fatalf("valid password did not reach business writes: %v", err)
		}
		return
	}
	var p *Problem
	if !errors.As(err, &p) || p.Status != 422 || (p.Code != "invalid_access_password" && p.Code != "access_password_required") {
		t.Fatalf("invalid password error = %v, want 422", err)
	}
}
