package links

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// buildListFilter is shared by paginated reads and bounded CSV exports. Values
// are parameters throughout; only fixed, allowlisted SQL fragments are joined.
func buildListFilter(actor Actor, query map[string]string) (string, []any, error) {
	where := []string{}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if actor.Role != "admin" {
		add("sl.workspace_id=$%d", actor.WorkspaceID)
	} else if workspace := query["workspaceId"]; workspace != "" {
		id, err := parseID("ws", workspace)
		if err != nil {
			return "", nil, problem(422, "invalid_workspace_id", "工作空间参数无效")
		}
		add("sl.workspace_id=$%d", id)
	}
	if actor.BusinessID != "" {
		add("sl.business_id=$%d", actor.BusinessID)
	}
	for _, pair := range [][2]string{{"status", "sl.status"}, {"targetType", "sl.target_type"}, {"code", "sc.code"}} {
		key, column := pair[0], pair[1]
		if value := query[key]; value != "" {
			if (key == "status" && value != "enabled" && value != "disabled") || (key == "targetType" && value != "url" && value != "minio_asset") {
				return "", nil, problem(422, "invalid_filter", "筛选参数无效")
			}
			add(column+"=$%d", strings.ToLower(value))
		}
	}
	if raw, present := query["maxVisits"]; present {
		maxVisits, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || maxVisits < -1 {
			return "", nil, problem(422, "invalid_filter", "maxVisits 只能为 -1、0 或正整数")
		}
		add("sl.max_visits=$%d", maxVisits)
	}
	if keyword := strings.TrimSpace(query["keyword"]); keyword != "" {
		if len([]rune(keyword)) > 160 {
			return "", nil, problem(422, "invalid_filter", "关键词不能超过 160 个字符")
		}
		args = append(args, keyword)
		index := len(args)
		where = append(where, fmt.Sprintf(`(
			strpos(lower(sl.title),lower($%d))>0 OR
			strpos(lower(COALESCE(sl.target_url,'')),lower($%d))>0 OR
			strpos(lower(COALESCE(sc.code,'')),lower($%d))>0 OR
			EXISTS (SELECT 1 FROM jsonb_array_elements_text(sl.tags) AS labels(value) WHERE strpos(lower(labels.value),lower($%d))>0)
		)`, index, index, index, index))
	}
	if rawAssetID := strings.TrimSpace(query["assetId"]); rawAssetID != "" {
		assetID, err := parseID("ast", rawAssetID)
		if err != nil {
			return "", nil, problem(422, "invalid_asset_id", "素材 ID 格式无效")
		}
		add("sl.asset_id=$%d", assetID)
	}
	groupID, hasGroup := query["groupId"]
	ungrouped, hasUngrouped := query["ungrouped"]
	if hasUngrouped && ungrouped != "true" && ungrouped != "false" {
		return "", nil, problem(422, "invalid_filter", "ungrouped 必须是 true 或 false")
	}
	if hasGroup && ungrouped == "true" {
		return "", nil, problem(422, "invalid_filter", "groupId 与 ungrouped=true 不能同时使用")
	}
	if hasGroup {
		id, err := parseID("grp", groupID)
		if err != nil {
			return "", nil, problem(422, "invalid_group_id", "分组 ID 格式无效")
		}
		add("sl.group_id=$%d", id)
	} else if ungrouped == "true" {
		where = append(where, "sl.group_id IS NULL")
	}
	if availability := query["availability"]; availability != "" {
		if availability != "available" && availability != "expired" && availability != "window_ended" && availability != "not_started" && availability != "visit_limit_reached" && availability != "disabled" {
			return "", nil, problem(422, "invalid_filter", "可用状态筛选参数无效")
		}
		add(availabilityExpression+"=$%d", availability)
	}
	if from := query["createdFrom"]; from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return "", nil, problem(422, "invalid_filter", "创建时间参数无效")
		}
		add("sl.created_at >= $%d", t)
	}
	if to := query["createdTo"]; to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return "", nil, problem(422, "invalid_filter", "创建时间参数无效")
		}
		add("sl.created_at <= $%d", t)
	}
	if len(where) == 0 {
		where = append(where, "TRUE")
	}
	return strings.Join(where, " AND "), args, nil
}

const availabilityExpression = "CASE WHEN sl.status='disabled' THEN 'disabled' WHEN sl.expires_at IS NOT NULL AND sl.expires_at<=now() THEN 'expired' WHEN sl.active_until IS NOT NULL AND sl.active_until<=now() THEN 'window_ended' WHEN sl.active_from IS NOT NULL AND sl.active_from>now() THEN 'not_started' WHEN sl.max_visits>=0 AND sl.visits_used>=sl.max_visits THEN 'visit_limit_reached' ELSE 'available' END"
