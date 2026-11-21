package notifications

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
)

type Service struct{ db *sql.DB }

func NewService(db *sql.DB) *Service { return &Service{db: db} }

func requireRecipient(recipient uuid.UUID) error {
	if recipient == uuid.Nil {
		return &Problem{Status: 401, Code: "unauthorized", Message: "请先登录"}
	}
	return nil
}

func (s *Service) List(ctx context.Context, recipient uuid.UUID, unreadOnly bool, page, pageSize int) (Page, error) {
	if err := requireRecipient(recipient); err != nil {
		return Page{}, err
	}
	if page < 1 || pageSize < 1 || pageSize > 100 || page-1 > int(^uint(0)>>1)/pageSize {
		return Page{}, &Problem{Status: 422, Code: "invalid_pagination", Message: "分页参数无效"}
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications
		WHERE recipient_user_id=$1 AND (NOT $2 OR read_at IS NULL)`, recipient, unreadOnly).Scan(&total); err != nil {
		return Page{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT notification_id,workspace_id,link_id,kind,title,summary,created_at,read_at
		FROM notifications WHERE recipient_user_id=$1 AND (NOT $2 OR read_at IS NULL)
		ORDER BY created_at DESC,notification_id DESC LIMIT $3 OFFSET $4`, recipient, unreadOnly, pageSize, (page-1)*pageSize)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	items := make([]Notification, 0)
	for rows.Next() {
		var id, workspaceID uuid.UUID
		var linkID uuid.NullUUID
		var readAt sql.NullTime
		var item Notification
		if err := rows.Scan(&id, &workspaceID, &linkID, &item.Kind, &item.Title, &item.Summary, &item.CreatedAt, &readAt); err != nil {
			return Page{}, err
		}
		item.NotificationID, item.WorkspaceID = publicID("ntf", id), publicID("ws", workspaceID)
		item.CreatedAt = item.CreatedAt.UTC()
		if linkID.Valid {
			value := publicID("link", linkID.UUID)
			item.LinkID = &value
		}
		if readAt.Valid {
			value := readAt.Time.UTC()
			item.ReadAt = &value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	return Page{Items: items, Pagination: Pagination{Page: page, PageSize: pageSize, Total: total}}, nil
}

func (s *Service) UnreadCount(ctx context.Context, recipient uuid.UUID) (int64, error) {
	if err := requireRecipient(recipient); err != nil {
		return 0, err
	}
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE recipient_user_id=$1 AND read_at IS NULL`, recipient).Scan(&count)
	return count, err
}

func (s *Service) MarkRead(ctx context.Context, recipient uuid.UUID, rawID string) error {
	if err := requireRecipient(recipient); err != nil {
		return err
	}
	rawUUID, ok := strings.CutPrefix(rawID, "ntf_")
	id, err := uuid.Parse(rawUUID)
	if !ok || err != nil || id == uuid.Nil {
		return notificationNotFound()
	}
	// Updating an already-read notification preserves its first read timestamp.
	result, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at=COALESCE(read_at,now())
		WHERE notification_id=$1 AND recipient_user_id=$2`, id, recipient)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return notificationNotFound()
	}
	return nil
}

func (s *Service) ReadAll(ctx context.Context, recipient uuid.UUID) (int64, error) {
	if err := requireRecipient(recipient); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at=now()
		WHERE recipient_user_id=$1 AND read_at IS NULL`, recipient)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) GetSettings(ctx context.Context, recipient uuid.UUID) (Settings, error) {
	if err := requireRecipient(recipient); err != nil {
		return Settings{}, err
	}
	var settings Settings
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(ns.expiry_enabled,true),COALESCE(ns.visits_enabled,true)
		FROM users u LEFT JOIN notification_settings ns ON ns.user_id=u.user_id WHERE u.user_id=$1`, recipient).
		Scan(&settings.ExpiryEnabled, &settings.VisitsEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, &Problem{Status: 401, Code: "unauthorized", Message: "请先登录"}
	}
	return settings, err
}

func (s *Service) UpdateSettings(ctx context.Context, recipient uuid.UUID, input UpdateSettingsRequest) (Settings, error) {
	if err := requireRecipient(recipient); err != nil {
		return Settings{}, err
	}
	if input.ExpiryEnabled == nil && input.VisitsEnabled == nil {
		return Settings{}, &Problem{Status: 422, Code: "empty_update", Message: "至少提供一个通知设置"}
	}
	// Serialize settings changes with reminder generation even when no settings row exists yet.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+recipientLockKey+`)`, recipient); err != nil {
		return Settings{}, err
	}
	var settings Settings
	// Merge omitted fields in SQL so partial patches preserve the other setting.
	err = tx.QueryRowContext(ctx, `INSERT INTO notification_settings(user_id,expiry_enabled,visits_enabled,updated_at)
	VALUES($1,COALESCE($2::boolean,true),COALESCE($3::boolean,true),now())
	ON CONFLICT(user_id) DO UPDATE SET
	expiry_enabled=COALESCE($2::boolean,notification_settings.expiry_enabled),
	visits_enabled=COALESCE($3::boolean,notification_settings.visits_enabled),updated_at=now()
	RETURNING expiry_enabled,visits_enabled`, recipient, input.ExpiryEnabled, input.VisitsEnabled).
		Scan(&settings.ExpiryEnabled, &settings.VisitsEnabled)
	if err != nil {
		return Settings{}, err
	}
	return settings, tx.Commit()
}

const recipientLockKey = `hashtextextended('notification-settings:' || $1::uuid::text, 0)`

func notificationNotFound() error {
	return &Problem{Status: 404, Code: "notification_not_found", Message: "通知不存在或无权访问"}
}

func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }
