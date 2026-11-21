package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/yourGaomu/short_links/back/internal/modules/logs"
	"github.com/yourGaomu/short_links/back/internal/platform/csvexport"
)

var exportAcquireScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 1 end
local used = tonumber(redis.call('GET', KEYS[2]) or '0')
if used >= 10 then return 2 end
local count = redis.call('INCR', KEYS[2])
if count == 1 then redis.call('EXPIRE', KEYS[2], 3600) end
redis.call('SET', KEYS[1], ARGV[1], 'EX', 120)
return 0
`)

var exportReleaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

func registerExportRoutes(router *gin.Engine, deps Dependencies) {
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth), requireHumanSession())
	api.GET("/links/export.csv", requirePermission(deps.DB, "links.read"), func(c *gin.Context) {
		filters := map[string]string{}
		for _, key := range []string{"workspaceId", "status", "availability", "targetType", "code", "keyword", "assetId", "createdFrom", "createdTo", "groupId"} {
			if value := c.Query(key); value != "" {
				filters[key] = value
			}
		}
		for _, key := range []string{"maxVisits", "ungrouped"} {
			if values, present := c.Request.URL.Query()[key]; present && len(values) > 0 {
				filters[key] = values[0]
			}
		}
		runCSVExport(c, deps, "links", filters, func(ctx context.Context, write func([]string) error) (int, error) {
			return deps.Links.ExportCSV(ctx, linkActor(c), filters, write)
		})
	})
	api.GET("/logs/access/export.csv", requirePermission(deps.DB, "logs.read_own"), func(c *gin.Context) {
		filter, ok := accessExportFilter(c)
		if !ok {
			return
		}
		runCSVExport(c, deps, "access", appliedExportFilter(c), func(ctx context.Context, write func([]string) error) (int, error) {
			return deps.Logs.ExportAccessCSV(ctx, logActor(c), filter, write)
		})
	})
	api.GET("/logs/audit/export.csv", requirePermission(deps.DB, "logs.read_own"), func(c *gin.Context) {
		filter := logs.AuditFilter{
			WorkspaceID: c.Query("workspaceId"), ActorUserID: c.Query("actorUserId"), BusinessID: c.Query("businessId"),
			ResourceType: c.Query("resourceType"), ResourceID: c.Query("resourceId"), Action: c.Query("action"), Result: c.Query("result"),
			From: c.Query("from"), To: c.Query("to"),
		}
		runCSVExport(c, deps, "audit", appliedExportFilter(c), func(ctx context.Context, write func([]string) error) (int, error) {
			return deps.Logs.ExportAuditCSV(ctx, logActor(c), filter, write)
		})
	})
}

func accessExportFilter(c *gin.Context) (logs.AccessFilter, bool) {
	ungrouped := false
	if raw, present := c.Request.URL.Query()["ungrouped"]; present {
		value, err := strconv.ParseBool(raw[0])
		if err != nil {
			writeError(c, 422, "invalid_filter", "ungrouped 参数无效")
			return logs.AccessFilter{}, false
		}
		ungrouped = value
	}
	return logs.AccessFilter{
		LinkID: c.Query("linkId"), From: c.Query("from"), To: c.Query("to"), Result: c.Query("result"), Reason: c.Query("reason"),
		WorkspaceID: c.Query("workspaceId"), BusinessID: c.Query("businessId"), LinkOwnerUserID: c.Query("linkOwnerUserId"),
		GroupID: c.Query("groupId"), Ungrouped: ungrouped,
	}, true
}

func appliedExportFilter(c *gin.Context) map[string]string {
	filters := map[string]string{}
	for _, key := range []string{"linkId", "from", "to", "result", "reason", "workspaceId", "businessId", "linkOwnerUserId", "groupId", "ungrouped", "actorUserId", "resourceType", "resourceId", "action"} {
		if value := c.Query(key); value != "" {
			filters[key] = value
		}
	}
	return filters
}

func runCSVExport(c *gin.Context, deps Dependencies, kind string, filters map[string]string, generate func(context.Context, func([]string) error) (int, error)) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Minute)
	defer cancel()
	user := currentAuth(c).User.ID.String()
	lease, counter := "csv-export:active:"+user, "csv-export:hourly:"+user
	token := uuid.NewString()
	status, err := exportAcquireScript.Run(ctx, deps.Redis, []string{lease, counter}, token).Int()
	if err != nil {
		writeError(c, 503, "export_unavailable", "导出暂不可用，请稍后重试")
		return
	}
	if status != 0 {
		c.Header("Retry-After", "60")
		if status == 1 {
			writeError(c, 429, "export_in_progress", "当前已有导出正在生成，请稍后重试")
		} else {
			writeError(c, 429, "export_rate_limited", "每小时最多生成 10 次导出，请稍后重试")
		}
		return
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		_ = exportReleaseScript.Run(releaseCtx, deps.Redis, []string{lease}, token).Err()
	}()
	started := time.Now()
	keys := make([]string, 0, len(filters))
	for key := range filters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := recordExport(c, deps, ctx, kind, "export.start", "success", map[string]any{"filterKeys": keys}); err != nil {
		writeServiceError(c, err)
		return
	}
	file, err := csvexport.Prepare(ctx, func(write func([]string) error) (int, error) { return generate(ctx, write) })
	if err != nil {
		auditCtx, auditCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = recordExport(c, deps, auditCtx, kind, "export.generate", "failure", map[string]any{"failureCode": exportFailureCode(err), "durationMs": time.Since(started).Milliseconds()})
		auditCancel()
		switch {
		case errors.Is(err, csvexport.ErrLimit):
			writeError(c, 422, "export_limit_exceeded", "导出超过 10000 条或 20 MiB，请缩小筛选范围")
		case errors.Is(err, context.DeadlineExceeded):
			writeError(c, 504, "export_timeout", "导出超时，请缩小筛选范围后重试")
		case errors.Is(err, context.Canceled):
			writeError(c, 408, "export_cancelled", "导出已取消")
		default:
			writeServiceError(c, err)
		}
		return
	}
	defer file.Close()
	if err := recordExport(c, deps, ctx, kind, "export.generate", "success", map[string]any{"rowCount": file.Rows, "sizeBytes": file.Bytes, "durationMs": time.Since(started).Milliseconds()}); err != nil {
		writeServiceError(c, err)
		return
	}
	if err := ctx.Err(); err != nil {
		writeError(c, 504, "export_timeout", "导出超时，请重试")
		return
	}
	name := fmt.Sprintf("stub-%s-%s.csv", kind, time.Now().UTC().Format("20060102T150405Z"))
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, file.Bytes, "text/csv; charset=utf-8", file, nil)
}

func recordExport(c *gin.Context, deps Dependencies, ctx context.Context, kind, action, result string, details map[string]any) error {
	identity := currentAuth(c)
	workspace, err := parsePublicID("ws", identity.User.DefaultWorkspaceID)
	if err != nil {
		return err
	}
	details["exportKind"] = kind
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = deps.DB.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,'csv_export',$4,NULLIF($5,'')::inet,$6,$7,$8::jsonb,NULLIF($9,''))`, identity.User.ID, workspace, action, kind, c.ClientIP(), truncateLogValue(c.Request.UserAgent(), 1024), result, string(raw), requestID(c))
	return err
}

func exportFailureCode(err error) string {
	if errors.Is(err, csvexport.ErrLimit) {
		return "export_limit_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "export_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "export_timeout"
	}
	return "export_generation_failed"
}
