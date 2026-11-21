package links

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
)

func TestLinkWindowBoundaries(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	r := linkRecord{
		Status: "enabled", MaxVisits: 1, IPCIDRs: "{}",
		ActiveFrom:  sql.NullTime{Time: start.In(time.FixedZone("UTC+08", 8*3600)), Valid: true},
		ActiveUntil: sql.NullTime{Time: end.In(time.FixedZone("UTC-07", -7*3600)), Valid: true},
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		want string
	}{
		{"just_before_start", start.Add(-time.Nanosecond), "not_started"},
		{"at_start", start, "available"},
		{"just_before_end", end.Add(-time.Nanosecond), "available"},
		{"at_end", end, "window_ended"},
		{"after_end", end.Add(time.Nanosecond), "window_ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := availabilityAt(r, tc.now); got != tc.want {
				t.Errorf("availability at %s = %q, want %q", tc.now, got, tc.want)
			}
			reason := tc.want
			if reason == "available" {
				reason = ""
			}
			if got := precheckAccessReason(r, "active", "192.0.2.10", tc.now); got != reason {
				t.Errorf("access denial at %s = %q, want %q", tc.now, got, reason)
			}
		})
	}
}

func TestLinkWindowAvailabilityPriority(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	past := sql.NullTime{Time: now.Add(-time.Hour), Valid: true}
	future := sql.NullTime{Time: now.Add(time.Hour), Valid: true}
	atDeadline := sql.NullTime{Time: now, Valid: true}
	for _, tc := range []struct {
		name, status           string
		expiresAt, from, until sql.NullTime
		want                   string
	}{
		{"disabled_before_expiry", "disabled", atDeadline, past, atDeadline, "disabled"},
		{"expiry_before_window_end", "enabled", atDeadline, past, atDeadline, "expired"},
		{"window_end_before_limit", "enabled", future, past, atDeadline, "window_ended"},
		{"not_started_before_limit", "enabled", sql.NullTime{}, future, sql.NullTime{}, "not_started"},
		{"limit_inside_window", "enabled", future, past, future, "visit_limit_reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := linkRecord{Status: tc.status, ExpiresAt: tc.expiresAt, ActiveFrom: tc.from, ActiveUntil: tc.until, MaxVisits: 1, VisitsUsed: 1, IPCIDRs: "{}"}
			if got := availabilityAt(r, now); got != tc.want {
				t.Errorf("availability = %q, want %q", got, tc.want)
			}
			if got := precheckAccessReason(r, "active", "192.0.2.10", now); got != tc.want {
				t.Errorf("access denial = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLinkWindowDeniesPasswordAccess(t *testing.T) {
	hash, err := auth.HashPassword("correct")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, window := range []struct {
		reason     string
		start, end time.Time
		status     int
	}{
		{"not_started", now.Add(24 * time.Hour), now.Add(48 * time.Hour), http.StatusForbidden},
		{"window_ended", now.Add(-48 * time.Hour), now.Add(-24 * time.Hour), http.StatusGone},
	} {
		for _, request := range []struct {
			name, password string
			submit         bool
		}{
			{"GET", "", false},
			{"POST_correct", "correct", true},
			{"POST_wrong", "wrong", true},
		} {
			t.Run(window.reason+"/"+request.name, func(t *testing.T) {
				s, mock := newLinkTestService(t)
				r := testLink()
				r.ActiveFrom = sql.NullTime{Time: window.start, Valid: true}
				r.ActiveUntil = sql.NullTime{Time: window.end, Valid: true}
				r.RequirePassword, r.PasswordHash = true, sql.NullString{String: hash, Valid: true}
				expectAccessLock(mock, r, "active")
				// The denial log is the only expected write.
				expectAccessLog(mock, r, "denied", window.reason, false, "link_access").
					WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(1))
				mock.ExpectCommit()
				got, err := s.Access(context.Background(), r.Code.String, testAccessMeta(), request.password, request.submit)
				if err != nil || got.StatusCode != window.status || got.Location != "" || got.Challenge {
					t.Fatalf("Access = %+v, %v, want status %d without redirect or challenge", got, err, window.status)
				}
			})
		}
	}
}

func TestUpdateRulesWindowPatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	oldFrom, oldUntil, oldExpiry := now.Add(-72*time.Hour), now.Add(-48*time.Hour), now.Add(-24*time.Hour)
	newFrom, newUntil, newExpiry := now.Add(24*time.Hour), now.Add(48*time.Hour), now.Add(72*time.Hour)
	for _, tc := range []struct {
		name, payload       string
		from, until, expiry *time.Time
		maxVisits           int64
		availability        string
	}{
		{"omit_historical_times_and_change_limit", `{"maxVisits":7}`, &oldFrom, &oldUntil, &oldExpiry, 7, "expired"},
		{"clear_start_only", `{"activeFrom":null}`, nil, &oldUntil, &oldExpiry, 5, "expired"},
		{"replace_expiry_and_keep_ended_window", fmt.Sprintf(`{"expiresAt":%q}`, offsetWindowTimestamp(newExpiry)), &oldFrom, &oldUntil, &newExpiry, 5, "window_ended"},
		{"replace_start_and_clear_deadlines", fmt.Sprintf(`{"activeFrom":%q,"activeUntil":null,"expiresAt":null}`, offsetWindowTimestamp(newFrom)), &newFrom, nil, nil, 5, "not_started"},
		{"replace_window_and_expiry_together", fmt.Sprintf(`{"activeFrom":%q,"activeUntil":%q,"expiresAt":%q}`, offsetWindowTimestamp(newFrom), offsetWindowTimestamp(newUntil), offsetWindowTimestamp(newExpiry)), &newFrom, &newUntil, &newExpiry, 5, "not_started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.ActiveFrom = sql.NullTime{Time: oldFrom, Valid: true}
			r.ActiveUntil = sql.NullTime{Time: oldUntil, Valid: true}
			r.ExpiresAt = sql.NullTime{Time: oldExpiry, Valid: true}
			r.MaxVisits, r.VisitsUsed = 5, 3
			actor := Actor{UserID: uuid.New(), WorkspaceID: r.WorkspaceID, Role: "user"}
			var input UpdateRulesRequest
			if err := json.Unmarshal([]byte(tc.payload), &input); err != nil {
				t.Fatal(err)
			}

			mock.ExpectBegin()
			expectLoadLink(mock, r)
			const update = `UPDATE short_links SET expires_at=$2,max_visits=$3,allowed_ip_cidrs=$4::cidr[],require_password=$5,password_hash=$6,wrong_password_policy=$7::jsonb,updated_at=$8,active_from=$9,active_until=$10,visits_cycle=CASE WHEN max_visits<>$3 THEN visits_cycle+1 ELSE visits_cycle END WHERE link_id=$1`
			mock.ExpectExec("^"+regexp.QuoteMeta(update)+"$").
				WithArgs(r.ID, tc.expiry, tc.maxVisits, "{}", false, nil, nil, sqlmock.AnyArg(), tc.from, tc.until).
				WillReturnResult(sqlmock.NewResult(0, 1))
			expectWindowAudit(mock, actor, r, "link.rules.update", auditFields{
				"expiresAtBefore": auditWindowTime(&oldExpiry), "expiresAtAfter": auditWindowTime(tc.expiry),
				"activeFromBefore": auditWindowTime(&oldFrom), "activeFromAfter": auditWindowTime(tc.from),
				"activeUntilBefore": auditWindowTime(&oldUntil), "activeUntilAfter": auditWindowTime(tc.until),
				"maxVisitsBefore": float64(r.MaxVisits), "maxVisitsAfter": float64(tc.maxVisits),
			})
			mock.ExpectCommit()

			got, err := s.UpdateRules(context.Background(), actor, publicID("link", r.ID), input, RequestMeta{})
			if err != nil {
				t.Fatal(err)
			}
			assertWindowUTC(t, "activeFrom", got.ActiveFrom, tc.from)
			assertWindowUTC(t, "activeUntil", got.ActiveUntil, tc.until)
			assertWindowUTC(t, "expiresAt", got.ExpiresAt, tc.expiry)
			if got.Rules.MaxVisits != tc.maxVisits || got.VisitsUsed != r.VisitsUsed || got.Availability != tc.availability {
				t.Errorf("UpdateRules = %+v, want maxVisits=%d visitsUsed=%d availability=%q", got, tc.maxVisits, r.VisitsUsed, tc.availability)
			}
		})
	}
}

func TestCreateRejectsWindowCollapsedAtStoragePrecision(t *testing.T) {
	s, _ := newLinkTestService(t)
	start := time.Now().UTC().Truncate(time.Second).Add(time.Hour + 123*time.Nanosecond)
	end := start.Add(500 * time.Nanosecond)
	_, err := s.Create(context.Background(), Actor{}, CreateRequest{
		Target:     TargetInput{Type: "url", URL: "https://example.test/window"},
		ActiveFrom: &start, ActiveUntil: &end,
	}, RequestMeta{})
	assertLinkProblem(t, err, 422, "invalid_window")
	if got := nullTime(&start); !got.Valid || got.Time.Nanosecond() != 0 {
		t.Fatalf("window value at storage precision = %v", got)
	}
}

func TestUpdateRulesWindowValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start, expiry, end := now.Add(24*time.Hour), now.Add(48*time.Hour), now.Add(72*time.Hour)
	for _, tc := range []struct{ name, payload, code string }{
		{"start_at_retained_expiry", fmt.Sprintf(`{"activeFrom":%q}`, offsetWindowTimestamp(expiry)), "invalid_window"},
		{"end_at_retained_start", fmt.Sprintf(`{"activeUntil":%q}`, offsetWindowTimestamp(start)), "invalid_window"},
		{"expiry_before_retained_start", fmt.Sprintf(`{"expiresAt":%q}`, offsetWindowTimestamp(now.Add(time.Hour))), "invalid_window"},
		{"explicit_past_end", fmt.Sprintf(`{"activeFrom":null,"activeUntil":%q}`, offsetWindowTimestamp(now.Add(-time.Hour))), "invalid_window"},
		{"explicit_past_expiry", fmt.Sprintf(`{"activeFrom":null,"expiresAt":%q}`, offsetWindowTimestamp(now.Add(-time.Hour))), "invalid_expiration"},
		{"malformed_start", `{"activeFrom":"2026-08-01T00:00:00"}`, "invalid_window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.ActiveFrom = sql.NullTime{Time: start, Valid: true}
			r.ActiveUntil = sql.NullTime{Time: end, Valid: true}
			r.ExpiresAt = sql.NullTime{Time: expiry, Valid: true}
			var input UpdateRulesRequest
			if err := json.Unmarshal([]byte(tc.payload), &input); err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			expectLoadLink(mock, r)
			mock.ExpectRollback()
			_, err := s.UpdateRules(context.Background(), Actor{Role: "admin"}, publicID("link", r.ID), input, RequestMeta{})
			assertLinkProblem(t, err, 422, tc.code)
		})
	}
}

func TestRenewWindowConsistency(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(24*time.Hour), now.Add(72*time.Hour)
	for _, tc := range []struct {
		name    string
		expiry  time.Time
		invalid bool
	}{
		{"future_expiry_before_start", start.Add(-time.Hour), true},
		{"expiry_at_start_with_offset", start, true},
		{"future_expiry_inside_window", start.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.ActiveFrom = sql.NullTime{Time: start, Valid: true}
			r.ActiveUntil = sql.NullTime{Time: end, Valid: true}
			r.MaxVisits, r.VisitsUsed = 5, 3
			actor := Actor{UserID: uuid.New(), WorkspaceID: r.WorkspaceID, Role: "user"}
			expiresAt, err := time.Parse(time.RFC3339, offsetWindowTimestamp(tc.expiry))
			if err != nil {
				t.Fatal(err)
			}
			input := RenewRequest{ExpiresAt: expiresAt}
			mock.ExpectBegin()
			expectLoadLink(mock, r)
			if tc.invalid {
				mock.ExpectRollback()
				_, err := s.Renew(context.Background(), actor, publicID("link", r.ID), input, RequestMeta{})
				assertLinkProblem(t, err, 422, "invalid_window")
				return
			}

			mock.ExpectExec(`^UPDATE short_links SET expires_at=\$2,updated_at=now\(\) WHERE link_id=\$1$`).
				WithArgs(r.ID, sameWindowInstant(tc.expiry)).WillReturnResult(sqlmock.NewResult(0, 1))
			expectWindowAudit(mock, actor, r, "link.renew", auditFields{"expiresAt": auditWindowTime(&tc.expiry), "codeStrategy": "keep", "codeChanged": false})
			mock.ExpectCommit()
			got, err := s.Renew(context.Background(), actor, publicID("link", r.ID), input, RequestMeta{})
			if err != nil {
				t.Fatal(err)
			}
			if expires, ok := got["expiresAt"].(time.Time); ok {
				assertWindowUTC(t, "expiresAt", &expires, &tc.expiry)
			} else {
				t.Errorf("Renew expiresAt = %#v, want time.Time", got["expiresAt"])
			}
			if got["code"] != r.Code.String || got["codeChanged"] != false || got["visitsUsed"] != r.VisitsUsed || got["maxVisits"] != r.MaxVisits {
				t.Errorf("Renew = %+v, want existing code and visit counts", got)
			}
		})
	}
}

func TestResetVisitsAdvancesCycle(t *testing.T) {
	s, mock := newLinkTestService(t)
	r := testLink()
	r.MaxVisits, r.VisitsUsed = 5, 3
	r.ActiveFrom = sql.NullTime{Time: time.Now().UTC().Add(24 * time.Hour), Valid: true}
	actor := Actor{UserID: uuid.New(), WorkspaceID: r.WorkspaceID, Role: "user"}
	mock.ExpectBegin()
	expectLoadLink(mock, r)
	mock.ExpectExec(`^UPDATE short_links SET visits_used=0,visits_cycle=visits_cycle\+1,updated_at=\$2 WHERE link_id=\$1$`).
		WithArgs(r.ID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	expectWindowAudit(mock, actor, r, "link.reset_visits", auditFields{"previousVisitsUsed": float64(3), "visitsUsed": float64(0), "maxVisits": float64(5)})
	mock.ExpectCommit()
	got, err := s.ResetVisits(context.Background(), actor, publicID("link", r.ID), RequestMeta{})
	if err != nil || got["visitsUsed"] != 0 || got["maxVisits"] != r.MaxVisits {
		t.Fatalf("ResetVisits = %+v, %v, want zero visits and original limit", got, err)
	}
}

func TestListAndExportWindowAvailabilityFilters(t *testing.T) {
	// Expect the same boundary operators and state priority in the filter and projection.
	const stateSQL = "CASE WHEN sl.status='disabled' THEN 'disabled' " +
		"WHEN sl.expires_at IS NOT NULL AND sl.expires_at<=now() THEN 'expired' " +
		"WHEN sl.active_until IS NOT NULL AND sl.active_until<=now() THEN 'window_ended' " +
		"WHEN sl.active_from IS NOT NULL AND sl.active_from>now() THEN 'not_started' " +
		"WHEN sl.max_visits>=0 AND sl.visits_used>=sl.max_visits THEN 'visit_limit_reached' ELSE 'available' END"
	now := time.Now().In(time.FixedZone("UTC+08", 8*3600))
	for _, state := range []string{"not_started", "window_ended"} {
		t.Run(state, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			actor := Actor{Role: "user", WorkspaceID: r.WorkspaceID, BusinessID: "canvas"}
			query := map[string]string{"availability": state}
			filter := regexp.QuoteMeta(" WHERE sl.workspace_id=$1 AND sl.business_id=$2 AND " + stateSQL + "=$3")
			order := regexp.QuoteMeta(" ORDER BY sl.created_at DESC,sl.link_id DESC")
			start := now.Add(24 * time.Hour)
			if state == "window_ended" {
				start = now.Add(-24 * time.Hour)
			}
			end := start.Add(time.Hour)
			mock.ExpectQuery(`^SELECT count\(\*\) FROM short_links sl .*`+filter+`$`).
				WithArgs(r.WorkspaceID, "canvas", state).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			listProjection := regexp.QuoteMeta(stateSQL + ", sl.group_id,g.name,sl.active_from,sl.active_until")
			mock.ExpectQuery(`^SELECT sl\.link_id, sc\.code, .* `+listProjection+` FROM .*`+filter+order+` LIMIT \$4 OFFSET \$5$`).
				WithArgs(r.WorkspaceID, "canvas", state, 10, 0).
				WillReturnRows(sqlmock.NewRows([]string{"link_id", "code", "title", "tags", "type", "url", "asset", "size", "status", "expires", "visits", "max", "created", "availability", "group_id", "group_name", "active_from", "active_until"}).
					AddRow(r.ID, r.Code, "Scheduled", `[]`, r.TargetType, r.TargetURL, nil, nil, r.Status, nil, 0, 1, r.CreatedAt, state, nil, nil, start, end))
			exportProjection := regexp.QuoteMeta(",sl.status," + stateSQL + ",sl.visits_used")
			mock.ExpectQuery(`^SELECT sl\.link_id,sc\.code,.*`+exportProjection+`.*`+filter+order+` LIMIT 10001$`).
				WithArgs(r.WorkspaceID, "canvas", state).
				WillReturnRows(csvTestRows().AddRow(r.ID, r.Code, "Scheduled", `[]`, r.TargetType, nil, nil, nil, nil, r.Status, state, 0, 1, nil, r.CreatedAt))

			page, err := s.List(context.Background(), actor, 1, 10, query)
			if err != nil || page.Pagination.Total != 1 || len(page.Items) != 1 || page.Items[0].Availability != state {
				t.Fatalf("List = %+v, %v, want one %s item", page, err, state)
			}
			assertWindowUTC(t, "activeFrom", page.Items[0].ActiveFrom, &start)
			assertWindowUTC(t, "activeUntil", page.Items[0].ActiveUntil, &end)

			var rows [][]string
			count, err := s.ExportCSV(context.Background(), actor, query, func(row []string) error {
				rows = append(rows, row)
				return nil
			})
			const availabilityColumn = 11
			if err != nil || count != 1 || len(rows) != 2 || len(rows[0]) <= availabilityColumn || len(rows[1]) <= availabilityColumn || rows[0][availabilityColumn] != "availability" || rows[1][availabilityColumn] != state {
				t.Fatalf("ExportCSV = %d, %v, rows %v, want one %s row", count, err, rows, state)
			}
		})
	}
	t.Run("invalid_state", func(t *testing.T) {
		s, _ := newLinkTestService(t)
		actor := Actor{Role: "admin"}
		query := map[string]string{"availability": "outside_window"}
		_, err := s.List(context.Background(), actor, 1, 10, query)
		assertLinkProblem(t, err, 422, "invalid_filter")
		_, err = s.ExportCSV(context.Background(), actor, query, func([]string) error {
			t.Fatal("invalid filter called CSV writer")
			return nil
		})
		assertLinkProblem(t, err, 422, "invalid_filter")
	})
}

func offsetWindowTimestamp(value time.Time) string {
	return value.In(time.FixedZone("UTC+05:30", 5*3600+30*60)).Format(time.RFC3339)
}

func auditWindowTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func expectWindowAudit(mock sqlmock.Sqlmock, actor Actor, r linkRecord, action string, fields auditFields) {
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(actor.UserID, r.WorkspaceID, action, "short_link", r.ID.String(), "", "", fields, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func assertWindowUTC(t *testing.T, field string, got, want *time.Time) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s = %v, want nil", field, got)
		}
		return
	}
	if got == nil || got.Location() != time.UTC || !got.Equal(*want) {
		t.Errorf("%s = %v, want %v", field, got, want.UTC())
	}
}

type sameWindowInstant time.Time

func (want sameWindowInstant) Match(value driver.Value) bool {
	got, ok := value.(time.Time)
	return ok && got.Equal(time.Time(want))
}
