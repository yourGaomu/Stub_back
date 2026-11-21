package httpapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/platform/csvexport"
)

type exportRedisHook struct {
	status int
	err    error
	calls  int
}

func (h *exportRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unexpected network") }
}
func (h *exportRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *exportRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.calls++
		value, ok := cmd.(*redis.Cmd)
		if !ok {
			return errors.New("unexpected Redis command")
		}
		if h.err != nil {
			value.SetErr(h.err)
			return h.err
		}
		if len(cmd.Args()) > 2 && cmd.Args()[2] == 2 {
			value.SetVal(int64(h.status))
		} else {
			value.SetVal(int64(1))
		}
		return nil
	}
}

func TestCSVDownloadStagesBeforeHeaders(t *testing.T) {
	for _, mode := range []string{"success", "writer_error", "row_limit", "byte_limit", "empty", "cancelled", "deadline", "audit_start_error", "audit_result_error", "busy", "rate", "cache_error"} {
		t.Run(mode, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cache := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
			defer cache.Close()
			hook := &exportRedisHook{}
			if mode == "busy" {
				hook.status = 1
			}
			if mode == "rate" {
				hook.status = 2
			}
			if mode == "cache_error" {
				hook.err = errors.New("cache unavailable")
			}
			cache.AddHook(hook)
			deps := Dependencies{DB: db, Redis: cache}
			started := mode != "busy" && mode != "rate" && mode != "cache_error"
			if started {
				start := mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
				if mode == "audit_start_error" {
					start.WillReturnError(errors.New("audit unavailable"))
				} else {
					result := mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
					if mode == "audit_result_error" {
						result.WillReturnError(errors.New("audit unavailable"))
					}
				}
			}
			generated := false
			router := gin.New()
			router.GET("/export", func(c *gin.Context) {
				c.Set("authContext", authContext{User: auth.User{ID: uuid.New(), DefaultWorkspaceID: "ws_" + uuid.NewString(), Role: "user"}})
				runCSVExport(c, deps, "links", map[string]string{"keyword": "private"}, func(ctx context.Context, write func([]string) error) (int, error) {
					generated = true
					if c.Writer.Written() {
						t.Error("headers sent before file generation")
					}
					if mode == "empty" {
						return 0, nil
					}
					if err := write([]string{"title"}); err != nil {
						return 0, err
					}
					switch mode {
					case "writer_error":
						return 0, io.ErrUnexpectedEOF
					case "row_limit":
						return 0, csvexport.ErrLimit
					case "byte_limit":
						return 1, write([]string{strings.Repeat("x", csvexport.MaxBytes)})
					case "cancelled":
						return 0, context.Canceled
					case "deadline":
						return 0, context.DeadlineExceeded
					}
					return 1, write([]string{"=1+1"})
				})
			})
			res := httptest.NewRecorder()
			router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/export", nil))
			want := 500
			switch mode {
			case "success":
				want = 200
			case "row_limit", "byte_limit":
				want = 422
			case "cancelled":
				want = 408
			case "deadline":
				want = 504
			case "busy", "rate":
				want = 429
			case "cache_error":
				want = 503
			}
			if res.Code != want {
				t.Fatalf("status=%d want=%d body=%s", res.Code, want, res.Body.String())
			}
			if mode == "success" {
				if !strings.HasPrefix(res.Body.String(), "\xef\xbb\xbftitle\r\n'=1+1") || res.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(res.Header().Get("Content-Disposition"), "attachment;") || res.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("invalid download: %v %q", res.Header(), res.Body.String())
				}
			} else if res.Header().Get("Content-Disposition") != "" || !strings.Contains(res.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("failure sent download headers: %v", res.Header())
			}
			if !started && generated {
				t.Error("generated without limiter authorization")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCSVExportRequiresHumanSessionBeforeBusiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("authContext", authContext{APIKey: &auth.APIKeyIdentity{}}) })
	router.GET("/export", requireHumanSession(), func(c *gin.Context) { t.Error("machine credential reached export"); c.Status(200) })
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest("GET", "/export", nil))
	if res.Code != 403 {
		t.Fatalf("status=%d", res.Code)
	}
}
