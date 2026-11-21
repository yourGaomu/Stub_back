package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/yourGaomu/short_links/back/internal/config"
	"github.com/yourGaomu/short_links/back/internal/modules/assets"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/modules/business"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
	"github.com/yourGaomu/short_links/back/internal/modules/logs"
	"github.com/yourGaomu/short_links/back/internal/modules/templates"
)

func routerTestDependencies(t *testing.T, origins []string) (Dependencies, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	cache := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
		cache.Close()
	})
	cfg := config.Config{CORSAllowedOrigins: origins}
	return Dependencies{
		Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: db,
		DBPing: func(context.Context) error { return nil }, Redis: cache,
		Auth: auth.NewService(db, cache, cfg), Assets: assets.NewService(db, nil),
		Links: links.NewService(db, cfg, nil), Templates: templates.NewService(db),
		Businesses: business.NewService(db, nil), Logs: logs.NewService(db),
	}, mock
}

func TestRouterCORSPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		origins      []string
		status       int
	}{
		{"explicit_web", "https://app.example.com", []string{"https://app.example.com"}, 204},
		{"explicit_capacitor", "capacitor://localhost", []string{"capacitor://localhost"}, 204},
		{"explicit_https_localhost", "https://localhost", []string{"https://localhost"}, 204},
		{"development_origin_explicit", "http://localhost:5173", []string{"http://localhost:5173"}, 204},
		{"unlisted_origin", "https://other.example.com", []string{"https://app.example.com"}, 403},
		{"no_implicit_localhost", "http://localhost", []string{"https://app.example.com"}, 403},
		{"no_implicit_5174", "https://localhost:5174", []string{"https://app.example.com"}, 403},
		{"empty_rejects_web", "https://app.example.com", nil, 403},
		{"empty_rejects_dev", "http://localhost:5173", nil, 403},
		{"empty_rejects_capacitor", "capacitor://localhost", nil, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := routerTestDependencies(t, tc.origins)
			router, err := NewRouter(deps)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodOptions, "https://api.example.com/api/v1/links", nil)
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("Access-Control-Request-Method", "GET")
			request.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type,X-API-Key,X-Client-App,X-Request-ID")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("preflight = %d, want %d", response.Code, tc.status)
			}
			if tc.status == 204 {
				if response.Header().Get("Access-Control-Allow-Origin") != tc.origin || response.Header().Get("Access-Control-Allow-Credentials") != "true" {
					t.Fatalf("unexpected CORS headers: %v", response.Header())
				}
			} else if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("denied preflight must not allow origin")
			}
		})
	}
}

func TestRouterEmptyCORSAllowsSameOriginAndNonBrowser(t *testing.T) {
	deps, _ := routerTestDependencies(t, nil)
	router, err := NewRouter(deps)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"", "https://api.example.com", "https://unlisted.example.com"} {
		request := httptest.NewRequest(http.MethodGet, "https://api.example.com/healthz", nil)
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		want := 200
		if origin == "https://unlisted.example.com" {
			want = 403
		}
		if response.Code != want || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("origin %q: status=%d headers=%v", origin, response.Code, response.Header())
		}
	}
}

func TestRouterInvalidCORSReturnsError(t *testing.T) {
	deps, _ := routerTestDependencies(t, []string{"https://app.example.com/path"})
	if _, err := NewRouter(deps); err == nil {
		t.Fatal("invalid origin must return a startup error")
	}
}

func TestLinksMaxVisitsHandler(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		value       int64
		status      int
		filtered    bool
	}{
		{"unlimited", "?maxVisits=-1", -1, 200, true},
		{"zero", "?maxVisits=0", 0, 200, true},
		{"positive", "?maxVisits=12", 12, 200, true},
		{"omitted", "", 0, 200, false},
		{"empty", "?maxVisits=", 0, 422, false},
		{"negative", "?maxVisits=-2", 0, 422, false},
		{"fraction", "?maxVisits=1.5", 0, 422, false},
		{"overflow", "?maxVisits=9223372036854775808", 0, 422, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, mock := routerTestDependencies(t, nil)
			router, err := NewRouter(deps)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte("test-session"))
			mock.ExpectQuery(`SELECT u.user_id, u.default_workspace_id, s.session_id, .* FROM user_sessions s JOIN users u`).
				WithArgs(hex.EncodeToString(digest[:])).WillReturnRows(sqlmock.NewRows([]string{"user", "workspace", "session", "email", "role", "verified", "invited", "created"}).
				AddRow(uuid.New(), uuid.New(), uuid.New(), "admin@example.com", "admin", time.Now(), nil, time.Now()))
			if tc.status == 200 {
				filter := `TRUE`
				if tc.filtered {
					filter = `sl.max_visits=\$1`
				}
				count := mock.ExpectQuery(`SELECT count\(\*\) FROM short_links sl .* WHERE ` + filter)
				if tc.filtered {
					count.WithArgs(tc.value)
				}
				count.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
				list := mock.ExpectQuery(`SELECT sl.link_id, sc.code, .* WHERE ` + filter + ` ORDER BY sl.created_at DESC,sl.link_id DESC LIMIT \$[12] OFFSET \$[23]`)
				if tc.filtered {
					list.WithArgs(tc.value, 20, 0)
				} else {
					list.WithArgs(20, 0)
				}
				list.WillReturnRows(sqlmock.NewRows([]string{"link_id", "code", "title", "tags", "type", "url", "asset", "size", "status", "expires", "visits", "max", "created", "availability", "group_id", "group_name"}))
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/links"+tc.query, nil)
			request.Header.Set("Authorization", "Bearer test-session")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("GET links = %d, want %d; body=%s", response.Code, tc.status, response.Body.String())
			}
		})
	}
}
