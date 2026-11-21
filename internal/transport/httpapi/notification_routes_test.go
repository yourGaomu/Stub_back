package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/yourGaomu/short_links/back/internal/modules/notifications"
)

const (
	notificationInboxPath    = "/api/v1/notifications"
	notificationSettingsPath = "/api/v1/me/notification-settings"
)

var notificationFixtureTime = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func TestNotificationRoutesRequireHumanSession(t *testing.T) {
	const apiKey = "stub_sk_notification_test_only"
	for _, route := range []struct{ name, method, path, body string }{
		{"list", http.MethodGet, notificationInboxPath, ""},
		{"unread count", http.MethodGet, notificationInboxPath + "/unread-count", ""},
		{"mark read", http.MethodPatch, notificationInboxPath + "/ntf_" + uuid.NewString() + "/read", ""},
		{"read all", http.MethodPost, notificationInboxPath + "/read-all", ""},
		{"get settings", http.MethodGet, notificationSettingsPath, ""},
		{"patch settings", http.MethodPatch, notificationSettingsPath, `{"visitsEnabled":false}`},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, credential := range []struct{ name, header, value string }{
				{name: "anonymous"},
				{name: "api key header", header: "X-API-Key", value: apiKey},
				{name: "api key bearer", header: "Authorization", value: "Bearer " + apiKey},
			} {
				t.Run(credential.name, func(t *testing.T) {
					router, mock := newNotificationTestRouter(t)
					request := newNotificationRequest(route.method, route.path, route.body)
					status, code := http.StatusUnauthorized, "unauthorized"
					if credential.header != "" {
						expectValidNotificationAPIKey(mock, apiKey)
						request.Header.Set(credential.header, credential.value)
						status, code = http.StatusForbidden, "api_key_forbidden_endpoint"
					}
					response := serveNotificationRequest(t, router, request, status)
					assertNotificationRouteError(t, response, code)
				})
			}
		})
	}
}

func TestNotificationRoutesInboxIsScopedToSessionRecipient(t *testing.T) {
	// Both recipients share a workspace, and each owns a different notification.
	workspace, user, admin := uuid.New(), uuid.New(), uuid.New()
	userNotification, adminNotification, missing := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		role                   string
		queries                []string
		recipient, own, other  uuid.UUID
		unreadOnly             bool
		page, pageSize, offset int
		total                  int64
	}{
		{
			role: "user", queries: []string{"?unread=true&page=2&pageSize=2"},
			recipient: user, own: userNotification, other: adminNotification,
			unreadOnly: true, page: 2, pageSize: 2, offset: 2, total: 3,
		},
		{
			role: "admin", queries: []string{"", "?unread=false"},
			recipient: admin, own: adminNotification, other: userNotification,
			page: 1, pageSize: 20, total: 1,
		},
	} {
		t.Run(tc.role, func(t *testing.T) {
			router, mock := newNotificationTestRouter(t)
			session := notificationTestSession{recipient: tc.recipient, workspace: workspace, role: tc.role}
			for _, query := range tc.queries {
				request := session.request(mock, http.MethodGet, notificationInboxPath+query, "")
				const listPredicate = `recipient_user_id=\$1 AND \(NOT \$2 OR read_at IS NULL\)`
				mock.ExpectQuery(`^SELECT count\(\*\) FROM notifications WHERE `+listPredicate+`$`).
					WithArgs(tc.recipient, tc.unreadOnly).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.total))
				mock.ExpectQuery(`^SELECT .* FROM notifications WHERE `+listPredicate+` ORDER BY .* LIMIT \$3 OFFSET \$4$`).
					WithArgs(tc.recipient, tc.unreadOnly, tc.pageSize, tc.offset).
					WillReturnRows(sqlmock.NewRows([]string{"notification_id", "workspace_id", "link_id", "kind", "title", "summary", "created_at", "read_at"}).
						AddRow(tc.own, workspace, nil, notifications.KindExpiry24h, "Expiring link", "Expires tomorrow", notificationFixtureTime, nil))
				page := notificationResponseJSON[struct {
					Items []struct {
						ID     string          `json:"notificationId"`
						ReadAt json.RawMessage `json:"readAt"`
					} `json:"items"`
					Pagination struct {
						Page     int   `json:"page"`
						PageSize int   `json:"pageSize"`
						Total    int64 `json:"total"`
					} `json:"pagination"`
				}](t, serveNotificationRequest(t, router, request, http.StatusOK))
				if page.Pagination.Page != tc.page || page.Pagination.PageSize != tc.pageSize || page.Pagination.Total != tc.total {
					t.Fatalf("pagination for %q = %+v; want page=%d pageSize=%d total=%d", query, page.Pagination, tc.page, tc.pageSize, tc.total)
				}
				if want := "ntf_" + tc.own.String(); len(page.Items) != 1 || page.Items[0].ID != want || string(page.Items[0].ReadAt) != "null" {
					t.Fatalf("items for %q = %+v; want only unread notification %s", query, page.Items, want)
				}
			}

			const unreadPredicate = `recipient_user_id=\$1 AND read_at IS NULL$`
			request := session.request(mock, http.MethodGet, notificationInboxPath+"/unread-count", "")
			mock.ExpectQuery(`^SELECT count\(\*\) FROM notifications WHERE ` + unreadPredicate).
				WithArgs(tc.recipient).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.total))
			counts := notificationResponseJSON[map[string]int64](t, serveNotificationRequest(t, router, request, http.StatusOK))
			if want := map[string]int64{"unreadCount": tc.total}; !reflect.DeepEqual(counts, want) {
				t.Fatalf("unread count = %v; want %v", counts, want)
			}

			for _, mark := range []struct {
				name   string
				id     uuid.UUID
				status int
				rows   int64
			}{
				{"own notification", tc.own, http.StatusNoContent, 1},
				{"other recipient", tc.other, http.StatusNotFound, 0},
				{"absent notification", missing, http.StatusNotFound, 0},
			} {
				t.Run(mark.name, func(t *testing.T) {
					request := session.request(mock, http.MethodPatch, notificationInboxPath+"/ntf_"+mark.id.String()+"/read", "")
					mock.ExpectExec(`^UPDATE notifications SET read_at=.* WHERE notification_id=\$1 AND recipient_user_id=\$2$`).
						WithArgs(mark.id, tc.recipient).WillReturnResult(sqlmock.NewResult(0, mark.rows))
					response := serveNotificationRequest(t, router, request, mark.status)
					if mark.status == http.StatusNotFound {
						assertNotificationRouteError(t, response, "notification_not_found")
					} else if response.Body.Len() != 0 {
						t.Fatalf("204 response has body %q", response.Body.String())
					}
				})
			}

			request = session.request(mock, http.MethodPost, notificationInboxPath+"/read-all", "")
			marked := tc.total - 1
			mock.ExpectExec(`^UPDATE notifications SET read_at=now\(\) WHERE ` + unreadPredicate).
				WithArgs(tc.recipient).WillReturnResult(sqlmock.NewResult(0, marked))
			counts = notificationResponseJSON[map[string]int64](t, serveNotificationRequest(t, router, request, http.StatusOK))
			if want := map[string]int64{"markedCount": marked}; !reflect.DeepEqual(counts, want) {
				t.Fatalf("read all count = %v; want %v", counts, want)
			}
		})
	}
}

func TestNotificationRoutesRejectInvalidListParameters(t *testing.T) {
	for _, tc := range []struct{ query, code string }{
		{"?unread=", "invalid_unread_filter"},
		{"?unread=1", "invalid_unread_filter"},
		{"?unread=true&unread=false", "invalid_unread_filter"},
		{"?page=0", "invalid_pagination"},
		{"?pageSize=101", "invalid_pagination"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			router, mock := newNotificationTestRouter(t)
			session := notificationTestSession{recipient: uuid.New(), workspace: uuid.New(), role: "user"}
			request := session.request(mock, http.MethodGet, notificationInboxPath+tc.query, "")
			response := serveNotificationRequest(t, router, request, http.StatusUnprocessableEntity)
			assertNotificationRouteError(t, response, tc.code)
		})
	}
}

func TestNotificationRoutesSettingsAreScopedToSessionUser(t *testing.T) {
	workspace := uuid.New()
	for _, role := range []string{"user", "admin"} {
		t.Run(role, func(t *testing.T) {
			router, mock := newNotificationTestRouter(t)
			session := notificationTestSession{recipient: uuid.New(), workspace: workspace, role: role}
			columns := []string{"expiry_enabled", "visits_enabled"}
			request := session.request(mock, http.MethodGet, notificationSettingsPath, "")
			mock.ExpectQuery(`^SELECT .* FROM users u LEFT JOIN notification_settings ns ON ns.user_id=u.user_id WHERE u.user_id=\$1$`).
				WithArgs(session.recipient).WillReturnRows(sqlmock.NewRows(columns).AddRow(true, true))
			settings := notificationResponseJSON[map[string]bool](t, serveNotificationRequest(t, router, request, http.StatusOK))
			if want := map[string]bool{"expiryEnabled": true, "visitsEnabled": true}; !reflect.DeepEqual(settings, want) {
				t.Fatalf("GET settings = %v; want %v", settings, want)
			}

			request = session.request(mock, http.MethodPatch, notificationSettingsPath, `{"visitsEnabled":false}`)
			mock.ExpectBegin()
			mock.ExpectExec(`^SELECT pg_advisory_xact_lock\(`).WithArgs(session.recipient).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`^INSERT INTO notification_settings\(user_id,.* RETURNING expiry_enabled,visits_enabled$`).
				WithArgs(session.recipient, nil, false).WillReturnRows(sqlmock.NewRows(columns).AddRow(true, false))
			mock.ExpectCommit()
			settings = notificationResponseJSON[map[string]bool](t, serveNotificationRequest(t, router, request, http.StatusOK))
			if want := map[string]bool{"expiryEnabled": true, "visitsEnabled": false}; !reflect.DeepEqual(settings, want) {
				t.Fatalf("PATCH settings = %v; want %v", settings, want)
			}

			request = session.request(mock, http.MethodPatch, notificationSettingsPath, `{}`)
			response := serveNotificationRequest(t, router, request, http.StatusUnprocessableEntity)
			assertNotificationRouteError(t, response, "empty_update")
		})
	}
}

func newNotificationTestRouter(t *testing.T) (http.Handler, sqlmock.Sqlmock) {
	t.Helper()
	deps, mock := routerTestDependencies(t, nil)
	deps.Redis.AddHook(notificationAuthRedisHook{})
	router, err := NewRouter(deps)
	if err != nil {
		t.Fatal(err)
	}
	return router, mock
}

type notificationTestSession struct {
	recipient, workspace uuid.UUID
	role                 string
}

func (s notificationTestSession) request(mock sqlmock.Sqlmock, method, path, body string) *http.Request {
	token := "notification-test-session-" + s.role
	digest := sha256.Sum256([]byte(token))
	mock.ExpectQuery(`SELECT u.user_id, u.default_workspace_id, s.session_id, .* FROM user_sessions s JOIN users u`).
		WithArgs(hex.EncodeToString(digest[:])).
		WillReturnRows(sqlmock.NewRows([]string{"user", "workspace", "session", "email", "role", "verified", "invited", "created"}).
			AddRow(s.recipient, s.workspace, uuid.New(), "inbox@example.test", s.role, notificationFixtureTime, nil, notificationFixtureTime))
	request := newNotificationRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func newNotificationRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func serveNotificationRequest(t *testing.T, router http.Handler, request *http.Request, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s = %d; want %d; body = %s", request.Method, request.URL, response.Code, wantStatus, response.Body.String())
	}
	return response
}

func notificationResponseJSON[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var body T
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return body
}

func assertNotificationRouteError(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	if got := notificationResponseJSON[apiErrorResponse](t, response).Error.Code; got != code {
		t.Fatalf("error code = %q; want %q; body = %s", got, code, response.Body.String())
	}
}

func expectValidNotificationAPIKey(mock sqlmock.Sqlmock, key string) {
	keyID, owner, workspace := uuid.New(), uuid.New(), uuid.New()
	at := notificationFixtureTime
	digest := sha256.Sum256([]byte(key))
	// An active admin-owned key with account access must still hit the human-session gate.
	mock.ExpectQuery(`^SELECT k.api_key_id,.* FROM api_keys k JOIN users u .* WHERE k.key_hash=\$1$`).
		WithArgs(hex.EncodeToString(digest[:])).
		WillReturnRows(sqlmock.NewRows([]string{
			"api_key_id", "user_id", "workspace_id", "business_id", "name", "description", "key_prefix", "status", "allow_account_access",
			"allowed_ip_cidrs", "rate_limit_per_min", "max_upload_bytes", "uploaded_bytes_used", "max_created_links", "created_links_used",
			"max_requests", "requests_used", "expires_at", "last_used_at", "last_used_ip", "revoked_at", "created_at", "updated_at",
			"email", "role", "verified_at", "user_created_at", "default_workspace_id", "invited_by_user_id",
		}).AddRow(keyID, owner, workspace, "canvas", "Mock key", "", "stub_sk_test", "active", true,
			"{}", 10, nil, 0, nil, 0, nil, 1, nil, nil, nil, nil, at, at, "key-owner@example.test", "admin", at, at, workspace, nil))
	mock.ExpectExec(`^UPDATE api_keys SET requests_used=requests_used\+1,.* WHERE api_key_id=\$1`).
		WithArgs(keyID, "192.0.2.1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`^SELECT permission_code FROM api_key_permissions WHERE api_key_id=\$1 ORDER BY permission_code$`).
		WithArgs(keyID).WillReturnRows(sqlmock.NewRows([]string{"permission_code"}).AddRow("links.read"))
}

type notificationAuthRedisHook struct{}

func (notificationAuthRedisHook) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unexpected Redis dial")
	}
}

func (notificationAuthRedisHook) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return errors.New("unexpected Redis pipeline") }
}

func (notificationAuthRedisHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if counter, ok := cmd.(*redis.IntCmd); ok && counter.Name() == "incr" {
			counter.SetVal(2) // The stubbed rate window is already open.
			return nil
		}
		return errors.New("unexpected Redis command: " + cmd.Name())
	}
}
