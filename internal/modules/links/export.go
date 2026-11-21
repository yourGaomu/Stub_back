package links

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// ExportCSV performs one bounded query and streams safe rows to the callback.
// The HTTP caller writes a temporary file and discards it on any error, including
// the 10001st row; it owns byte limits, formula escaping and auditing.
func (s *Service) ExportCSV(ctx context.Context, actor Actor, query map[string]string, writeRow func([]string) error) (int, error) {
	filter, args, err := buildListFilter(actor, query)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sl.link_id,sc.code,sl.title,sl.tags::text,sl.target_type,sl.asset_id,a.size_bytes,
		sl.group_id,g.name,sl.status,`+availabilityExpression+`,sl.visits_used,sl.max_visits,sl.expires_at,sl.created_at
		FROM short_links sl LEFT JOIN short_link_codes sc ON sc.link_id=sl.link_id AND sc.status='active'
		LEFT JOIN assets a ON a.asset_id=sl.asset_id
		LEFT JOIN link_groups g ON g.group_id=sl.group_id AND g.workspace_id=sl.workspace_id
		WHERE `+filter+` ORDER BY sl.created_at DESC,sl.link_id DESC LIMIT 10001`, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if err := writeRow([]string{"linkId", "code", "url", "title", "tags", "targetType", "assetId", "assetSizeBytes", "groupId", "groupName", "status", "availability", "visitsUsed", "maxVisits", "expiresAt", "createdAt"}); err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		if count == 10000 {
			return 0, problem(422, "export_limit_exceeded", "最多导出 10000 条短链，请缩小筛选范围")
		}
		var id uuid.UUID
		var code, assetID, groupID, groupName sql.NullString
		var title, targetType, status, availability string
		var tagsJSON []byte
		var size sql.NullInt64
		var visitsUsed, maxVisits int64
		var expires sql.NullTime
		var created time.Time
		if err := rows.Scan(&id, &code, &title, &tagsJSON, &targetType, &assetID, &size,
			&groupID, &groupName, &status, &availability, &visitsUsed, &maxVisits, &expires, &created); err != nil {
			return 0, err
		}
		tags := []string{}
		if err := json.Unmarshal(tagsJSON, &tags); err != nil {
			return 0, fmt.Errorf("读取短链标签失败: %w", err)
		}
		tagsJSON, err = json.Marshal(tags)
		if err != nil {
			return 0, err
		}
		asset, assetSize, expiration, group, name := "", "", "", "", ""
		if assetID.Valid {
			parsed, err := uuid.Parse(assetID.String)
			if err != nil {
				return 0, err
			}
			asset = publicID("ast", parsed)
		}
		if size.Valid {
			assetSize = strconv.FormatInt(size.Int64, 10)
		}
		if expires.Valid {
			expiration = expires.Time.UTC().Format(time.RFC3339)
		}
		if publicID, publicName := publicGroupFields(groupID, groupName); publicID != nil {
			group = *publicID
			if publicName != nil {
				name = *publicName
			}
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := writeRow([]string{publicID("link", id), code.String, s.linkURL(code.String), title, string(tagsJSON), targetType,
			asset, assetSize, group, name, status, availability, strconv.FormatInt(visitsUsed, 10), strconv.FormatInt(maxVisits, 10), expiration, created.UTC().Format(time.RFC3339)}); err != nil {
			return 0, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	return count, nil
}
