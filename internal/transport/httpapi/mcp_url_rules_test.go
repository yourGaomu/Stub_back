package httpapi

import (
	"database/sql/driver"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type mcpExpiresNear struct{ days int64 }

func (a mcpExpiresNear) Match(value driver.Value) bool {
	expires, ok := value.(time.Time)
	if !ok {
		return false
	}
	difference := time.Until(expires) - time.Duration(a.days)*24*time.Hour
	return difference > -5*time.Second && difference < 5*time.Second
}

func expectMCPURLCreation(f *mcpFixture, visits int64, expires driver.Value, title string) {
	f.mock.ExpectBegin()
	f.mock.ExpectQuery(`SELECT role, url_link_quota, minio_asset_link_quota FROM users WHERE user_id=\$1 FOR (NO KEY )?UPDATE`).WithArgs(f.ctx.actor.UserID).
		WillReturnRows(sqlmock.NewRows([]string{"role", "url_quota", "asset_quota"}).AddRow("admin", 100, 100))
	args := []driver.Value{
		sqlmock.AnyArg(), f.ctx.actor.WorkspaceID, "", title, "[]", "url", "https://example.test/page", nil,
		expires, visits, "{}", false, nil, nil, nil, nil, f.ctx.actor.UserID,
	}
	f.mock.ExpectExec(`INSERT INTO short_links .*`).WithArgs(mcpLinkInsertArgs(args)...).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectExec(`INSERT INTO short_link_codes`).WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectCommit()
}

func TestMCPURLCreationPreservesExactVisitAndExpiryRules(t *testing.T) {
	for _, visits := range []int64{-1, 0, 7, 9007199254740993, math.MaxInt64} {
		t.Run(stringInt64(visits), func(t *testing.T) {
			f := newMCPFixture(t)
			expectMCPURLCreation(f, visits, mcpExpiresNear{days: 3}, "URL share")
			result := mcpCall(t, f, "create_url_shortlink", map[string]any{
				"url": "https://example.test/page", "title": "URL share", "maxVisits": visits, "expireDays": 3,
			})
			if result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "https://short.example.test/r/") {
				t.Fatalf("URL creation failed: %+v", result)
			}
			if visits == 0 && !strings.Contains(result.Content[0].Text, "禁止访问") {
				t.Fatal("disabled link was described as unlimited")
			}
		})
	}
	t.Run("omitted_limit_and_zero_days", func(t *testing.T) {
		f := newMCPFixture(t)
		expectMCPURLCreation(f, -1, nil, "Permanent")
		result := mcpCall(t, f, "create_url_shortlink", map[string]any{"url": "https://example.test/page", "title": "Permanent", "expireDays": 0})
		if result.IsError {
			t.Fatalf("omitted limit did not use unlimited rules: %+v", result)
		}
	})
}

func TestMCPBurnLinkUsesInlineSingleVisitRule(t *testing.T) {
	f := newMCPFixture(t)
	expectMCPURLCreation(f, 1, mcpExpiresNear{days: 1}, "阅后即焚安全分享")
	result := mcpCall(t, f, "create_burn_link", map[string]any{"url": "https://example.test/page"})
	if result.IsError || !strings.Contains(result.Content[0].Text, "https://short.example.test/r/") {
		t.Fatalf("burn link did not reach valid inline rule creation: %+v", result)
	}
}

func TestMCPURLRulesRejectInvalidNumbersBeforeCreation(t *testing.T) {
	for _, input := range []map[string]any{
		{"maxVisits": -2}, {"maxVisits": 1.5}, {"maxVisits": "5"}, {"maxVisits": nil},
		{"maxVisits": json.Number("9223372036854775808")},
		{"expireDays": -1}, {"expireDays": 0.5}, {"expireDays": "2"}, {"expireDays": nil},
		{"expireDays": json.Number("9223372036854775807")},
	} {
		encoded, _ := json.Marshal(input)
		t.Run(string(encoded), func(t *testing.T) {
			f := newMCPFixture(t)
			input["url"] = "https://example.test/page"
			if result := mcpCall(t, f, "create_url_shortlink", input); !result.IsError {
				t.Fatal("invalid numeric rule reached creation")
			}
		})
	}
}
