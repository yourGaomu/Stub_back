package links

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/yourGaomu/short_links/back/internal/config"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/platform/objectstore"
)

type presignedGetFunc func(context.Context, string, string, time.Duration, string, string) (string, error)

func (f presignedGetFunc) PresignedGet(ctx context.Context, bucket, key string, ttl time.Duration, contentType, fileName string) (string, error) {
	return f(ctx, bucket, key, ttl, contentType, fileName)
}

func newLinkTestService(t *testing.T) (*Service, sqlmock.Sqlmock) {
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
	return NewService(db, config.Config{}, nil), mock
}

func testLink() linkRecord {
	return linkRecord{
		ID: uuid.New(), WorkspaceID: uuid.New(), TargetType: "url",
		TargetURL: sql.NullString{String: "https://example.com/target", Valid: true},
		MaxVisits: 1, IPCIDRs: "{}", Status: "enabled",
		Code:       sql.NullString{String: "test-code", Valid: true},
		CodeStatus: sql.NullString{String: "active", Valid: true},
		CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func expectLoadLink(mock sqlmock.Sqlmock, r linkRecord) {
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, _ := json.Marshal(tags)
	rows := sqlmock.NewRows([]string{
		"link_id", "workspace_id", "business_id", "target_type", "target_url", "asset_id", "size_bytes",
		"bucket", "object_key", "original_name", "content_type", "expires_at", "max_visits", "visits_used",
		"allowed_ip_cidrs", "require_password", "password_hash", "wrong_password_policy", "status",
		"template_id", "template_version", "code", "code_status", "created_at", "updated_at", "title", "tags", "group_id", "group_name", "active_from", "active_until",
	}).AddRow(r.ID, r.WorkspaceID, r.BusinessID, r.TargetType, r.TargetURL, r.AssetID, r.AssetSize,
		r.Bucket, r.ObjectKey, r.OriginalName, r.ContentType, r.ExpiresAt, r.MaxVisits, r.VisitsUsed,
		r.IPCIDRs, r.RequirePassword, r.PasswordHash, r.PolicyJSON, r.Status, r.TemplateID,
		r.TemplateVersion, r.Code, r.CodeStatus, r.CreatedAt, r.UpdatedAt, r.Title, string(tagsJSON), r.GroupID, r.GroupName, r.ActiveFrom, r.ActiveUntil)
	mock.ExpectQuery(`SELECT sl\.link_id,sl\.workspace_id,.* WHERE sl\.link_id=\$1 FOR UPDATE OF sl`).
		WithArgs(r.ID).WillReturnRows(rows)
}

func expectAccessLock(mock sqlmock.Sqlmock, r linkRecord, codeStatus string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT link_id FROM short_link_codes WHERE code=\$1`).WithArgs("test-code").
		WillReturnRows(sqlmock.NewRows([]string{"link_id"}).AddRow(r.ID))
	expectLoadLink(mock, r)
	query := mock.ExpectQuery(`SELECT status FROM short_link_codes WHERE code=\$1 AND link_id=\$2`).WithArgs("test-code", r.ID)
	rows := sqlmock.NewRows([]string{"status"})
	if codeStatus != "" {
		rows.AddRow(codeStatus)
	}
	query.WillReturnRows(rows)
}

func expectAccessLog(mock sqlmock.Sqlmock, r linkRecord, result, reason string, consumes bool, eventType string) *sqlmock.ExpectedQuery {
	var business any
	if r.AssetID.Valid {
		business = "canvas"
		mock.ExpectQuery(`SELECT business_id FROM assets WHERE asset_id=\$1`).WithArgs(r.AssetID.String).
			WillReturnRows(sqlmock.NewRows([]string{"business_id"}).AddRow(business))
	}
	return mock.ExpectQuery(`INSERT INTO short_link_access_logs\(.* RETURNING event_id`).
		WithArgs(r.ID, r.WorkspaceID, business, eventType, "192.0.2.10", "test-agent", result, reason, consumes, "test-request")
}

func testAccessMeta() RequestMeta {
	return RequestMeta{SourceIP: "192.0.2.10", UserAgent: "test-agent", RequestID: "test-request"}
}

func TestAccessDenialsDoNotConsumeVisits(t *testing.T) {
	expired := sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}
	cases := []struct {
		name, codeStatus, reason string
		status                   int
		change                   func(*linkRecord)
	}{
		{"revoked_precedes_all", "revoked", "code_revoked", 410, func(r *linkRecord) {
			r.Status = "disabled"
			r.ExpiresAt = expired
			r.MaxVisits = 0
			r.IPCIDRs = "{10.0.0.0/8}"
		}},
		{"missing_code_after_lock", "", "code_revoked", 410, nil},
		{"disabled_precedes_expired", "active", "disabled", 410, func(r *linkRecord) { r.Status = "disabled"; r.ExpiresAt = expired; r.MaxVisits = 0 }},
		{"expired_precedes_limit", "active", "expired", 410, func(r *linkRecord) { r.ExpiresAt = expired; r.MaxVisits = 0 }},
		{"zero_limit_precedes_ip", "active", "visit_limit_reached", 403, func(r *linkRecord) { r.MaxVisits = 0; r.IPCIDRs = "{10.0.0.0/8}" }},
		{"limit_reached", "active", "visit_limit_reached", 403, func(r *linkRecord) { r.VisitsUsed = 1 }},
		{"ip_denied", "active", "ip_denied", 403, func(r *linkRecord) { r.IPCIDRs = "{10.0.0.0/8}" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			if tc.change != nil {
				tc.change(&r)
			}
			expectAccessLock(mock, r, tc.codeStatus)
			expectAccessLog(mock, r, "denied", tc.reason, false, "link_access").
				WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(1))
			mock.ExpectCommit()
			result, err := s.Access(context.Background(), " TEST-CODE ", testAccessMeta(), "", false)
			if err != nil || result.StatusCode != tc.status || result.Location != "" {
				t.Fatalf("Access = %+v, %v; want status %d without Location", result, err, tc.status)
			}
		})
	}
}

func TestAccessPreparesLocationBeforeConsuming(t *testing.T) {
	cases := []struct {
		name, location                                   string
		asset, available, noStore, signError, missingKey bool
		status                                           int
	}{
		{name: "url_last_visit", location: "https://example.com/target", status: 302},
		{name: "url_unlimited", location: "https://example.com/target", status: 302},
		{name: "url_userinfo_compatibility", location: "https://user:pass@example.com/target", status: 302},
		{name: "asset", asset: true, available: true, location: "https://files.example.com/file?signature=test", status: 302},
		{name: "asset_deleted_or_purged", asset: true, status: 503},
		{name: "asset_store_nil", asset: true, available: true, noStore: true, status: 503},
		{name: "asset_signing_failure", asset: true, available: true, signError: true, status: 503},
		{name: "asset_empty_location", asset: true, available: true, status: 503},
		{name: "asset_invalid_location", asset: true, available: true, location: "javascript:alert(1)", status: 503},
		{name: "asset_missing_key", asset: true, available: true, missingKey: true, status: 503},
		{name: "url_empty_location", status: 503},
		{name: "url_invalid_location", location: "https://example.com/\r\nInjected: true", status: 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.TargetURL.String = tc.location
			if tc.name == "url_unlimited" {
				r.MaxVisits, r.VisitsUsed = -1, 99
			}
			calls := 0
			if tc.asset {
				r.TargetType, r.TargetURL = "minio_asset", sql.NullString{}
				r.AssetID = sql.NullString{String: uuid.NewString(), Valid: true}
				r.Bucket = sql.NullString{String: "canvas-bucket", Valid: true}
				r.ObjectKey = sql.NullString{String: "file.png", Valid: !tc.missingKey}
				r.ContentType = sql.NullString{String: "image/png", Valid: true}
				r.OriginalName = sql.NullString{String: "example.png", Valid: true}
				if !tc.noStore {
					s.store = presignedGetFunc(func(_ context.Context, bucket, key string, ttl time.Duration, contentType, fileName string) (string, error) {
						calls++
						if bucket != r.Bucket.String || key != r.ObjectKey.String || ttl != 15*time.Minute || contentType != "image/png" || fileName != "example.png" {
							t.Error("incorrect signing arguments")
						}
						if tc.signError {
							return tc.location, errors.New("signing unavailable")
						}
						return tc.location, nil
					})
				}
			}
			expectAccessLock(mock, r, "active")
			if tc.asset {
				mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM assets WHERE asset_id=\$1 AND deleted_at IS NULL AND purged_at IS NULL\)`).
					WithArgs(r.AssetID.String).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.available))
			}
			result, reason := "denied", "asset_unavailable"
			if tc.status == 302 {
				mock.ExpectExec(`UPDATE short_links SET visits_used=visits_used\+1 WHERE link_id=\$1`).
					WithArgs(r.ID).WillReturnResult(sqlmock.NewResult(0, 1))
				result, reason = "allowed", ""
			}
			expectAccessLog(mock, r, result, reason, tc.status == 302, "link_access").
				WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(1))
			mock.ExpectCommit()
			got, err := s.Access(context.Background(), "test-code", testAccessMeta(), "", false)
			if err != nil || got.StatusCode != tc.status {
				t.Fatalf("Access = %+v, %v; want status %d", got, err, tc.status)
			}
			if tc.status == 302 && got.Location != tc.location || tc.status != 302 && got.Location != "" {
				t.Fatalf("unexpected Location %q", got.Location)
			}
			wantCalls := 0
			if tc.asset && tc.available && !tc.noStore && !tc.missingKey {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("signing calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestAccessTransactionFailuresNeverReturnRedirect(t *testing.T) {
	for _, stage := range []string{"counter", "log", "commit", "denial_log"} {
		t.Run(stage, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			failure := errors.New("database failure")
			if stage == "denial_log" {
				r.MaxVisits = 0
			}
			expectAccessLock(mock, r, "active")
			if stage != "denial_log" {
				update := mock.ExpectExec(`UPDATE short_links SET visits_used=visits_used\+1 WHERE link_id=\$1`).WithArgs(r.ID)
				if stage == "counter" {
					update.WillReturnError(failure)
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if stage == "denial_log" {
				expectAccessLog(mock, r, "denied", "visit_limit_reached", false, "link_access").WillReturnError(failure)
			} else if stage != "counter" {
				log := expectAccessLog(mock, r, "allowed", "", true, "link_access")
				if stage == "log" {
					log.WillReturnError(failure)
				} else {
					log.WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(1))
				}
			}
			if stage == "commit" {
				mock.ExpectCommit().WillReturnError(failure)
			} else {
				mock.ExpectRollback()
			}
			got, err := s.Access(context.Background(), "test-code", testAccessMeta(), "", false)
			if !errors.Is(err, failure) || got.StatusCode == 302 || got.Location != "" {
				t.Fatalf("Access = %+v, %v; want failure without redirect", got, err)
			}
		})
	}
}

func TestAccessPasswordFlows(t *testing.T) {
	hash, err := auth.HashPassword("correct")
	if err != nil {
		t.Fatal(err)
	}
	for _, flow := range []string{"challenge", "wrong", "cooldown", "correct", "correct_commit_failure", "revoked"} {
		t.Run(flow, func(t *testing.T) {
			s, mock := newLinkTestService(t)
			r := testLink()
			r.RequirePassword, r.PasswordHash = true, sql.NullString{String: hash, Valid: true}
			codeStatus := "active"
			if flow == "revoked" {
				codeStatus = "revoked"
			}
			expectAccessLock(mock, r, codeStatus)
			wantStatus, wantChallenge := 200, true
			submit, password := flow != "challenge", "wrong"
			result, reason, eventType, consumes := "denied", "password_invalid", "password_attempt", false
			if flow == "challenge" {
				result, reason, eventType = "challenge", "password_required", "link_access"
			} else if flow == "revoked" {
				reason, eventType, wantStatus, wantChallenge = "code_revoked", "link_access", 410, false
			} else {
				mock.ExpectExec(`INSERT INTO short_link_password_attempt_state`).WithArgs(r.ID, "192.0.2.10").WillReturnResult(sqlmock.NewResult(0, 1))
				var locked any
				if flow == "cooldown" {
					locked = time.Now().Add(time.Hour)
				}
				mock.ExpectQuery(`SELECT window_started_at,failed_attempts,last_failed_at,cooldown_level,locked_until .* FOR UPDATE`).
					WithArgs(r.ID, "192.0.2.10").WillReturnRows(sqlmock.NewRows([]string{"window", "failures", "last_failed", "level", "locked"}).
					AddRow(time.Now(), 0, time.Now(), 0, locked))
				if strings.HasPrefix(flow, "correct") {
					password = "correct"
					mock.ExpectExec(`DELETE FROM short_link_password_attempt_state`).WithArgs(r.ID, "192.0.2.10").WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectExec(`UPDATE short_links SET visits_used=visits_used\+1`).WithArgs(r.ID).WillReturnResult(sqlmock.NewResult(0, 1))
					result, reason, eventType, consumes, wantStatus, wantChallenge = "allowed", "", "link_access", true, 302, false
				} else if flow == "cooldown" {
					reason, wantStatus, wantChallenge = "password_cooldown", 429, false
				} else {
					mock.ExpectExec(`UPDATE short_link_password_attempt_state`).
						WithArgs(r.ID, "192.0.2.10", sqlmock.AnyArg(), 1, sqlmock.AnyArg(), 0, nil).WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			expectAccessLog(mock, r, result, reason, consumes, eventType).WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(1))
			commit := mock.ExpectCommit()
			if flow == "correct_commit_failure" {
				commit.WillReturnError(errors.New("commit failed"))
			}
			got, err := s.Access(context.Background(), "test-code", testAccessMeta(), password, submit)
			if flow == "correct_commit_failure" {
				if err == nil || got.StatusCode == 302 || got.Location != "" {
					t.Fatalf("commit failure returned redirect: %+v, %v", got, err)
				}
			} else if err != nil || got.StatusCode != wantStatus || got.Challenge != wantChallenge {
				t.Fatalf("Access = %+v, %v; want status=%d challenge=%v", got, err, wantStatus, wantChallenge)
			}
		})
	}
}

func TestAccessRechecksWindowAfterPreparingLocation(t *testing.T) {
	s, mock := newLinkTestService(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.accessNow = func() time.Time { return now }
	r := testLink()
	r.ActiveUntil = sql.NullTime{Time: now.Add(time.Second), Valid: true}
	r.TargetType = "minio_asset"
	r.AssetID = sql.NullString{String: uuid.NewString(), Valid: true}
	r.Bucket = sql.NullString{String: "fixture-bucket", Valid: true}
	r.ObjectKey = sql.NullString{String: "file.png", Valid: true}
	s.store = presignedGetFunc(func(context.Context, string, string, time.Duration, string, string) (string, error) {
		now = r.ActiveUntil.Time
		return "https://files.example.test/file", nil
	})
	expectAccessLock(mock, r, "active")
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM assets WHERE asset_id=\$1 AND deleted_at IS NULL AND purged_at IS NULL\)`).
		WithArgs(r.AssetID.String).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectAccessLog(mock, r, "denied", "window_ended", false, "link_access").
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(1))
	mock.ExpectCommit()
	got, err := s.Access(context.Background(), "test-code", testAccessMeta(), "", false)
	if err != nil || got.StatusCode != 410 || got.Location != "" {
		t.Fatalf("Access = %+v, %v; want 410 without redirect", got, err)
	}
}

func TestNewServiceNilStore(t *testing.T) {
	var store *objectstore.Store
	if s := NewService(nil, config.Config{}, store); s.store != nil {
		t.Fatal("typed nil must remain an unconfigured store")
	}
}
