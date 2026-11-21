package logs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const exportMaxRows = 10000

func validateAccessFilter(actor Actor, filter AccessFilter) error {
	if !validAccessResult(filter.Result) || len(filter.Reason) > 40 || len(filter.BusinessID) > 64 {
		return problem(422, "invalid_filter", "日志筛选参数无效")
	}
	if actor.Role != "admin" && (filter.WorkspaceID != "" || filter.BusinessID != "" || filter.LinkOwnerUserID != "") {
		return problem(422, "invalid_filter", "普通用户不能指定管理员筛选范围")
	}
	return nil
}

func validateAuditFilter(actor Actor, filter AuditFilter) error {
	if !validAuditResult(filter.Result) || len(filter.Action) > 80 || len(filter.ResourceType) > 40 || len(filter.ResourceID) > 128 || len(filter.BusinessID) > 64 {
		return problem(422, "invalid_filter", "日志筛选参数无效")
	}
	if actor.Role != "admin" && (filter.WorkspaceID != "" || filter.ActorUserID != "" || filter.BusinessID != "") {
		return problem(422, "invalid_filter", "普通用户不能指定管理员筛选范围")
	}
	return nil
}

func exportRange(fromRaw, toRaw string) (time.Time, time.Time, error) {
	from, to, err := parseRange(fromRaw, toRaw)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to.Sub(from) > 90*24*time.Hour {
		return time.Time{}, time.Time{}, problem(422, "export_time_range_exceeded", "日志导出时间范围不能超过 90 天，请缩小范围")
	}
	return from, to, nil
}

func (s *Service) ExportAccessCSV(ctx context.Context, actor Actor, filter AccessFilter, write func([]string) error) (int, error) {
	if err := validateAccessFilter(actor, filter); err != nil {
		return 0, err
	}
	from, to, err := exportRange(filter.From, filter.To)
	if err != nil {
		return 0, err
	}
	where, args, err := accessWhere(actor, filter, from, to)
	if err != nil {
		return 0, err
	}
	args = append(args, exportMaxRows+1)
	rows, err := s.db.QueryContext(ctx, `SELECT l.event_id,l.link_id,l.accessed_at,l.source_ip::text,l.user_agent,l.result,l.reason,l.consumes_visit,l.request_id FROM short_link_access_logs l JOIN short_links sl ON sl.link_id=l.link_id WHERE `+where+fmt.Sprintf(" ORDER BY l.accessed_at DESC,l.event_id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if err := write([]string{"event_id", "link_id", "accessed_at_utc", "source_ip", "user_agent", "result", "reason", "consumes_visit", "request_id"}); err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if count == exportMaxRows {
			return count, problem(422, "export_limit_exceeded", "单次最多导出 10000 条，请缩小筛选范围")
		}
		var eventID int64
		var linkID uuid.UUID
		var at time.Time
		var ip, userAgent, reason, requestID sql.NullString
		var result string
		var consumes bool
		if err := rows.Scan(&eventID, &linkID, &at, &ip, &userAgent, &result, &reason, &consumes, &requestID); err != nil {
			return count, err
		}
		if err := write([]string{strconv.FormatInt(eventID, 10), publicID("link", linkID), at.UTC().Format(time.RFC3339Nano), ip.String, userAgent.String, result, reason.String, strconv.FormatBool(consumes), requestID.String}); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

func (s *Service) ExportAuditCSV(ctx context.Context, actor Actor, filter AuditFilter, write func([]string) error) (int, error) {
	if err := validateAuditFilter(actor, filter); err != nil {
		return 0, err
	}
	from, to, err := exportRange(filter.From, filter.To)
	if err != nil {
		return 0, err
	}
	where, args, err := auditWhere(actor, filter, from, to)
	if err != nil {
		return 0, err
	}
	args = append(args, exportMaxRows+1)
	rows, err := s.db.QueryContext(ctx, `SELECT a.event_id,a.actor_user_id,a.event_type,a.target_type,a.target_id,a.occurred_at,a.source_ip::text,a.result,a.details,a.request_id FROM audit_logs a WHERE `+where+fmt.Sprintf(" ORDER BY a.occurred_at DESC,a.event_id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if err := write([]string{"event_id", "actor_type", "actor_id", "action", "resource_type", "resource_id", "occurred_at_utc", "source_ip", "result", "details_json", "request_id"}); err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if count == exportMaxRows {
			return count, problem(422, "export_limit_exceeded", "单次最多导出 10000 条，请缩小筛选范围")
		}
		var eventID int64
		var actorID uuid.NullUUID
		var action, result string
		var kind, id, ip, requestID sql.NullString
		var at time.Time
		var raw []byte
		if err := rows.Scan(&eventID, &actorID, &action, &kind, &id, &at, &ip, &result, &raw, &requestID); err != nil {
			return count, err
		}
		actorType, actorValue := "system", ""
		if actorID.Valid {
			actorType, actorValue = "user", publicID("usr", actorID.UUID)
		}
		resource := ""
		if value := publicResourceID(kind.String, id); value != nil {
			resource = *value
		}
		details, err := json.Marshal(safeDetails(raw))
		if err != nil {
			return count, err
		}
		if err := write([]string{strconv.FormatInt(eventID, 10), actorType, actorValue, action, kind.String, resource, at.UTC().Format(time.RFC3339Nano), ip.String, result, string(details), requestID.String}); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}
