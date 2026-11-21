package notifications

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PostgreSQL LEAST ignores a NULL bound when the other bound is set. Derive
// the same key for the shortlist and the locked recheck, at database precision.
const eligibleReminders = `
	FROM short_links sl
	LEFT JOIN notification_settings ns ON ns.user_id=sl.created_by
	CROSS JOIN LATERAL (SELECT LEAST(sl.expires_at,sl.active_until) AS deadline) d
	CROSS JOIN LATERAL (
		SELECT 'expiry_24h' AS kind,'expiry:'||sl.link_id::text||':'||
			rtrim(rtrim(to_char(d.deadline AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US'),'0'),'.')||'Z' AS dedupe_key
		WHERE COALESCE(ns.expiry_enabled,true) AND d.deadline>$1 AND d.deadline<=$2
		UNION ALL
		SELECT 'visits_80' AS kind,'visits:'||sl.link_id::text||':'||sl.visits_cycle::text AS dedupe_key
		WHERE COALESCE(ns.visits_enabled,true) AND sl.max_visits>0
			AND sl.visits_used>=sl.max_visits-sl.max_visits/5
	) reminder
	WHERE sl.status='enabled' AND NOT EXISTS (
		SELECT 1 FROM notifications n WHERE n.dedupe_key=reminder.dedupe_key
	)`

const runOnceQuery = `SELECT sl.link_id,sl.created_by,reminder.kind` + eligibleReminders + `
	ORDER BY CASE WHEN reminder.kind='expiry_24h' THEN d.deadline END NULLS LAST,sl.link_id,reminder.kind LIMIT $3`

const recheckReminderQuery = `SELECT sl.workspace_id,d.deadline,sl.max_visits,sl.visits_used,reminder.dedupe_key,
	(SELECT sc.code FROM short_link_codes sc WHERE sc.link_id=sl.link_id AND sc.status='active')` + eligibleReminders + `
	AND sl.link_id=$3 AND sl.created_by=$4 AND reminder.kind=$5`

const insertNotificationQuery = `INSERT INTO notifications(notification_id,recipient_user_id,workspace_id,link_id,kind,title,summary,dedupe_key,created_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(dedupe_key) DO NOTHING`

type workerCandidate struct {
	linkID      uuid.UUID
	recipientID uuid.UUID
	kind        string
}

const maxRemindersPerScan = 100

// RunOnce rechecks each candidate in its own transaction. Allow extra attempts
// so a skipped lock or stale candidate does not consume a delivery slot.
func (s *Service) RunOnce(ctx context.Context, limit int) error {
	return s.runOnce(ctx, time.Now, limit)
}

func (s *Service) runOnce(ctx context.Context, clock func() time.Time, limit int) error {
	if limit < 1 {
		return nil
	}
	limit = min(limit, maxRemindersPerScan)
	candidates, err := s.shortlist(ctx, clock().UTC(), 2*limit)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		created, err := s.createReminder(ctx, clock, candidate)
		if err != nil {
			return err
		}
		if created {
			limit--
			if limit == 0 {
				break
			}
		}
	}
	return nil
}

func (s *Service) shortlist(ctx context.Context, now time.Time, limit int) ([]workerCandidate, error) {
	rows, err := s.db.QueryContext(ctx, runOnceQuery, now, now.Add(24*time.Hour), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []workerCandidate
	for rows.Next() {
		var candidate workerCandidate
		if err := rows.Scan(&candidate.linkID, &candidate.recipientID, &candidate.kind); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Service) createReminder(ctx context.Context, clock func() time.Time, candidate workerCandidate) (bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Lock the recipient first. UpdateSettings uses this key even when the
	// default preference has no row yet. Busy recipients are retried next scan.
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(`+recipientLockKey+`)`, candidate.recipientID).Scan(&locked); err != nil || !locked {
		return false, err
	}

	var linkID uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT link_id FROM short_links WHERE link_id=$1 AND created_by=$2 FOR SHARE SKIP LOCKED`,
		candidate.linkID, candidate.recipientID).Scan(&linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// A separate statement takes a fresh Read Committed snapshot after both
	// locks, including the current short code, cutoff, cycle, and preferences.
	var workspaceID uuid.UUID
	var deadline sql.NullTime
	var maxVisits, visitsUsed int64
	var key string
	var code sql.NullString
	now := clock().UTC()
	err = tx.QueryRowContext(ctx, recheckReminderQuery, now, now.Add(24*time.Hour), linkID, candidate.recipientID, candidate.kind).
		Scan(&workspaceID, &deadline, &maxVisits, &visitsUsed, &key, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	label := safeLinkLabel(code.String)
	var title, summary string
	switch candidate.kind {
	case KindExpiry24h:
		title = "短链即将到期"
		summary = fmt.Sprintf("%s 将在 24 小时内到达实际截止时间（%s）。", label, deadline.Time.UTC().Format(time.RFC3339))
	case KindVisits80:
		title = "短链访问次数提醒"
		summary = fmt.Sprintf("%s 的访问次数已使用 %d/%d，达到 80%% 提醒阈值。", label, visitsUsed, maxVisits)
	default:
		return false, fmt.Errorf("unexpected notification kind %q", candidate.kind)
	}
	result, err := tx.ExecContext(ctx, insertNotificationQuery, uuid.New(), candidate.recipientID,
		workspaceID, linkID, candidate.kind, title, summary, key, now)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return inserted > 0, nil
}

func safeLinkLabel(code string) string {
	if len(code) < 4 || len(code) > 64 {
		return "该短链"
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return "该短链"
		}
	}
	return "短链 /r/" + code
}
