package notifications

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newNotificationTestService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
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

func TestNotificationServiceList(t *testing.T) {
	recipient, workspace, link := uuid.New(), uuid.New(), uuid.New()
	unreadID, readID := uuid.New(), uuid.New()
	created := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	readAt := created.Add(time.Minute)
	linkPublicID := "link_" + link.String()
	unread := Notification{
		NotificationID: "ntf_" + unreadID.String(), WorkspaceID: "ws_" + workspace.String(),
		Kind: KindExpiry24h, Title: "Expiring link", Summary: "Expires tomorrow", CreatedAt: created,
	}
	read := Notification{
		NotificationID: "ntf_" + readID.String(), WorkspaceID: "ws_" + workspace.String(), LinkID: &linkPublicID,
		Kind: KindVisits80, Title: "Visit limit", Summary: "80% used", CreatedAt: created, ReadAt: &readAt,
	}
	for _, tc := range []struct {
		name                   string
		unreadOnly             bool
		page, pageSize, offset int
		total                  int64
		items                  []Notification
	}{
		{name: "unread page", unreadOnly: true, page: 2, pageSize: 2, offset: 2, total: 3, items: []Notification{unread}},
		{name: "includes read", page: 1, pageSize: 20, total: 2, items: []Notification{unread, read}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock := newNotificationTestService(t)
			const recipientFilter = `recipient_user_id=$1 AND (NOT $2 OR read_at IS NULL)`
			mock.ExpectQuery(`SELECT count(*) FROM notifications WHERE `+recipientFilter).
				WithArgs(recipient, tc.unreadOnly).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.total))
			rows := sqlmock.NewRows([]string{"notification_id", "workspace_id", "link_id", "kind", "title", "summary", "created_at", "read_at"}).
				AddRow(unreadID, workspace, nil, unread.Kind, unread.Title, unread.Summary, created, nil)
			if !tc.unreadOnly {
				rows.AddRow(readID, workspace, link, read.Kind, read.Title, read.Summary, created, readAt)
			}
			mock.ExpectQuery(`SELECT notification_id,workspace_id,link_id,kind,title,summary,created_at,read_at
				FROM notifications WHERE `+recipientFilter+` ORDER BY created_at DESC,notification_id DESC LIMIT $3 OFFSET $4`).
				WithArgs(recipient, tc.unreadOnly, tc.pageSize, tc.offset).WillReturnRows(rows)

			got, err := svc.List(t.Context(), recipient, tc.unreadOnly, tc.page, tc.pageSize)
			want := Page{Items: tc.items, Pagination: Pagination{Page: tc.page, PageSize: tc.pageSize, Total: tc.total}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("List = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestNotificationServiceMarkRead(t *testing.T) {
	recipient, id := uuid.New(), uuid.New()
	const update = `UPDATE notifications SET read_at=COALESCE(read_at,now())
		WHERE notification_id=$1 AND recipient_user_id=$2`

	t.Run("repeated calls preserve first read timestamp in SQL", func(t *testing.T) {
		svc, mock := newNotificationTestService(t)
		for attempt := 1; attempt <= 2; attempt++ {
			mock.ExpectExec(update).WithArgs(id, recipient).WillReturnResult(sqlmock.NewResult(0, 1))
			if err := svc.MarkRead(t.Context(), recipient, "ntf_"+id.String()); err != nil {
				t.Fatalf("MarkRead attempt %d: %v", attempt, err)
			}
		}
	})

	t.Run("no matching recipient row", func(t *testing.T) {
		svc, mock := newNotificationTestService(t)
		mock.ExpectExec(update).WithArgs(id, recipient).WillReturnResult(sqlmock.NewResult(0, 0))
		err := svc.MarkRead(t.Context(), recipient, "ntf_"+id.String())
		var problem *Problem
		if !errors.As(err, &problem) || problem.Status != http.StatusNotFound || problem.Code != "notification_not_found" {
			t.Fatalf("MarkRead = %#v; want 404 notification_not_found", err)
		}
	})
}

func TestNotificationServiceUnreadCountAndReadAll(t *testing.T) {
	svc, mock := newNotificationTestService(t)
	recipient := uuid.New()
	const unreadRecipient = `WHERE recipient_user_id=$1 AND read_at IS NULL`
	mock.ExpectQuery(`SELECT count(*) FROM notifications ` + unreadRecipient).
		WithArgs(recipient).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	if got, err := svc.UnreadCount(t.Context(), recipient); err != nil || got != 3 {
		t.Fatalf("UnreadCount = %d, %v; want 3", got, err)
	}
	for _, want := range []int64{3, 0} {
		mock.ExpectExec(`UPDATE notifications SET read_at=now() ` + unreadRecipient).
			WithArgs(recipient).WillReturnResult(sqlmock.NewResult(0, want))
		if got, err := svc.ReadAll(t.Context(), recipient); err != nil || got != want {
			t.Fatalf("ReadAll = %d, %v; want %d", got, err, want)
		}
	}
}

func TestNotificationServiceGetSettingsDefaults(t *testing.T) {
	svc, mock := newNotificationTestService(t)
	recipient := uuid.New()
	mock.ExpectQuery(`SELECT COALESCE(ns.expiry_enabled,true),COALESCE(ns.visits_enabled,true)
		FROM users u LEFT JOIN notification_settings ns ON ns.user_id=u.user_id WHERE u.user_id=$1`).
		WithArgs(recipient).WillReturnRows(sqlmock.NewRows([]string{"expiry_enabled", "visits_enabled"}).AddRow(true, true))
	want := Settings{ExpiryEnabled: true, VisitsEnabled: true}
	if got, err := svc.GetSettings(t.Context(), recipient); err != nil || got != want {
		t.Fatalf("GetSettings = %+v, %v; want %+v", got, err, want)
	}
}

func TestNotificationServiceUpdateSettings(t *testing.T) {
	const upsert = `INSERT INTO notification_settings(user_id,expiry_enabled,visits_enabled,updated_at)
		VALUES($1,COALESCE($2::boolean,true),COALESCE($3::boolean,true),now())
		ON CONFLICT(user_id) DO UPDATE SET
		expiry_enabled=COALESCE($2::boolean,notification_settings.expiry_enabled),
		visits_enabled=COALESCE($3::boolean,notification_settings.visits_enabled),updated_at=now()
		RETURNING expiry_enabled,visits_enabled`
	const lock = `SELECT pg_advisory_xact_lock(hashtextextended('notification-settings:' || $1::uuid::text, 0))`
	recipient := uuid.New()
	disabled, enabled := false, true
	for _, tc := range []struct {
		name                 string
		input                UpdateSettingsRequest
		expiryArg, visitsArg any
		want                 Settings
	}{
		{
			name: "new settings default omitted visits to enabled", input: UpdateSettingsRequest{ExpiryEnabled: &disabled},
			expiryArg: false, want: Settings{ExpiryEnabled: false, VisitsEnabled: true},
		},
		{
			name: "visits patch preserves disabled expiry", input: UpdateSettingsRequest{VisitsEnabled: &disabled},
			visitsArg: false, want: Settings{ExpiryEnabled: false, VisitsEnabled: false},
		},
		{
			name: "expiry patch preserves disabled visits", input: UpdateSettingsRequest{ExpiryEnabled: &enabled},
			expiryArg: true, want: Settings{ExpiryEnabled: true, VisitsEnabled: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock := newNotificationTestService(t)
			mock.ExpectBegin()
			mock.ExpectExec(lock).WithArgs(recipient).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(upsert).WithArgs(recipient, tc.expiryArg, tc.visitsArg).
				WillReturnRows(sqlmock.NewRows([]string{"expiry_enabled", "visits_enabled"}).AddRow(tc.want.ExpiryEnabled, tc.want.VisitsEnabled))
			mock.ExpectCommit()

			if got, err := svc.UpdateSettings(t.Context(), recipient, tc.input); err != nil || got != tc.want {
				t.Fatalf("UpdateSettings = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	t.Run("upsert failure rolls back", func(t *testing.T) {
		svc, mock := newNotificationTestService(t)
		writeErr := errors.New("settings write failed")
		mock.ExpectBegin()
		mock.ExpectExec(lock).WithArgs(recipient).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(upsert).WithArgs(recipient, nil, false).WillReturnError(writeErr)
		mock.ExpectRollback()

		if _, err := svc.UpdateSettings(t.Context(), recipient, UpdateSettingsRequest{VisitsEnabled: &disabled}); !errors.Is(err, writeErr) {
			t.Fatalf("UpdateSettings = %v; want %v", err, writeErr)
		}
	})
}
