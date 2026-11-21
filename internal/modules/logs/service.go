package logs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Problem struct {
	Status  int
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

type Actor struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	Email       string
	Role        string
}

type Meta struct {
	SourceIP  string
	UserAgent string
	RequestID string
}

type Page[T any] struct {
	Items      []T `json:"items"`
	Pagination struct {
		Page     int   `json:"page"`
		PageSize int   `json:"pageSize"`
		Total    int64 `json:"total"`
	} `json:"pagination"`
}

type AccessFilter struct {
	LinkID          string
	From            string
	To              string
	Result          string
	Reason          string
	WorkspaceID     string
	BusinessID      string
	GroupID         string
	Ungrouped       bool
	LinkOwnerUserID string
	Page            int
	PageSize        int
}

type AccessEvent struct {
	EventID       int64     `json:"eventId"`
	LinkID        string    `json:"linkId"`
	AccessedAt    time.Time `json:"accessedAt"`
	SourceIP      *string   `json:"sourceIp"`
	UserAgent     *string   `json:"userAgent"`
	Result        string    `json:"result"`
	Reason        *string   `json:"reason"`
	ConsumesVisit bool      `json:"consumesVisit"`
	RequestID     *string   `json:"requestId"`
}

type AuditFilter struct {
	WorkspaceID  string
	ActorUserID  string
	BusinessID   string
	ResourceType string
	ResourceID   string
	Action       string
	Result       string
	From         string
	To           string
	Page         int
	PageSize     int
}

type AuditEvent struct {
	EventID      int64          `json:"eventId"`
	ActorType    string         `json:"actorType"`
	ActorID      *string        `json:"actorId"`
	Action       string         `json:"action"`
	ResourceType *string        `json:"resourceType"`
	ResourceID   *string        `json:"resourceId"`
	OccurredAt   time.Time      `json:"occurredAt"`
	SourceIP     *string        `json:"sourceIp"`
	Result       string         `json:"result"`
	Details      map[string]any `json:"details"`
	RequestID    *string        `json:"requestId"`
}

type UnknownFilter struct {
	From        string
	To          string
	SourceIP    string
	RateLimited *bool
	Page        int
	PageSize    int
}

type UnknownEvent struct {
	EventID         int64     `json:"eventId"`
	CodeFingerprint string    `json:"codeFingerprint"`
	WindowStartedAt time.Time `json:"windowStartedAt"`
	FirstOccurredAt time.Time `json:"firstOccurredAt"`
	LastOccurredAt  time.Time `json:"lastOccurredAt"`
	EventCount      int64     `json:"eventCount"`
	SourceIP        string    `json:"sourceIp"`
	UserAgent       *string   `json:"userAgent"`
	RequestID       *string   `json:"requestId"`
	RateLimited     bool      `json:"rateLimited"`
}

type Metric struct {
	RequestCount      int64 `json:"requestCount"`
	VisitAttemptCount int64 `json:"visitAttemptCount"`
	RedirectCount     int64 `json:"redirectCount"`
	FailureCount      int64 `json:"failureCount"`
}

type FailureReason struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type MetricBucket struct {
	At time.Time `json:"at"`
	Metric
}

type Analytics struct {
	Range          string          `json:"range"`
	From           time.Time       `json:"from"`
	To             time.Time       `json:"to"`
	TimeZone       string          `json:"timeZone"`
	Totals         Metric          `json:"totals"`
	FailureReasons []FailureReason `json:"failureReasons"`
	Series         []MetricBucket  `json:"series"`
}

type Service struct{ db *sql.DB }

func NewService(db *sql.DB) *Service { return &Service{db: db} }

func (s *Service) Access(ctx context.Context, actor Actor, filter AccessFilter) (Page[AccessEvent], error) {
	// 1. 校验分页、时间范围和筛选字段；普通用户的可见范围由身份强制限定。
	if err := validatePage(filter.Page, filter.PageSize); err != nil {
		return Page[AccessEvent]{}, err
	}
	from, to, err := parseRange(filter.From, filter.To)
	if err != nil {
		return Page[AccessEvent]{}, err
	}
	if err := validateAccessFilter(actor, filter); err != nil {
		return Page[AccessEvent]{}, err
	}

	// 2. 用参数化条件组合列表和总数查询，避免筛选值进入 SQL 文本。
	where, args, err := accessWhere(actor, filter, from, to)
	if err != nil {
		return Page[AccessEvent]{}, err
	}
	var total int64
	base := ` FROM short_link_access_logs l JOIN short_links sl ON sl.link_id=l.link_id WHERE ` + where
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+base, args...).Scan(&total); err != nil {
		return Page[AccessEvent]{}, err
	}
	queryArgs := append(append([]any{}, args...), filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT l.event_id,l.link_id,l.accessed_at,l.source_ip::text,l.user_agent,l.result,l.reason,l.consumes_visit,l.request_id`+base+
		fmt.Sprintf(" ORDER BY l.accessed_at DESC,l.event_id DESC LIMIT $%d OFFSET $%d", len(queryArgs)-1, len(queryArgs)), queryArgs...)
	if err != nil {
		return Page[AccessEvent]{}, err
	}
	defer rows.Close()
	items := make([]AccessEvent, 0)
	for rows.Next() {
		var item AccessEvent
		var linkID uuid.UUID
		var ip, userAgent, reason, requestID sql.NullString
		if err := rows.Scan(&item.EventID, &linkID, &item.AccessedAt, &ip, &userAgent, &item.Result, &reason, &item.ConsumesVisit, &requestID); err != nil {
			return Page[AccessEvent]{}, err
		}
		item.LinkID = publicID("link", linkID)
		item.AccessedAt = item.AccessedAt.UTC()
		item.SourceIP, item.UserAgent, item.Reason, item.RequestID = nullableString(ip), nullableString(userAgent), nullableString(reason), nullableString(requestID)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[AccessEvent]{}, err
	}
	return makePage(items, filter.Page, filter.PageSize, total), nil
}

func (s *Service) Audit(ctx context.Context, actor Actor, filter AuditFilter) (Page[AuditEvent], error) {
	// 1. 校验查询范围；普通用户只能看到本人操作或本人工作空间资源的审计记录。
	if err := validatePage(filter.Page, filter.PageSize); err != nil {
		return Page[AuditEvent]{}, err
	}
	from, to, err := parseRange(filter.From, filter.To)
	if err != nil {
		return Page[AuditEvent]{}, err
	}
	if err := validateAuditFilter(actor, filter); err != nil {
		return Page[AuditEvent]{}, err
	}

	// 2. 使用固定白名单字段生成 SQL 谓词，并以稳定资源 ID 返回审计数据。
	where, args, err := auditWhere(actor, filter, from, to)
	if err != nil {
		return Page[AuditEvent]{}, err
	}
	base := ` FROM audit_logs a WHERE ` + where
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+base, args...).Scan(&total); err != nil {
		return Page[AuditEvent]{}, err
	}
	queryArgs := append(append([]any{}, args...), filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT a.event_id,a.actor_user_id,a.event_type,a.target_type,a.target_id,a.occurred_at,a.source_ip::text,a.result,a.details,a.request_id`+base+
		fmt.Sprintf(" ORDER BY a.occurred_at DESC,a.event_id DESC LIMIT $%d OFFSET $%d", len(queryArgs)-1, len(queryArgs)), queryArgs...)
	if err != nil {
		return Page[AuditEvent]{}, err
	}
	defer rows.Close()
	items := make([]AuditEvent, 0)
	for rows.Next() {
		var item AuditEvent
		var actorID uuid.NullUUID
		var resourceType, resourceID, sourceIP, requestID sql.NullString
		var rawDetails []byte
		if err := rows.Scan(&item.EventID, &actorID, &item.Action, &resourceType, &resourceID, &item.OccurredAt, &sourceIP, &item.Result, &rawDetails, &requestID); err != nil {
			return Page[AuditEvent]{}, err
		}
		item.ActorType = "system"
		if actorID.Valid {
			item.ActorType = "user"
			id := publicID("usr", actorID.UUID)
			item.ActorID = &id
		}
		item.ResourceType = nullableString(resourceType)
		item.ResourceID = publicResourceID(resourceType.String, resourceID)
		item.OccurredAt = item.OccurredAt.UTC()
		item.SourceIP, item.RequestID = nullableString(sourceIP), nullableString(requestID)
		item.Details = safeDetails(rawDetails)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[AuditEvent]{}, err
	}
	return makePage(items, filter.Page, filter.PageSize, total), nil
}

func (s *Service) UnknownShortCodes(ctx context.Context, actor Actor, filter UnknownFilter, meta Meta) (Page[UnknownEvent], error) {
	// 1. 未知短码安全事件只允许管理员查询，筛选时间基于聚合窗口起始时刻。
	if actor.Role != "admin" {
		return Page[UnknownEvent]{}, problem(403, "admin_required", "需要管理员权限")
	}
	if err := validatePage(filter.Page, filter.PageSize); err != nil {
		return Page[UnknownEvent]{}, err
	}
	from, to, err := parseRange(filter.From, filter.To)
	if err != nil {
		return Page[UnknownEvent]{}, err
	}
	if filter.SourceIP != "" && net.ParseIP(filter.SourceIP) == nil {
		return Page[UnknownEvent]{}, problem(422, "invalid_filter", "来源 IP 无效")
	}
	where := []string{"window_started_at >= $1", "window_started_at < $2"}
	args := []any{from, to}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.SourceIP != "" {
		add("source_ip=$%d::inet", filter.SourceIP)
	}
	if filter.RateLimited != nil {
		add("rate_limited=$%d", *filter.RateLimited)
	}
	base := ` FROM unknown_short_code_events WHERE ` + strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+base, args...).Scan(&total); err != nil {
		return Page[UnknownEvent]{}, err
	}
	queryArgs := append(append([]any{}, args...), filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,code_fingerprint,window_started_at,first_occurred_at,last_occurred_at,event_count,source_ip::text,user_agent,request_id,rate_limited`+base+
		fmt.Sprintf(" ORDER BY last_occurred_at DESC,event_id DESC LIMIT $%d OFFSET $%d", len(queryArgs)-1, len(queryArgs)), queryArgs...)
	if err != nil {
		return Page[UnknownEvent]{}, err
	}
	defer rows.Close()
	items := make([]UnknownEvent, 0)
	for rows.Next() {
		var item UnknownEvent
		var userAgent, requestID sql.NullString
		if err := rows.Scan(&item.EventID, &item.CodeFingerprint, &item.WindowStartedAt, &item.FirstOccurredAt, &item.LastOccurredAt, &item.EventCount, &item.SourceIP, &userAgent, &requestID, &item.RateLimited); err != nil {
			return Page[UnknownEvent]{}, err
		}
		item.WindowStartedAt = item.WindowStartedAt.UTC()
		item.FirstOccurredAt = item.FirstOccurredAt.UTC()
		item.LastOccurredAt = item.LastOccurredAt.UTC()
		item.UserAgent, item.RequestID = nullableString(userAgent), nullableString(requestID)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[UnknownEvent]{}, err
	}
	result := makePage(items, filter.Page, filter.PageSize, total)
	// 2. 安全事件查询本身也留下管理员审计记录，不写入任何原始短码。
	details, err := json.Marshal(map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)})
	if err != nil {
		return Page[UnknownEvent]{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,result,source_ip,user_agent,details,request_id) VALUES($1,$2,'admin.security_events.read','unknown_short_code_events','success',NULLIF($3,'')::inet,$4,$5::jsonb,NULLIF($6,''))`, actor.UserID, actor.WorkspaceID, meta.SourceIP, meta.UserAgent, string(details), meta.RequestID)
	if err != nil {
		return Page[UnknownEvent]{}, err
	}
	return result, nil
}

func (s *Service) Summary(ctx context.Context, actor Actor, rangeName, timeZone, workspaceID string) (Analytics, error) {
	// 1. 普通用户汇总本人短链；管理员可以显式选择一个工作空间。
	return s.analytics(ctx, actor, "", rangeName, timeZone, workspaceID)
}

func (s *Service) Link(ctx context.Context, actor Actor, rawLinkID, rangeName, timeZone string) (Analytics, error) {
	// 1. 单链统计先验证短链可见范围，避免用统计接口探测其他用户的链接。
	linkID, err := parseID("link", rawLinkID)
	if err != nil {
		return Analytics{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	var visible bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM short_links WHERE link_id=$1 AND ($2='admin' OR created_by=$3))`, linkID, actor.Role, actor.UserID).Scan(&visible); err != nil {
		return Analytics{}, err
	}
	if !visible {
		return Analytics{}, problem(404, "link_not_found", "短链不存在或无权访问")
	}
	return s.analytics(ctx, actor, rawLinkID, rangeName, timeZone, "")
}

func (s *Service) analytics(ctx context.Context, actor Actor, rawLinkID, rangeName, timeZone, workspaceID string) (Analytics, error) {
	// 1. 解析统计窗口与 IANA 时区；所有时间边界统一使用 UTC 查询。
	if rangeName == "" {
		rangeName = "30d"
	}
	if timeZone == "" {
		timeZone = "UTC"
	}
	loc, err := time.LoadLocation(timeZone)
	if err != nil {
		return Analytics{}, problem(422, "invalid_time_zone", "timeZone 不是有效的 IANA 时区")
	}
	now := time.Now().UTC()
	var from time.Time
	var unit string
	switch rangeName {
	case "24h":
		from, unit = now.Add(-24*time.Hour), "hour"
	case "7d":
		local := now.In(loc).AddDate(0, 0, -6)
		from, unit = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC(), "day"
	case "30d":
		local := now.In(loc).AddDate(0, 0, -29)
		from, unit = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC(), "day"
	default:
		return Analytics{}, problem(422, "invalid_range", "range 只能是 24h、7d 或 30d")
	}
	if actor.Role != "admin" && workspaceID != "" {
		return Analytics{}, problem(422, "invalid_filter", "普通用户不能指定工作空间")
	}
	var workspace uuid.UUID
	if workspaceID != "" {
		workspace, err = parseID("ws", workspaceID)
		if err != nil {
			return Analytics{}, problem(422, "invalid_filter", "工作空间参数无效")
		}
	}
	var linkID uuid.UUID
	if rawLinkID != "" {
		linkID, _ = parseID("link", rawLinkID)
	}

	// 2. 通过同一访问日志事实表计算汇总、失败原因与时间桶，不猜测 302 后的目标结果。
	where := []string{"l.accessed_at >= $1", "l.accessed_at < $2"}
	args := []any{from, now}
	if actor.Role != "admin" {
		args = append(args, actor.UserID)
		where = append(where, fmt.Sprintf("sl.created_by=$%d", len(args)))
	} else if workspaceID != "" {
		args = append(args, workspace)
		where = append(where, fmt.Sprintf("sl.workspace_id=$%d", len(args)))
	}
	if rawLinkID != "" {
		args = append(args, linkID)
		where = append(where, fmt.Sprintf("l.link_id=$%d", len(args)))
	}
	filter := strings.Join(where, " AND ")
	var totals Metric
	query := `SELECT count(*),count(*) FILTER(WHERE l.event_type='link_access' AND l.result!='challenge'),count(*) FILTER(WHERE l.event_type='link_access' AND l.result='allowed' AND l.reason IS NULL),count(*) FILTER(WHERE l.result='denied') FROM short_link_access_logs l JOIN short_links sl ON sl.link_id=l.link_id WHERE ` + filter
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&totals.RequestCount, &totals.VisitAttemptCount, &totals.RedirectCount, &totals.FailureCount); err != nil {
		return Analytics{}, err
	}
	reasonRows, err := s.db.QueryContext(ctx, `SELECT l.reason,count(*) FROM short_link_access_logs l JOIN short_links sl ON sl.link_id=l.link_id WHERE `+filter+` AND l.result='denied' AND l.reason IS NOT NULL GROUP BY l.reason ORDER BY count(*) DESC,l.reason`, args...)
	if err != nil {
		return Analytics{}, err
	}
	failures := make([]FailureReason, 0)
	for reasonRows.Next() {
		var item FailureReason
		if err := reasonRows.Scan(&item.Reason, &item.Count); err != nil {
			reasonRows.Close()
			return Analytics{}, err
		}
		failures = append(failures, item)
	}
	if err := reasonRows.Err(); err != nil {
		reasonRows.Close()
		return Analytics{}, err
	}
	reasonRows.Close()

	// 3. 聚合桶使用本地时区边界；请求总量仍使用精确 UTC 时间窗口。
	var bucketExpression string
	bucketArgs := append([]any{}, args...)
	if unit == "hour" {
		// UTC 小时桶在夏令时回拨时也保持唯一；客户端按 timeZone 格式化 At。
		bucketExpression = `date_trunc('hour',l.accessed_at)`
	} else {
		tzIndex := len(args) + 1
		bucketExpression = fmt.Sprintf(`date_trunc('day',l.accessed_at AT TIME ZONE $%d) AT TIME ZONE $%d`, tzIndex, tzIndex)
		bucketArgs = append(bucketArgs, timeZone)
	}
	bucketQuery := `SELECT ` + bucketExpression + `,count(*),count(*) FILTER(WHERE l.event_type='link_access' AND l.result!='challenge'),count(*) FILTER(WHERE l.event_type='link_access' AND l.result='allowed' AND l.reason IS NULL),count(*) FILTER(WHERE l.result='denied') FROM short_link_access_logs l JOIN short_links sl ON sl.link_id=l.link_id WHERE ` + filter + fmt.Sprintf(" GROUP BY 1 ORDER BY 1")
	rows, err := s.db.QueryContext(ctx, bucketQuery, bucketArgs...)
	if err != nil {
		return Analytics{}, err
	}
	byBucket := map[string]Metric{}
	layout := "2006-01-02"
	for rows.Next() {
		var at time.Time
		var metric Metric
		if err := rows.Scan(&at, &metric.RequestCount, &metric.VisitAttemptCount, &metric.RedirectCount, &metric.FailureCount); err != nil {
			rows.Close()
			return Analytics{}, err
		}
		key := at.In(loc).Format(layout)
		if unit == "hour" {
			key = at.UTC().Format(time.RFC3339)
		}
		byBucket[key] = metric
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Analytics{}, err
	}
	rows.Close()
	series := make([]MetricBucket, 0)
	if unit == "hour" {
		start := from.UTC().Truncate(time.Hour)
		for at := start; !at.After(now); at = at.Add(time.Hour) {
			series = append(series, MetricBucket{At: at.In(loc), Metric: byBucket[at.Format(time.RFC3339)]})
		}
	} else {
		startLocal := from.In(loc)
		at := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, loc)
		endLocal := now.In(loc)
		for !at.After(endLocal) {
			series = append(series, MetricBucket{At: at, Metric: byBucket[at.Format(layout)]})
			at = at.AddDate(0, 0, 1)
		}
	}
	return Analytics{Range: rangeName, From: from, To: now, TimeZone: timeZone, Totals: totals, FailureReasons: failures, Series: series}, nil
}

func accessWhere(actor Actor, filter AccessFilter, from, to time.Time) (string, []any, error) {
	where := []string{"l.accessed_at >= $1", "l.accessed_at < $2"}
	args := []any{from, to}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if actor.Role != "admin" {
		add("sl.created_by=$%d", actor.UserID)
	} else {
		if filter.WorkspaceID != "" {
			id, err := parseID("ws", filter.WorkspaceID)
			if err != nil {
				return "", nil, problem(422, "invalid_filter", "工作空间参数无效")
			}
			add("sl.workspace_id=$%d", id)
		}
		if filter.LinkOwnerUserID != "" {
			id, err := parseID("usr", filter.LinkOwnerUserID)
			if err != nil {
				return "", nil, problem(422, "invalid_filter", "用户参数无效")
			}
			add("sl.created_by=$%d", id)
		}
	}
	if filter.LinkID != "" {
		id, err := parseID("link", filter.LinkID)
		if err != nil {
			return "", nil, problem(422, "invalid_filter", "短链 ID 无效")
		}
		add("l.link_id=$%d", id)
	}
	if filter.GroupID != "" && filter.Ungrouped {
		return "", nil, problem(422, "invalid_filter", "groupId 与 ungrouped 不能同时提供")
	}
	if filter.GroupID != "" {
		id, err := parseID("grp", filter.GroupID)
		if err != nil {
			return "", nil, problem(422, "invalid_filter", "分组 ID 无效")
		}
		add("sl.group_id=$%d", id)
	} else if filter.Ungrouped {
		where = append(where, "sl.group_id IS NULL")
	}
	if filter.BusinessID != "" {
		add("l.business_id=$%d", filter.BusinessID)
	}
	if filter.Result != "" {
		add("l.result=$%d", filter.Result)
	}
	if filter.Reason != "" {
		add("l.reason=$%d", filter.Reason)
	}
	return strings.Join(where, " AND "), args, nil
}

func auditWhere(actor Actor, filter AuditFilter, from, to time.Time) (string, []any, error) {
	where := []string{"a.occurred_at >= $1", "a.occurred_at < $2"}
	args := []any{from, to}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if actor.Role != "admin" {
		args = append(args, actor.UserID, actor.WorkspaceID, actor.Email)
		where = append(where, fmt.Sprintf("(a.actor_user_id=$%d OR a.workspace_id=$%d OR (a.target_type='user_email' AND lower(a.target_id)=lower($%d)))", len(args)-2, len(args)-1, len(args)))
	} else {
		if filter.WorkspaceID != "" {
			id, err := parseID("ws", filter.WorkspaceID)
			if err != nil {
				return "", nil, problem(422, "invalid_filter", "工作空间参数无效")
			}
			add("a.workspace_id=$%d", id)
		}
		if filter.ActorUserID != "" {
			id, err := parseID("usr", filter.ActorUserID)
			if err != nil {
				return "", nil, problem(422, "invalid_filter", "操作者用户 ID 无效")
			}
			add("a.actor_user_id=$%d", id)
		}
	}
	if filter.BusinessID != "" {
		args = append(args, filter.BusinessID)
		placeholder := len(args)
		where = append(where, fmt.Sprintf("(a.details->>'businessId'=$%d OR (a.target_type='business' AND a.target_id=$%d) OR (a.target_type='business_access' AND split_part(a.target_id,':',1)=$%d))", placeholder, placeholder, placeholder))
	}
	if filter.ResourceType != "" {
		add("a.target_type=$%d", filter.ResourceType)
	}
	if filter.ResourceID != "" {
		resourceID, err := internalResourceID(filter.ResourceType, filter.ResourceID)
		if err != nil {
			return "", nil, problem(422, "invalid_filter", "资源 ID 无效")
		}
		add("a.target_id=$%d", resourceID)
	}
	if filter.Action != "" {
		add("a.event_type=$%d", filter.Action)
	}
	if filter.Result != "" {
		add("a.result=$%d", filter.Result)
	}
	return strings.Join(where, " AND "), args, nil
}

func parseRange(fromRaw, toRaw string) (time.Time, time.Time, error) {
	// 1. 显式时间必须成对提供且采用 RFC 3339；缺省查询最近 30 天。
	now := time.Now().UTC()
	if fromRaw == "" && toRaw == "" {
		return now.Add(-30 * 24 * time.Hour), now, nil
	}
	if fromRaw == "" || toRaw == "" {
		return time.Time{}, time.Time{}, problem(422, "invalid_time_range", "from 和 to 必须同时提供")
	}
	from, errFrom := time.Parse(time.RFC3339, fromRaw)
	to, errTo := time.Parse(time.RFC3339, toRaw)
	if errFrom != nil || errTo != nil || !from.Before(to) {
		return time.Time{}, time.Time{}, problem(422, "invalid_time_range", "时间范围必须是有效 RFC 3339 且 from 早于 to")
	}
	return from.UTC(), to.UTC(), nil
}

func validatePage(page, pageSize int) error {
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return problem(422, "invalid_filter", "分页参数无效")
	}
	return nil
}

func validAccessResult(value string) bool {
	return value == "" || value == "allowed" || value == "denied" || value == "challenge"
}

func validAuditResult(value string) bool {
	return value == "" || value == "success" || value == "failure"
}

func makePage[T any](items []T, page, pageSize int, total int64) Page[T] {
	result := Page[T]{Items: items}
	result.Pagination.Page, result.Pagination.PageSize, result.Pagination.Total = page, pageSize, total
	return result
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func safeDetails(raw []byte) map[string]any {
	// 1. 仅返回方案规定的白名单业务元数据，不把任意 JSON 或凭据带出数据库。
	allowed := map[string]bool{
		"groupId": true, "groupIdBefore": true, "groupIdAfter": true, "groupName": true, "groupNameBefore": true, "groupNameAfter": true, "unlinkedCount": true,
		"exportKind": true, "rowCount": true, "sizeBytes": true, "filterKeys": true, "durationMs": true,
		"assetId": true, "businessId": true, "path": true, "fileName": true, "contentType": true, "checksumSha256": true,
		"permissionCode": true, "enabled": true, "revokedByPermission": true, "inviteId": true, "createdByUserId": true, "redeemedByUserId": true, "invitedByUserId": true, "targetUserId": true, "assignedRole": true, "boundEmail": true, "role": true, "registeredViaInvite": true, "templateId": true, "templateVersion": true, "version": true,
		"targetType": true, "code": true, "oldCode": true, "codeChanged": true, "codeStrategy": true, "strategy": true,
		"expiresAt": true, "expiresAtBefore": true, "expiresAtAfter": true,
		"activeFrom": true, "activeFromBefore": true, "activeFromAfter": true,
		"activeUntil": true, "activeUntilBefore": true, "activeUntilAfter": true,
		"title": true, "titleBefore": true, "titleAfter": true, "tags": true, "tagsBefore": true, "tagsAfter": true,
		"allowedIpCidrsBefore": true, "allowedIpCidrsAfter": true, "wrongPasswordStrategyBefore": true, "wrongPasswordStrategyAfter": true,
		"maxVisits": true, "maxVisitsBefore": true, "maxVisitsAfter": true, "requirePassword": true, "requirePasswordBefore": true, "requirePasswordAfter": true,
		"status": true, "action": true, "granted": true, "deleted": true, "previousVisitsUsed": true, "visitsUsed": true,
		"name": true, "nameBefore": true, "nameAfter": true, "descriptionChanged": true,
		"displayName": true, "displayNameBefore": true, "displayNameAfter": true, "bucketName": true, "statusBefore": true, "statusAfter": true,
		"query": true, "roleFilter": true, "page": true, "pageSize": true,
		"failureCode": true, "clientAppId": true, "attemptCount": true,
		"from": true, "to": true,
		"allowPublicRegistrationBefore": true, "allowPublicRegistrationAfter": true, "allowInviteRegistrationBefore": true, "allowInviteRegistrationAfter": true,
		"maxSingleFileBytesBefore": true, "maxSingleFileBytesAfter": true, "storageQuotaBytesBefore": true, "storageQuotaBytesAfter": true,
		"urlLinkQuotaBefore": true, "urlLinkQuotaAfter": true, "minioAssetLinkQuotaBefore": true, "minioAssetLinkQuotaAfter": true,
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return map[string]any{}
	}
	result := make(map[string]any)
	for key, value := range values {
		if allowed[key] {
			result[key] = value
		}
	}
	return result
}

func publicResourceID(kind string, raw sql.NullString) *string {
	if !raw.Valid {
		return nil
	}
	prefix := map[string]string{"short_link": "link", "link_group": "grp", "rule_template": "tpl", "asset": "ast", "user": "usr", "session": "ses", "invite": "inv"}[kind]
	if prefix != "" {
		if id, err := uuid.Parse(raw.String); err == nil {
			value := publicID(prefix, id)
			return &value
		}
	}
	return &raw.String
}

func internalResourceID(kind, value string) (string, error) {
	// 1. 对外接受带类型前缀的 ID，数据库筛选仍使用内部 UUID 字符串。
	prefix := map[string]string{"short_link": "link", "link_group": "grp", "rule_template": "tpl", "asset": "ast", "user": "usr", "session": "ses", "invite": "inv"}[kind]
	if prefix == "" {
		for _, candidate := range []string{"link", "grp", "tpl", "ast", "usr", "ses", "inv"} {
			if strings.HasPrefix(value, candidate+"_") {
				prefix = candidate
				break
			}
		}
	}
	if prefix == "" || !strings.HasPrefix(value, prefix+"_") {
		return value, nil
	}
	id, err := parseID(prefix, value)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func parseID(prefix, value string) (uuid.UUID, error) {
	trimmed, ok := strings.CutPrefix(value, prefix+"_")
	if !ok {
		return uuid.Nil, errors.New("invalid public id")
	}
	return uuid.Parse(trimmed)
}

func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }

func problem(status int, code, message string) error {
	return &Problem{Status: status, Code: code, Message: message}
}
