package assets

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourGaomu/short_links/back/internal/platform/objectstore"
)

const recoveryPeriod = 7 * 24 * time.Hour

type Service struct {
	db    *sql.DB
	store *objectstore.Store
}

func NewService(db *sql.DB, store *objectstore.Store) *Service { return &Service{db: db, store: store} }

type assetRecord struct {
	ID, WorkspaceID, OwnerID                                         uuid.UUID
	BusinessID, Bucket, ObjectKey, Path, Name, ContentType, Checksum string
	Size                                                             int64
	DeletedAt, PurgedAt                                              sql.NullTime
	CreatedAt                                                        time.Time
}

func (s *Service) Upload(ctx context.Context, actor Actor, input UploadInput, meta Meta) (AssetDTO, error) {
	// 1. 验证上传 DTO 和逻辑目录；MinIO object key 始终由服务端生成。
	if input.File == nil || strings.TrimSpace(input.BusinessID) == "" {
		return AssetDTO{}, problem(422, "invalid_upload", "businessId、path 和 file 均为必填项")
	}
	if actor.BusinessID != "" && strings.TrimSpace(input.BusinessID) != actor.BusinessID {
		return AssetDTO{}, problem(403, "api_key_business_denied", "API Key 未授权该业务")
	}
	logicalPath, err := validateLogicalPath(input.Path)
	if err != nil {
		return AssetDTO{}, err
	}
	fileName := filepath.Base(strings.TrimSpace(input.File.Filename))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) || len([]rune(fileName)) > 255 || containsControl(fileName) {
		return AssetDTO{}, problem(422, "invalid_file_name", "文件名无效")
	}
	// 2. 用服务端解析后的 multipart 大小作快速预检，再实际读取、哈希并写入临时文件。
	var maxSingle int64
	if err := s.db.QueryRowContext(ctx, `SELECT max_single_file_bytes FROM users WHERE user_id=$1`, actor.UserID).Scan(&maxSingle); errors.Is(err, sql.ErrNoRows) {
		return AssetDTO{}, problem(401, "unauthorized", "请先登录")
	} else if err != nil {
		return AssetDTO{}, err
	}
	if input.File.Size > maxSingle {
		return AssetDTO{}, problem(413, "upload_quota_exceeded", "文件超过单次上传大小限制")
	}
	file, err := input.File.Open()
	if err != nil {
		return AssetDTO{}, problem(422, "invalid_upload", "无法读取上传文件")
	}
	defer file.Close()
	temp, err := os.CreateTemp("", "short-links-upload-*")
	if err != nil {
		return AssetDTO{}, fmt.Errorf("创建上传临时文件失败: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	hash := sha256.New()
	var source io.Reader = file
	if maxSingle < math.MaxInt64 {
		source = io.LimitReader(file, maxSingle+1)
	}
	written, copyErr := io.Copy(io.MultiWriter(temp, hash), source)
	if copyErr != nil {
		temp.Close()
		return AssetDTO{}, fmt.Errorf("读取上传文件失败: %w", copyErr)
	}
	if written > maxSingle {
		temp.Close()
		return AssetDTO{}, problem(413, "upload_quota_exceeded", "文件超过单次上传大小限制")
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	header := make([]byte, 512)
	n, _ := io.ReadFull(temp, header)
	contentType := http.DetectContentType(header[:n])
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = mediaType
	}
	// 3. APK 的文件结构基于 ZIP，按扩展名修正 MIME，确保下载端仍识别为安装包。
	if strings.EqualFold(filepath.Ext(fileName), ".apk") {
		contentType = "application/vnd.android.package-archive"
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	if s.store == nil {
		temp.Close()
		return AssetDTO{}, problem(503, "storage_unavailable", "MinIO 存储尚未配置")
	}
	// 3. 锁定用户额度行，原子校验业务授权和总容量后预留额度。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	defer tx.Rollback()
	var role string
	var storageUsed, storageReserved, storageQuota int64
	var lockedMax int64
	if err := tx.QueryRowContext(ctx, `SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users WHERE user_id=$1 FOR UPDATE`, actor.UserID).Scan(&role, &lockedMax, &storageUsed, &storageReserved, &storageQuota); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	if role != "admin" {
		actor.Role = role
	}
	if written > lockedMax {
		temp.Close()
		return AssetDTO{}, problem(413, "upload_quota_exceeded", "文件超过单次上传大小限制")
	}
	if storageUsed > storageQuota || storageReserved > storageQuota-storageUsed || written > storageQuota-storageUsed-storageReserved {
		temp.Close()
		return AssetDTO{}, problem(413, "upload_quota_exceeded", "用户素材总存储额度不足")
	}
	var bucket string
	err = tx.QueryRowContext(ctx, `SELECT bucket_name FROM business_integrations WHERE business_id=$1 AND status='active'`, input.BusinessID).Scan(&bucket)
	if errors.Is(err, sql.ErrNoRows) {
		temp.Close()
		return AssetDTO{}, problem(404, "business_not_found", "业务不存在、已停用或无权访问")
	}
	if err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	if actor.Role != "admin" {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_business_access WHERE user_id=$1 AND business_id=$2 AND revoked_at IS NULL)`, actor.UserID, input.BusinessID).Scan(&allowed); err != nil {
			temp.Close()
			return AssetDTO{}, err
		}
		if !allowed {
			temp.Close()
			return AssetDTO{}, problem(404, "business_not_found", "业务不存在、已停用或无权访问")
		}
	}
	if actor.APIKeyID != nil {
		result, err := tx.ExecContext(ctx, `UPDATE api_keys SET uploaded_bytes_used=uploaded_bytes_used+$2,updated_at=updated_at WHERE api_key_id=$1 AND status='active' AND (expires_at IS NULL OR expires_at>now()) AND (max_upload_bytes IS NULL OR uploaded_bytes_used <= max_upload_bytes-$2)`, *actor.APIKeyID, written)
		if err != nil {
			temp.Close()
			return AssetDTO{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			temp.Close()
			return AssetDTO{}, problem(429, "api_key_upload_quota_exceeded", "API Key 累计上传额度已用尽")
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`, actor.UserID, written); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	// 4. 将对象先写入私有业务桶，再提交数据库；数据库失败时通过补偿删除对象。
	assetID := uuid.New()
	objectKey := fmt.Sprintf("workspaces/%s/assets/%s", actor.WorkspaceID, assetID)
	now := time.Now().UTC()
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	if err := s.store.Put(ctx, bucket, objectKey, contentType, temp, written); err != nil {
		temp.Close()
		return AssetDTO{}, problem(503, "storage_unavailable", "素材存储暂不可用")
	}
	committed := false
	defer func() {
		if !committed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.store.Remove(cleanupCtx, bucket, objectKey)
		}
	}()
	if _, err := tx.ExecContext(ctx, `INSERT INTO assets(asset_id,workspace_id,owner_user_id,business_id,logical_path,bucket,object_key,original_name,content_type,size_bytes,checksum_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, assetID, actor.WorkspaceID, actor.UserID, input.BusinessID, logicalPath, bucket, objectKey, fileName, contentType, written, checksum, now); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	if err := audit(ctx, tx, actor.UserID, actor.WorkspaceID, "asset.upload", assetID.String(), meta, MetaUpload{AssetID: publicID("ast", assetID), BusinessID: input.BusinessID, Path: logicalPath, FileName: fileName, ContentType: contentType, SizeBytes: written, ChecksumSHA256: checksum}); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		temp.Close()
		return AssetDTO{}, err
	}
	committed = true
	temp.Close()
	return AssetDTO{AssetID: publicID("ast", assetID), BusinessID: input.BusinessID, Path: logicalPath, FileName: fileName, ContentType: contentType, SizeBytes: written, ChecksumSHA256: checksum, CreatedAt: now}, nil
}

func (s *Service) List(ctx context.Context, actor Actor, page, pageSize int, businessID, workspaceID, ownerUserID, keyword string, includeDeleted bool) (Page, error) {
	// 1. 普通用户始终限定为自己的空间和已获授权业务，管理员可显式跨空间筛选。
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return Page{}, problem(422, "invalid_pagination", "分页参数无效")
	}
	if actor.Role != "admin" && (workspaceID != "" || ownerUserID != "") {
		return Page{}, problem(404, "asset_not_found", "素材不存在或無權訪問")
	}
	if businessID != "" && actor.Role != "admin" {
		var allowed bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM business_integrations bi JOIN user_business_access uba ON uba.business_id=bi.business_id WHERE bi.business_id=$1 AND bi.status='active' AND uba.user_id=$2 AND uba.revoked_at IS NULL)`, businessID, actor.UserID).Scan(&allowed); err != nil {
			return Page{}, err
		}
		if !allowed {
			return Page{}, problem(404, "business_not_found", "业务不存在或无权访问")
		}
	}
	where := []string{}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if actor.Role != "admin" {
		add("a.workspace_id=$%d", actor.WorkspaceID)
		where = append(where, "EXISTS(SELECT 1 FROM business_integrations bi WHERE bi.business_id=a.business_id AND bi.status='active') AND EXISTS(SELECT 1 FROM user_business_access uba WHERE uba.user_id=$USER AND uba.business_id=a.business_id AND uba.revoked_at IS NULL)")
		where[len(where)-1] = strings.ReplaceAll(where[len(where)-1], "$USER", fmt.Sprintf("$%d", len(args)+1))
		args = append(args, actor.UserID)
	} else {
		if workspaceID != "" {
			id, err := parseID("ws", workspaceID)
			if err != nil {
				return Page{}, problem(422, "invalid_workspace_id", "工作空间参数无效")
			}
			add("a.workspace_id=$%d", id)
		}
		if ownerUserID != "" {
			id, err := parseID("usr", ownerUserID)
			if err != nil {
				return Page{}, problem(422, "invalid_user_id", "用户参数无效")
			}
			add("a.owner_user_id=$%d", id)
		}
	}
	if businessID != "" {
		add("a.business_id=$%d", businessID)
	}
	if keyword != "" {
		args = append(args, "%"+strings.TrimSpace(keyword)+"%")
		where = append(where, fmt.Sprintf("(a.original_name ILIKE $%d OR a.logical_path ILIKE $%d)", len(args), len(args)))
	}
	if !includeDeleted {
		where = append(where, "a.deleted_at IS NULL AND a.purged_at IS NULL")
	}
	filter := "TRUE"
	if len(where) > 0 {
		filter = strings.Join(where, " AND ")
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM assets a WHERE `+filter, args...).Scan(&total); err != nil {
		return Page{}, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	// 1.1 分片素材不计算整文件 SHA-256，数据库中的 NULL 按接口约定返回空字符串。
	rows, err := s.db.QueryContext(ctx, `SELECT a.asset_id,a.business_id,a.logical_path,a.original_name,a.content_type,a.size_bytes,COALESCE(a.checksum_sha256,''),a.deleted_at,a.created_at FROM assets a WHERE `+filter+fmt.Sprintf(" ORDER BY a.created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	items := make([]ListItem, 0)
	for rows.Next() {
		var id uuid.UUID
		var item ListItem
		var deleted sql.NullTime
		if err := rows.Scan(&id, &item.BusinessID, &item.Path, &item.FileName, &item.ContentType, &item.SizeBytes, &item.ChecksumSHA256, &deleted, &item.CreatedAt); err != nil {
			return Page{}, err
		}
		item.AssetID = publicID("ast", id)
		item.CreatedAt = item.CreatedAt.UTC()
		if deleted.Valid {
			value := deleted.Time.UTC()
			item.DeletedAt = &value
		}
		items = append(items, item)
	}
	result := Page{Items: items}
	result.Pagination.Page = page
	result.Pagination.PageSize = pageSize
	result.Pagination.Total = total
	return result, rows.Err()
}

func (s *Service) Get(ctx context.Context, actor Actor, rawID string) (AssetDTO, error) {
	// 1. 详情查询不返回 MinIO 桶名、object key 或对象访问地址。
	id, err := parseID("ast", rawID)
	if err != nil {
		return AssetDTO{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	record, err := s.load(ctx, s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return AssetDTO{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if err != nil {
		return AssetDTO{}, err
	}
	allowed, err := s.inScope(ctx, actor, record)
	if err != nil {
		return AssetDTO{}, err
	}
	if !allowed {
		return AssetDTO{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	return toDTO(record), nil
}

func (s *Service) Content(ctx context.Context, actor Actor, rawID string, meta Meta) (string, error) {
	// 1. 仅为当前可用素材签发短期公网 URL；调用本接口不消耗短链访问次数。
	id, err := parseID("ast", rawID)
	if err != nil {
		return "", problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	record, err := s.load(ctx, s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if err != nil {
		return "", err
	}
	allowed, err := s.inScope(ctx, actor, record)
	if err != nil {
		return "", err
	}
	if !allowed || record.DeletedAt.Valid || record.PurgedAt.Valid {
		return "", problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if s.store == nil {
		return "", problem(503, "storage_unavailable", "MinIO 存储暂不可用")
	}
	value, err := s.store.PresignedGet(ctx, record.Bucket, record.ObjectKey, 15*time.Minute, record.ContentType, record.Name)
	if err != nil {
		return "", problem(503, "storage_unavailable", "素材存储暂不可用")
	}
	// 2. 记录素材访问事实，但绝不把短期预签名 URL 写入审计详情。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := audit(ctx, tx, actor.UserID, record.WorkspaceID, "asset.content", id.String(), meta, map[string]any{
		"assetId": publicID("ast", id), "businessId": record.BusinessID, "sizeBytes": record.Size,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return value, nil
}

func (s *Service) Delete(ctx context.Context, actor Actor, rawID string, meta Meta) (DeleteResponse, error) {
	// 1. 删除只设置软删除和七天恢复记录；额度在对象物理删除前不释放。
	id, err := parseID("ast", rawID)
	if err != nil {
		return DeleteResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteResponse{}, err
	}
	defer tx.Rollback()
	record, err := s.load(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if err != nil {
		return DeleteResponse{}, err
	}
	allowed, err := s.inScope(ctx, actor, record)
	if err != nil {
		return DeleteResponse{}, err
	}
	if !allowed {
		return DeleteResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if record.PurgedAt.Valid {
		return DeleteResponse{}, problem(409, "asset_already_purged", "素材已经物理删除")
	}
	if record.DeletedAt.Valid {
		var deleted, purge time.Time
		var status string
		err := tx.QueryRowContext(ctx, `SELECT deleted_at,purge_after,status FROM asset_deletion_records WHERE asset_id=$1 AND status='pending'`, id).Scan(&deleted, &purge, &status)
		if err != nil {
			return DeleteResponse{}, err
		}
		if err := tx.Commit(); err != nil {
			return DeleteResponse{}, err
		}
		return DeleteResponse{AssetID: publicID("ast", id), DeletedAt: deleted.UTC(), PurgeAfter: purge.UTC(), Status: status}, nil
	}
	now := time.Now().UTC()
	purgeAt := now.Add(recoveryPeriod)
	deletionID := uuid.New()
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET deleted_at=$2 WHERE asset_id=$1`, id, now); err != nil {
		return DeleteResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_deletion_records(deletion_id,asset_id,workspace_id,deleted_by,deleted_at,purge_after,status) VALUES($1,$2,$3,$4,$5,$6,'pending')`, deletionID, id, record.WorkspaceID, actor.UserID, now, purgeAt); err != nil {
		return DeleteResponse{}, err
	}
	if err := audit(ctx, tx, actor.UserID, record.WorkspaceID, "asset.delete", id.String(), meta, map[string]any{"assetId": publicID("ast", id), "businessId": record.BusinessID}); err != nil {
		return DeleteResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteResponse{}, err
	}
	return DeleteResponse{AssetID: publicID("ast", id), DeletedAt: now, PurgeAfter: purgeAt, Status: "pending"}, nil
}

func (s *Service) Restore(ctx context.Context, actor Actor, rawID string, meta Meta) (RestoreResponse, error) {
	// 1. 先锁素材与清理记录，再确认对象存在，避免与后台物理清理互相覆盖。
	id, err := parseID("ast", rawID)
	if err != nil {
		return RestoreResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RestoreResponse{}, err
	}
	defer tx.Rollback()
	record, err := s.load(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return RestoreResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if err != nil {
		return RestoreResponse{}, err
	}
	allowed, err := s.inScope(ctx, actor, record)
	if err != nil {
		return RestoreResponse{}, err
	}
	if !allowed {
		return RestoreResponse{}, problem(404, "asset_not_found", "素材不存在或无权访问")
	}
	if !record.DeletedAt.Valid || record.PurgedAt.Valid {
		return RestoreResponse{}, problem(409, "asset_restore_expired", "素材不在可恢复状态")
	}
	var deletionID uuid.UUID
	var purgeAfter time.Time
	err = tx.QueryRowContext(ctx, `SELECT deletion_id,purge_after FROM asset_deletion_records WHERE asset_id=$1 AND status='pending' FOR UPDATE`, id).Scan(&deletionID, &purgeAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return RestoreResponse{}, problem(409, "asset_restore_expired", "素材不在可恢复状态")
	}
	if err != nil {
		return RestoreResponse{}, err
	}
	if !purgeAfter.After(time.Now()) {
		return RestoreResponse{}, problem(409, "asset_restore_expired", "素材恢复期限已过")
	}
	if s.store == nil {
		return RestoreResponse{}, problem(503, "storage_unavailable", "MinIO 存储暂不可用")
	}
	exists, err := s.store.ObjectExists(ctx, record.Bucket, record.ObjectKey)
	if err != nil {
		return RestoreResponse{}, problem(503, "storage_unavailable", "无法确认素材对象状态")
	}
	if !exists {
		return RestoreResponse{}, problem(409, "asset_restore_expired", "素材对象已被物理删除")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET deleted_at=NULL WHERE asset_id=$1`, id); err != nil {
		return RestoreResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_deletion_records SET status='restored' WHERE deletion_id=$1`, deletionID); err != nil {
		return RestoreResponse{}, err
	}
	if err := audit(ctx, tx, actor.UserID, record.WorkspaceID, "asset.restore", id.String(), meta, map[string]any{"assetId": publicID("ast", id), "businessId": record.BusinessID}); err != nil {
		return RestoreResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return RestoreResponse{}, err
	}
	return RestoreResponse{AssetID: publicID("ast", id), RestoredAt: now, Status: "active"}, nil
}

func (s *Service) PurgeExpired(ctx context.Context, limit int) error {
	// 1. 以 SKIP LOCKED 领取到期对象；只有 MinIO 删除成功后才减少用户已用额度。
	if s.store == nil {
		return nil
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT d.deletion_id,d.asset_id,a.owner_user_id,a.size_bytes,a.bucket,a.object_key,a.workspace_id,a.business_id FROM asset_deletion_records d JOIN assets a ON a.asset_id=d.asset_id WHERE d.status='pending' AND d.purge_after<=now() ORDER BY d.purge_after LIMIT $1 FOR UPDATE OF a,d SKIP LOCKED`, limit)
	if err != nil {
		return err
	}
	type due struct {
		deletion, asset, owner  uuid.UUID
		workspace               uuid.UUID
		size                    int64
		bucket, key, businessID string
	}
	items := make([]due, 0)
	for rows.Next() {
		var item due
		if err := rows.Scan(&item.deletion, &item.asset, &item.owner, &item.size, &item.bucket, &item.key, &item.workspace, &item.businessID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range items {
		if err := s.store.Remove(ctx, item.bucket, item.key); err != nil {
			var attempts int
			updateErr := tx.QueryRowContext(ctx, `UPDATE asset_deletion_records SET attempt_count=attempt_count+1,last_error=$2 WHERE deletion_id=$1 RETURNING attempt_count`, item.deletion, truncateError(err.Error())).Scan(&attempts)
			if updateErr != nil {
				return updateErr
			}
			if err := auditSystem(ctx, tx, item.workspace, "asset.purge", item.asset.String(), "failure", map[string]any{
				"assetId": publicID("ast", item.asset), "businessId": item.businessID, "failureCode": "storage_unavailable", "attemptCount": attempts,
			}); err != nil {
				return err
			}
			continue
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE assets SET purged_at=$2 WHERE asset_id=$1 AND purged_at IS NULL`, item.asset, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_used_bytes=GREATEST(0,storage_used_bytes-$2),updated_at=$3 WHERE user_id=$1`, item.owner, item.size, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE asset_deletion_records SET status='purged',purged_at=$2,last_error=NULL WHERE deletion_id=$1 AND status='pending'`, item.deletion, now); err != nil {
			return err
		}
		if err := auditSystem(ctx, tx, item.workspace, "asset.purge", item.asset.String(), "success", map[string]any{"assetId": publicID("ast", item.asset), "businessId": item.businessID}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) load(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID) (assetRecord, error) {
	var r assetRecord
	err := q.QueryRowContext(ctx, `SELECT asset_id,workspace_id,owner_user_id,business_id,bucket,object_key,logical_path,original_name,content_type,size_bytes,COALESCE(checksum_sha256,''),deleted_at,purged_at,created_at FROM assets WHERE asset_id=$1 FOR UPDATE`, id).Scan(&r.ID, &r.WorkspaceID, &r.OwnerID, &r.BusinessID, &r.Bucket, &r.ObjectKey, &r.Path, &r.Name, &r.ContentType, &r.Size, &r.Checksum, &r.DeletedAt, &r.PurgedAt, &r.CreatedAt)
	return r, err
}

func (s *Service) inScope(ctx context.Context, actor Actor, r assetRecord) (bool, error) {
	if actor.BusinessID != "" && actor.BusinessID != r.BusinessID {
		return false, nil
	}
	if actor.Role == "admin" {
		return true, nil
	}
	if actor.WorkspaceID != r.WorkspaceID {
		return false, nil
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM business_integrations bi JOIN user_business_access uba ON uba.business_id=bi.business_id WHERE bi.business_id=$1 AND bi.status='active' AND uba.user_id=$2 AND uba.revoked_at IS NULL)`, r.BusinessID, actor.UserID).Scan(&allowed)
	return allowed, err
}

func toDTO(r assetRecord) AssetDTO {
	dto := AssetDTO{AssetID: publicID("ast", r.ID), BusinessID: r.BusinessID, Path: r.Path, FileName: r.Name, ContentType: r.ContentType, SizeBytes: r.Size, ChecksumSHA256: r.Checksum, CreatedAt: r.CreatedAt.UTC()}
	if r.DeletedAt.Valid {
		value := r.DeletedAt.Time.UTC()
		dto.DeletedAt = &value
	}
	return dto
}

func validateLogicalPath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) || containsControl(value) {
		return "", problem(422, "invalid_path", "逻辑路径无效")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." {
			return "", problem(422, "invalid_path", "逻辑路径不能包含 . 或 .. 路径段")
		}
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", problem(422, "invalid_path", "逻辑路径无效")
	}
	return clean, nil
}
func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
func truncateError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}
func parseID(prefix, value string) (uuid.UUID, error) {
	trimmed, ok := strings.CutPrefix(value, prefix+"_")
	if !ok {
		return uuid.Nil, fmt.Errorf("invalid %s id", prefix)
	}
	return uuid.Parse(trimmed)
}
func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }
func problem(status int, code, message string) error {
	return &Problem{Status: status, Code: code, Message: message}
}
func audit(ctx context.Context, tx *sql.Tx, actor, workspace uuid.UUID, event, target string, meta Meta, details any) error {
	raw := "{}"
	if details != nil {
		payload := details
		if meta.AuditDetails != nil {
			encoded, err := json.Marshal(details)
			if err != nil {
				return err
			}
			values := map[string]any{}
			if err := json.Unmarshal(encoded, &values); err != nil {
				return err
			}
			for key, value := range meta.AuditDetails {
				values[key] = value
			}
			payload = values
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = string(encoded)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,workspace_id,event_type,target_type,target_id,source_ip,user_agent,result,details,request_id) VALUES($1,$2,$3,'asset',$4,NULLIF($5,'')::inet,$6,'success',$7::jsonb,NULLIF($8,''))`, actor, workspace, event, target, meta.SourceIP, meta.UserAgent, raw, meta.RequestID)
	return err
}

func auditSystem(ctx context.Context, tx *sql.Tx, workspace uuid.UUID, event, target, result string, details any) error {
	// 1. 后台物理清理由服务系统执行，审计记录不伪装成素材所有者的操作。
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs(workspace_id,event_type,target_type,target_id,result,details) VALUES($1,$2,'asset',$3,$4,$5::jsonb)`, workspace, event, target, result, string(raw))
	return err
}
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
