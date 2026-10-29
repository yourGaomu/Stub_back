package assets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const (
	multipartPartSize    int64 = 32 * 1024 * 1024
	multipartSessionTTL        = 24 * time.Hour
	multipartActiveLimit       = 3
)

type uploadSessionRecord struct {
	ID, WorkspaceID, OwnerID, APIKeyID                 uuid.UUID
	BusinessID, Bucket, ObjectKey, MinioUploadID       string
	FileName, ContentType, LogicalPath, IdempotencyKey string
	Size, PartSize, Reserved                           int64
	PartCount                                          int
	Status                                             string
	ExpiresAt                                          time.Time
	CancelledAt                                        sql.NullTime
	AssetID                                            uuid.NullUUID
}

// BeginMultipartUpload 预留用户额度，并在 MinIO 内网创建 multipart 会话。
func (s *Service) BeginMultipartUpload(ctx context.Context, actor Actor, input BeginUploadInput) (UploadSessionDTO, error) {
	// 1. 校验文件元数据，避免用非法路径或空文件创建长期会话。
	if strings.TrimSpace(input.BusinessID) == "" || input.SizeBytes <= 0 {
		return UploadSessionDTO{}, problem(422, "invalid_upload", "businessId、文件大小和文件名均为必填项")
	}
	if len(input.IdempotencyKey) > 128 {
		return UploadSessionDTO{}, problem(422, "invalid_idempotency_key", "幂等键长度不能超过 128 个字符")
	}
	if actor.BusinessID != "" && actor.BusinessID != strings.TrimSpace(input.BusinessID) {
		return UploadSessionDTO{}, problem(403, "api_key_business_denied", "API Key 未授权该业务")
	}
	if strings.TrimSpace(input.Path) == "" {
		input.Path = "uploads"
	}
	logicalPath, err := validateLogicalPath(input.Path)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	fileName := strings.TrimSpace(input.FileName)
	if fileName == "" || fileName == "." || strings.ContainsAny(fileName, `/\\`) || len([]rune(fileName)) > 255 || containsControl(fileName) {
		return UploadSessionDTO{}, problem(422, "invalid_file_name", "文件名无效")
	}
	contentType := strings.TrimSpace(input.ContentType)
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(fileName))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// 2. 先在数据库事务中校验权限、活动会话数并原子预留总额度。
	sessionID := uuid.New()
	objectKey := fmt.Sprintf("workspaces/%s/assets/%s", actor.WorkspaceID, sessionID)
	partCount := int((input.SizeBytes-1)/multipartPartSize + 1)
	if partCount > 10000 {
		return UploadSessionDTO{}, problem(413, "file_too_large", "文件超过当前分片策略允许的最大大小")
	}
	expiresAt := time.Now().UTC().Add(multipartSessionTTL)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	defer tx.Rollback()
	var role string
	var maxSingle, used, reserved, quota int64
	if err := tx.QueryRowContext(ctx, `SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users WHERE user_id=$1 FOR UPDATE`, actor.UserID).Scan(&role, &maxSingle, &used, &reserved, &quota); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSessionDTO{}, problem(401, "unauthorized", "请先登录")
		}
		return UploadSessionDTO{}, err
	}
	if role != "admin" {
		actor.Role = role
	}
	// 2.1 先锁定用户额度行，再查询幂等会话，串行化同一用户的并发创建重试。
	if input.IdempotencyKey != "" {
		var existing string
		var existingBusiness, existingPath, existingName, existingType string
		var existingSize int64
		err := tx.QueryRowContext(ctx, `SELECT upload_session_id::text,business_id,logical_path,original_name,content_type,expected_size_bytes FROM asset_upload_sessions WHERE owner_user_id=$1 AND idempotency_key=$2 AND status IN ('initializing','uploading','completing')`, actor.UserID, input.IdempotencyKey).Scan(&existing, &existingBusiness, &existingPath, &existingName, &existingType, &existingSize)
		if err == nil {
			tx.Rollback()
			if existingBusiness != input.BusinessID || existingPath != logicalPath || existingName != fileName || existingType != contentType || existingSize != input.SizeBytes {
				return UploadSessionDTO{}, problem(409, "idempotency_conflict", "幂等键已用于其他上传参数")
			}
			return s.GetMultipartUpload(ctx, actor, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return UploadSessionDTO{}, err
		}
	}
	if input.SizeBytes > maxSingle {
		return UploadSessionDTO{}, problem(413, "upload_quota_exceeded", "文件超过单次上传大小限制")
	}
	if used > quota || reserved > quota-used || input.SizeBytes > quota-used-reserved {
		return UploadSessionDTO{}, problem(413, "upload_quota_exceeded", "用户素材总存储额度不足")
	}
	var bucket string
	if err := tx.QueryRowContext(ctx, `SELECT bucket_name FROM business_integrations WHERE business_id=$1 AND status='active'`, input.BusinessID).Scan(&bucket); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSessionDTO{}, problem(404, "business_not_found", "业务不存在、已停用或无权访问")
		}
		return UploadSessionDTO{}, err
	}
	if actor.Role != "admin" {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_business_access WHERE user_id=$1 AND business_id=$2 AND revoked_at IS NULL)`, actor.UserID, input.BusinessID).Scan(&allowed); err != nil {
			return UploadSessionDTO{}, err
		}
		if !allowed {
			return UploadSessionDTO{}, problem(404, "business_not_found", "业务不存在、已停用或无权访问")
		}
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM asset_upload_sessions WHERE owner_user_id=$1 AND status IN ('initializing','uploading','completing') AND expires_at>now()`, actor.UserID).Scan(&active); err != nil {
		return UploadSessionDTO{}, err
	}
	if active >= multipartActiveLimit {
		return UploadSessionDTO{}, problem(429, "upload_session_limit_exceeded", "活动上传会话数量已达到上限")
	}
	if actor.APIKeyID != nil {
		result, err := tx.ExecContext(ctx, `UPDATE api_keys SET uploaded_bytes_used=uploaded_bytes_used+$2,updated_at=now() WHERE api_key_id=$1 AND status='active' AND (expires_at IS NULL OR expires_at>now()) AND (max_upload_bytes IS NULL OR uploaded_bytes_used <= max_upload_bytes-$2)`, *actor.APIKeyID, input.SizeBytes)
		if err != nil {
			return UploadSessionDTO{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return UploadSessionDTO{}, problem(429, "api_key_upload_quota_exceeded", "API Key 累计上传额度已用尽")
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_reserved_bytes=storage_reserved_bytes+$2,updated_at=now() WHERE user_id=$1`, actor.UserID, input.SizeBytes); err != nil {
		return UploadSessionDTO{}, err
	}
	placeholder := "initializing-" + sessionID.String()
	var apiKey any
	if actor.APIKeyID != nil {
		apiKey = *actor.APIKeyID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_upload_sessions(upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,api_quota_reserved,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,idempotency_key,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$13,'initializing',NULLIF($16,''),$17)`, sessionID, actor.WorkspaceID, actor.UserID, input.BusinessID, apiKey, actor.APIKeyID != nil, bucket, objectKey, placeholder, fileName, contentType, logicalPath, input.SizeBytes, multipartPartSize, partCount, input.IdempotencyKey, expiresAt); err != nil {
		return UploadSessionDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return UploadSessionDTO{}, err
	}

	// 3. 在会话行锁内创建 MinIO 会话，和取消操作串行化，避免取消先清理、初始化随后又建出孤儿分片。
	stateCtx, stateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stateCancel()
	initTx, err := s.db.BeginTx(stateCtx, nil)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	defer initTx.Rollback()
	var initializing uploadSessionRecord
	if err := s.scanUploadSession(initTx.QueryRowContext(stateCtx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,''),cancelled_at FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, sessionID), &initializing); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("initialize multipart %s: lock session: %w", sessionID, err)
	}
	if initializing.Status != "initializing" {
		if err := initTx.Commit(); err != nil {
			return UploadSessionDTO{}, err
		}
		return s.GetMultipartUpload(ctx, actor, publicID("upl", sessionID))
	}
	if initializing.CancelledAt.Valid {
		if _, err := initTx.ExecContext(stateCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',updated_at=now() WHERE upload_session_id=$1 AND status='initializing'`, sessionID); err != nil {
			return UploadSessionDTO{}, err
		}
		if err := initTx.Commit(); err != nil {
			return UploadSessionDTO{}, err
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.cleanupMultipartObject(cleanupCtx, initializing); err == nil {
			if err := s.releaseUploadReservation(cleanupCtx, initializing, "cancelled"); err == nil {
				cancel()
				return uploadSessionDTO(initializing, "cancelled"), nil
			}
		} else {
			_, _ = s.db.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET last_error=$2,updated_at=now() WHERE upload_session_id=$1 AND status='cleanup_pending'`, sessionID, truncateError(err.Error()))
		}
		cancel()
		return s.GetMultipartUpload(ctx, actor, publicID("upl", sessionID))
	}
	if s.store == nil {
		if err := initTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return UploadSessionDTO{}, err
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.failMultipartUpload(cleanupCtx, sessionID, "storage_unavailable")
		cancel()
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "MinIO 存储尚未配置")
	}
	storageCtx, storageCancel := context.WithTimeout(context.Background(), 30*time.Second)
	minioID, err := s.store.BeginMultipart(storageCtx, bucket, objectKey, contentType)
	storageCancel()
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr := s.store.AbortMultipartForObject(cleanupCtx, bucket, objectKey)
		if cleanupErr != nil {
			_, _ = initTx.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, sessionID, truncateError(cleanupErr.Error()))
			_ = initTx.Commit()
			cancel()
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "素材存储暂不可用，清理完成后会释放预留额度")
		}
		_, _ = initTx.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error='storage_unavailable',updated_at=now() WHERE upload_session_id=$1`, sessionID)
		if commitErr := initTx.Commit(); commitErr != nil {
			cancel()
			return UploadSessionDTO{}, commitErr
		}
		_ = s.releaseUploadReservation(cleanupCtx, initializing, "failed")
		cancel()
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "素材存储暂不可用")
	}
	result, err := initTx.ExecContext(stateCtx, `UPDATE asset_upload_sessions SET minio_upload_id=$2,status='uploading',updated_at=now() WHERE upload_session_id=$1 AND status='initializing'`, sessionID, minioID)
	if err == nil {
		var count int64
		count, err = result.RowsAffected()
		if err == nil && count != 1 {
			err = problem(409, "upload_session_state_invalid", "上传会话状态已变化")
		}
	}
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr := s.store.AbortMultipart(cleanupCtx, bucket, objectKey, minioID)
		if cleanupErr != nil {
			_ = initTx.Rollback()
			_, _ = s.db.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1 AND status NOT IN ('completed','cancelled','expired','failed')`, sessionID, truncateError(cleanupErr.Error()))
			cancel()
			return UploadSessionDTO{}, fmt.Errorf("initialize multipart %s: persist MinIO upload ID: %w; abort upload: %v", sessionID, err, cleanupErr)
		}
		_, _ = initTx.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error='database_update_failed',updated_at=now() WHERE upload_session_id=$1`, sessionID)
		if commitErr := initTx.Commit(); commitErr != nil {
			cancel()
			return UploadSessionDTO{}, commitErr
		}
		_ = s.releaseUploadReservation(cleanupCtx, initializing, "failed")
		cancel()
		return UploadSessionDTO{}, err
	}
	if err := initTx.Commit(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr := s.store.AbortMultipart(cleanupCtx, bucket, objectKey, minioID)
		if cleanupErr == nil {
			_, _ = s.db.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error='database_commit_failed',updated_at=now() WHERE upload_session_id=$1`, sessionID)
			_ = s.releaseUploadReservation(cleanupCtx, initializing, "failed")
		} else {
			_, _ = s.db.ExecContext(cleanupCtx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, sessionID, truncateError(cleanupErr.Error()))
		}
		cancel()
		return UploadSessionDTO{}, fmt.Errorf("initialize multipart %s: commit MinIO upload ID: %w", sessionID, err)
	}
	return s.GetMultipartUpload(ctx, actor, publicID("upl", sessionID))
}

func (s *Service) UploadMultipartPart(ctx context.Context, actor Actor, input UploadPartInput) (UploadSessionDTO, error) {
	// 1. 对同一会话的分片持有共享行锁；完成与取消获取排他行锁等待在途分片。
	id, err := parseID("upl", input.SessionID)
	if err != nil {
		return UploadSessionDTO{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	defer tx.Rollback()
	var session uploadSessionRecord
	err = s.scanUploadSession(tx.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,''),cancelled_at FROM asset_upload_sessions WHERE upload_session_id=$1 FOR SHARE`, id), &session)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadSessionDTO{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	if err != nil {
		return UploadSessionDTO{}, err
	}
	if err := s.authorizeUploadSession(actor, session); err != nil {
		return UploadSessionDTO{}, err
	}
	if session.Status != "uploading" || time.Now().UTC().After(session.ExpiresAt) {
		return UploadSessionDTO{}, problem(409, "upload_session_expired", "上传会话已过期或不可继续上传")
	}
	if input.PartNumber < 1 || input.PartNumber > session.PartCount || input.ContentLength <= 0 {
		return UploadSessionDTO{}, problem(422, "upload_part_invalid", "分片序号或大小无效")
	}
	expected := session.PartSize
	if input.PartNumber == session.PartCount {
		expected = session.Size - session.PartSize*int64(session.PartCount-1)
	}
	if input.ContentLength != expected {
		return UploadSessionDTO{}, problem(422, "upload_part_invalid", "分片大小与会话定义不一致")
	}
	if s.store == nil {
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "MinIO 存储尚未配置")
	}
	limited := &io.LimitedReader{R: input.Reader, N: expected}
	etag, err := s.store.PutMultipartPart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID, input.PartNumber, limited, expected)
	if err != nil {
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "分片写入存储失败")
	}
	if limited.N != 0 {
		return UploadSessionDTO{}, problem(422, "upload_part_invalid", "分片实际内容长度与声明不一致")
	}
	var extra [1]byte
	if n, readErr := io.ReadFull(input.Reader, extra[:]); n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return UploadSessionDTO{}, problem(422, "upload_part_invalid", "分片实际内容长度与声明不一致")
	}
	// 2. 记录 ETag 及大小；重复上传同一序号是幂等覆盖。
	etag = strings.Trim(etag, "\"")
	if etag == "" {
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "存储未返回有效的分片标识")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_upload_parts(upload_session_id,part_number,size_bytes,etag) VALUES($1,$2,$3,$4) ON CONFLICT(upload_session_id,part_number) DO UPDATE SET size_bytes=EXCLUDED.size_bytes,etag=EXCLUDED.etag,updated_at=now()`, session.ID, input.PartNumber, expected, etag); err != nil {
		return UploadSessionDTO{}, err
	}
	if err := tx.Commit(); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("upload multipart %s: commit part record: %w", session.ID, err)
	}
	return s.GetMultipartUpload(ctx, actor, publicID("upl", session.ID))
}

func (s *Service) GetMultipartUpload(ctx context.Context, actor Actor, rawID string) (UploadSessionDTO, error) {
	// 1. 状态查询只返回分片号和进度，不暴露 MinIO bucket、object key 或 upload ID。
	session, err := s.loadUploadSession(ctx, rawID)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	if err := s.authorizeUploadSession(actor, session); err != nil {
		return UploadSessionDTO{}, err
	}
	parts := make([]int, 0)
	var received int64
	if session.Status == "completed" {
		for number := 1; number <= session.PartCount; number++ {
			parts = append(parts, number)
		}
		received = session.Size
	}
	if session.Status == "uploading" || session.Status == "completing" {
		if s.store == nil {
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "MinIO 存储尚未配置")
		}
		if size, statErr := s.store.ObjectSize(ctx, session.Bucket, session.ObjectKey); statErr == nil && size == session.Size {
			for number := 1; number <= session.PartCount; number++ {
				parts = append(parts, number)
			}
			received = session.Size
		} else {
			remoteParts, err := s.store.ListMultipartParts(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
			if err != nil {
				if session.Status != "completing" {
					return UploadSessionDTO{}, problem(503, "storage_unavailable", "无法读取已上传分片状态")
				}
				// MinIO 合并后 upload ID 已失效；大小探针暂时失败时保留 completing 任务供用户重试完成。
				for number := 1; number <= session.PartCount; number++ {
					parts = append(parts, number)
				}
				received = session.Size
			} else {
				for _, part := range remoteParts {
					parts = append(parts, part.PartNumber)
					received += part.Size
				}
			}
		}
	}
	result := UploadSessionDTO{UploadSessionID: publicID("upl", session.ID), IdempotencyKey: session.IdempotencyKey, Status: session.Status, FileName: session.FileName, BusinessID: session.BusinessID, Path: session.LogicalPath, SizeBytes: session.Size, PartSizeBytes: session.PartSize, PartCount: session.PartCount, ReceivedParts: parts, ReceivedBytes: received, ExpiresAt: session.ExpiresAt.UTC()}
	if session.Status == "initializing" && session.CancelledAt.Valid {
		result.Status = "cleanup_pending"
	}
	if session.AssetID.Valid {
		asset, err := s.Get(ctx, actor, publicID("ast", session.AssetID.UUID))
		if err != nil {
			return UploadSessionDTO{}, err
		}
		result.Asset = &asset
	}
	return result, nil
}

func (s *Service) ListMultipartUploads(ctx context.Context, actor Actor) ([]UploadSessionDTO, error) {
	// 1. 恢复入口仅返回当前用户尚未完成且未过期的会话。
	rows, err := s.db.QueryContext(ctx, `SELECT upload_session_id::text FROM asset_upload_sessions WHERE owner_user_id=$1 AND ($2='' OR business_id=$2) AND ((status IN ('uploading','completing') AND expires_at>now()) OR status='cleanup_pending' OR (status='initializing' AND cancelled_at IS NOT NULL)) ORDER BY created_at DESC`, actor.UserID, actor.BusinessID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, publicID("upl", mustUUID(id)))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	items := make([]UploadSessionDTO, 0, len(ids))
	for _, id := range ids {
		item, err := s.GetMultipartUpload(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) CompleteMultipartUpload(ctx context.Context, actor Actor, rawID string, meta Meta) (UploadSessionDTO, error) {
	// 1. 排他锁等待在途分片完成，并阻止取消或重复完成并发改写对象。
	id, err := parseID("upl", rawID)
	if err != nil {
		return UploadSessionDTO{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	defer tx.Rollback()
	var session uploadSessionRecord
	err = s.scanUploadSession(tx.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,''),cancelled_at FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, id), &session)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadSessionDTO{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	if err != nil {
		return UploadSessionDTO{}, err
	}
	if err := s.authorizeUploadSession(actor, session); err != nil {
		return UploadSessionDTO{}, err
	}
	if session.Status == "completed" {
		_ = tx.Rollback()
		return s.GetMultipartUpload(ctx, actor, rawID)
	}
	if (session.Status != "uploading" && session.Status != "completing") || time.Now().UTC().After(session.ExpiresAt) {
		return UploadSessionDTO{}, problem(409, "upload_session_expired", "上传会话已过期或不可完成")
	}
	if s.store == nil {
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "MinIO 存储尚未配置")
	}
	if session.Status == "uploading" {
		if _, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='completing',updated_at=now() WHERE upload_session_id=$1 AND status='uploading'`, session.ID); err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: mark completing: %w", session.ID, err)
		}
	}
	actual, objectErr := s.store.ObjectSize(ctx, session.Bucket, session.ObjectKey)
	if objectErr == nil && actual != session.Size {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		removeErr := s.cleanupMultipartObject(cleanupCtx, session)
		cancel()
		if removeErr != nil {
			_, _ = tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, session.ID, truncateError(removeErr.Error()))
			if err := tx.Commit(); err != nil {
				return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: persist cleanup state: %w", session.ID, err)
			}
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "合并对象校验失败，清理尚未完成，请稍后重试")
		}
		if err := s.releaseUploadReservationTx(ctx, tx, session, "failed", "upload_checksum_mismatch"); err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: release failed upload reservation: %w", session.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: commit failed upload cleanup: %w", session.ID, err)
		}
		return UploadSessionDTO{}, problem(422, "upload_checksum_mismatch", "合并后的对象大小校验失败")
	}
	if objectErr != nil {
		parts, err := s.store.ListMultipartParts(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
		if err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: list uploaded parts: %w", session.ID, problem(503, "storage_unavailable", "无法读取已上传分片"))
		}
		if len(parts) != session.PartCount {
			return UploadSessionDTO{}, problem(409, "upload_parts_incomplete", "分片尚未全部上传")
		}
		completeParts := make([]minio.CompletePart, session.PartCount)
		seen := make([]bool, session.PartCount)
		for _, part := range parts {
			if part.PartNumber < 1 || part.PartNumber > session.PartCount || seen[part.PartNumber-1] || part.ETag == "" {
				return UploadSessionDTO{}, problem(409, "upload_parts_incomplete", "分片序号无效")
			}
			expectedSize := session.PartSize
			if part.PartNumber == session.PartCount {
				expectedSize = session.Size - session.PartSize*int64(session.PartCount-1)
			}
			if part.Size != expectedSize {
				return UploadSessionDTO{}, problem(409, "upload_parts_incomplete", "分片大小与会话定义不一致")
			}
			seen[part.PartNumber-1] = true
			completeParts[part.PartNumber-1] = minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag}
		}
		if err := s.store.CompleteMultipart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID, completeParts, session.ContentType); err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: merge objects: %w", session.ID, problem(503, "storage_unavailable", "合并素材失败"))
		}
	}

	// 2. 合并后确认对象大小，再把预留额度转成已用额度并登记素材。
	actual, err = s.store.ObjectSize(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: verify merged object size: %w", session.ID, problem(503, "storage_unavailable", "素材已合并，暂时无法确认文件大小；请稍后重试完成"))
	}
	if actual != session.Size {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		removeErr := s.cleanupMultipartObject(cleanupCtx, session)
		cancel()
		if removeErr != nil {
			_, _ = tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, session.ID, truncateError(removeErr.Error()))
			if err := tx.Commit(); err != nil {
				return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: persist cleanup state: %w", session.ID, err)
			}
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "合并对象校验失败，清理尚未完成，请稍后重试")
		}
		if err := s.releaseUploadReservationTx(ctx, tx, session, "failed", "upload_checksum_mismatch"); err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: release failed upload reservation: %w", session.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: commit failed upload cleanup: %w", session.ID, err)
		}
		return UploadSessionDTO{}, problem(422, "upload_checksum_mismatch", "合并后的对象大小校验失败")
	}
	assetID := uuid.New()
	now := time.Now().UTC()
	if session.Status != "uploading" && session.Status != "completing" {
		return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传会话状态已变化")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assets(asset_id,workspace_id,owner_user_id,business_id,logical_path,bucket,object_key,original_name,content_type,size_bytes,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, assetID, session.WorkspaceID, session.OwnerID, session.BusinessID, session.LogicalPath, session.Bucket, session.ObjectKey, session.FileName, session.ContentType, session.Size, now); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: insert asset: %w", session.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_reserved_bytes=GREATEST(0,storage_reserved_bytes-$2),storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`, session.OwnerID, session.Reserved); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: settle storage quota: %w", session.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='completed',asset_id=$2,api_quota_reserved=false,completed_at=$3,updated_at=$3 WHERE upload_session_id=$1`, session.ID, assetID, now); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: complete session: %w", session.ID, err)
	}
	if err := audit(ctx, tx, actor.UserID, session.WorkspaceID, "asset.multipart_complete", assetID.String(), meta, map[string]any{"assetId": publicID("ast", assetID), "businessId": session.BusinessID, "sizeBytes": session.Size}); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: write audit log: %w", session.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: commit transaction: %w", session.ID, err)
	}
	asset := AssetDTO{AssetID: publicID("ast", assetID), BusinessID: session.BusinessID, Path: session.LogicalPath, FileName: session.FileName, ContentType: session.ContentType, SizeBytes: session.Size, CreatedAt: now}
	result := uploadSessionDTO(session, "completed")
	result.Asset = &asset
	result.ReceivedBytes = session.Size
	for number := 1; number <= session.PartCount; number++ {
		result.ReceivedParts = append(result.ReceivedParts, number)
	}
	return result, nil
}

func (s *Service) CancelMultipartUpload(ctx context.Context, actor Actor, rawID string) (UploadSessionDTO, error) {
	// 1. FOR UPDATE 等待并发分片的 FOR SHARE 事务结束，并阻止取消状态后再启动新分片。
	id, err := parseID("upl", rawID)
	if err != nil {
		return UploadSessionDTO{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadSessionDTO{}, fmt.Errorf("cancel multipart %s: begin state transaction: %w", id, err)
	}
	defer tx.Rollback()
	var session uploadSessionRecord
	err = s.scanUploadSession(tx.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,''),cancelled_at FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, id), &session)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadSessionDTO{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	if err != nil {
		return UploadSessionDTO{}, fmt.Errorf("cancel multipart %s: load session: %w", id, err)
	}
	if err := s.authorizeUploadSession(actor, session); err != nil {
		return UploadSessionDTO{}, err
	}
	if session.Status == "cancelled" {
		_ = tx.Rollback()
		return uploadSessionDTO(session, "cancelled"), nil
	}
	if session.Status == "completed" {
		return UploadSessionDTO{}, problem(409, "upload_session_completed", "已完成的上传不能取消")
	}
	if session.Status != "initializing" && session.Status != "uploading" && session.Status != "completing" && session.Status != "cleanup_pending" {
		return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传会话已结束，不能取消")
	}
	result, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',cancelled_at=COALESCE(cancelled_at,now()),updated_at=now() WHERE upload_session_id=$1 AND status IN ('initializing','uploading','completing','cleanup_pending')`, session.ID)
	if err != nil {
		return UploadSessionDTO{}, fmt.Errorf("cancel multipart %s: mark cleanup pending: %w", session.ID, err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return UploadSessionDTO{}, fmt.Errorf("cancel multipart %s: verify cleanup marker: %w", session.ID, err)
		}
		return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传会话状态已变化，请刷新后重试")
	}
	if err := tx.Commit(); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("cancel multipart %s: commit cleanup marker: %w", session.ID, err)
	}
	if s.store == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET last_error='object_store_unavailable',updated_at=now() WHERE upload_session_id=$1 AND status='cleanup_pending'`, session.ID)
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "上传清理暂未完成，请稍后重试")
	}
	if storageErr := s.cleanupMultipartObject(ctx, session); storageErr != nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, session.ID, truncateError(storageErr.Error()))
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "上传清理暂未完成，请稍后重试")
	}
	if err := s.releaseUploadReservation(ctx, session, "cancelled"); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("cancel multipart %s: release quota reservation: %w", session.ID, err)
	}
	return uploadSessionDTO(session, "cancelled"), nil
}

func uploadSessionDTO(session uploadSessionRecord, status string) UploadSessionDTO {
	return UploadSessionDTO{UploadSessionID: publicID("upl", session.ID), IdempotencyKey: session.IdempotencyKey, Status: status, FileName: session.FileName, BusinessID: session.BusinessID, Path: session.LogicalPath, SizeBytes: session.Size, PartSizeBytes: session.PartSize, PartCount: session.PartCount, ReceivedParts: []int{}, ExpiresAt: session.ExpiresAt.UTC()}
}

func (s *Service) loadUploadSession(ctx context.Context, rawID string) (uploadSessionRecord, error) {
	id, err := parseID("upl", rawID)
	if err != nil {
		return uploadSessionRecord{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	var result uploadSessionRecord
	err = s.scanUploadSession(s.db.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,''),cancelled_at FROM asset_upload_sessions WHERE upload_session_id=$1`, id), &result)
	if errors.Is(err, sql.ErrNoRows) {
		return uploadSessionRecord{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	if err != nil {
		return uploadSessionRecord{}, err
	}
	return result, nil
}

func (s *Service) scanUploadSession(row interface{ Scan(...any) error }, result *uploadSessionRecord) error {
	var apiKey uuid.NullUUID
	if err := row.Scan(&result.ID, &result.WorkspaceID, &result.OwnerID, &result.BusinessID, &apiKey, &result.Bucket, &result.ObjectKey, &result.MinioUploadID, &result.FileName, &result.ContentType, &result.LogicalPath, &result.Size, &result.PartSize, &result.PartCount, &result.Reserved, &result.Status, &result.ExpiresAt, &result.AssetID, &result.IdempotencyKey, &result.CancelledAt); err != nil {
		return err
	}
	if apiKey.Valid {
		result.APIKeyID = apiKey.UUID
	}
	return nil
}

func (s *Service) authorizeUploadSession(actor Actor, session uploadSessionRecord) error {
	if actor.Role != "admin" && actor.UserID != session.OwnerID {
		return problem(404, "upload_session_not_found", "上传会话不存在或无权访问")
	}
	if actor.BusinessID != "" && actor.BusinessID != session.BusinessID {
		return problem(403, "api_key_business_denied", "API Key 未授权该业务")
	}
	return nil
}

func (s *Service) failMultipartUpload(ctx context.Context, id uuid.UUID, reason string) error {
	session, err := s.loadUploadSession(ctx, publicID("upl", id))
	if err != nil {
		return err
	}
	if err := s.releaseUploadReservation(ctx, session, "failed"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET last_error=$2,updated_at=now() WHERE upload_session_id=$1`, id, reason)
	return err
}

func (s *Service) releaseUploadReservation(ctx context.Context, session uploadSessionRecord, status string) error {
	// 1. 用户存储额度、API Key 预留额度与会话终态在同一事务结算。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin quota release transaction: %w", err)
	}
	defer tx.Rollback()
	if err := s.releaseUploadReservationTx(ctx, tx, session, status, ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit quota release transaction: %w", err)
	}
	return nil
}

func (s *Service) releaseUploadReservationTx(ctx context.Context, tx *sql.Tx, session uploadSessionRecord, status, reason string) error {
	var reserved bool
	var currentStatus string
	if err := tx.QueryRowContext(ctx, `SELECT api_quota_reserved,status FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, session.ID).Scan(&reserved, &currentStatus); err != nil {
		return fmt.Errorf("lock upload session for quota release: %w", err)
	}
	if currentStatus == "completed" || currentStatus == "cancelled" || currentStatus == "expired" || currentStatus == "failed" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_reserved_bytes=GREATEST(0,storage_reserved_bytes-$2),updated_at=now() WHERE user_id=$1`, session.OwnerID, session.Reserved); err != nil {
		return fmt.Errorf("release user storage reservation: %w", err)
	}
	if reserved && session.APIKeyID != uuid.Nil {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET uploaded_bytes_used=GREATEST(0,uploaded_bytes_used-$2),updated_at=now() WHERE api_key_id=$1`, session.APIKeyID, session.Reserved); err != nil {
			return fmt.Errorf("release API Key upload reservation: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET api_quota_reserved=false,status=$2::varchar,cancelled_at=CASE WHEN $2::varchar='cancelled' THEN now() ELSE cancelled_at END,last_error=NULLIF($3::text,''),updated_at=now() WHERE upload_session_id=$1 AND status<>'completed'`, session.ID, status, reason); err != nil {
		return fmt.Errorf("finish upload session quota release: %w", err)
	}
	return nil
}

func (s *Service) cleanupMultipartObject(ctx context.Context, session uploadSessionRecord) error {
	if s.store == nil {
		return fmt.Errorf("object store is unavailable")
	}
	exists, err := s.store.ObjectExists(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		return err
	}
	if exists {
		return s.store.Remove(ctx, session.Bucket, session.ObjectKey)
	}
	if session.Status == "initializing" || strings.HasPrefix(session.MinioUploadID, "initializing-") {
		return s.store.AbortMultipartForObject(ctx, session.Bucket, session.ObjectKey)
	}
	return s.store.AbortMultipart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
}

// CleanupMultipartUploads 回收过期或取消清理失败的会话，供后台 worker 周期调用。
func (s *Service) CleanupMultipartUploads(ctx context.Context, limit int) error {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT upload_session_id::text FROM asset_upload_sessions WHERE status='cleanup_pending' OR (status IN ('initializing','uploading','completing') AND expires_at<=now()) ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, publicID("upl", mustUUID(id)))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if s.store == nil {
			continue
		}
		sessionID, parseErr := parseID("upl", id)
		if parseErr != nil {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var session uploadSessionRecord
		err = s.scanUploadSession(tx.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,''),cancelled_at FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, sessionID), &session)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			continue
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if session.Status == "completed" || session.Status == "cancelled" || session.Status == "expired" || session.Status == "failed" || (session.Status != "cleanup_pending" && session.ExpiresAt.After(time.Now().UTC())) {
			tx.Rollback()
			continue
		}
		if session.Status != "cleanup_pending" {
			if _, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',updated_at=now() WHERE upload_session_id=$1`, session.ID); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err := s.cleanupMultipartObject(ctx, session); err != nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET last_error=$2,updated_at=now() WHERE upload_session_id=$1 AND status='cleanup_pending'`, session.ID, truncateError(err.Error()))
			continue
		}
		finalStatus := "expired"
		if session.CancelledAt.Valid {
			finalStatus = "cancelled"
		}
		if err := s.releaseUploadReservation(ctx, session, finalStatus); err != nil {
			return err
		}
	}
	return nil
}

func mustUUID(value string) uuid.UUID {
	id, _ := uuid.Parse(value)
	return id
}
