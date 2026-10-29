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
	// 2.1 幂等重试优先返回原会话，不能因为原会话已经预留额度而再次触发额度不足。
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

	// 3. MinIO 会话创建与数据库无法组成原子事务，失败时立即释放预留额度。
	if s.store == nil {
		_ = s.failMultipartUpload(ctx, actor, sessionID, "storage_unavailable")
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "MinIO 存储尚未配置")
	}
	minioID, err := s.store.BeginMultipart(ctx, bucket, objectKey, contentType)
	if err != nil {
		_ = s.failMultipartUpload(ctx, actor, sessionID, "storage_unavailable")
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "素材存储暂不可用")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET minio_upload_id=$2,status='uploading',updated_at=now() WHERE upload_session_id=$1 AND status='initializing'`, sessionID, minioID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.store.AbortMultipart(cleanupCtx, bucket, objectKey, minioID)
		cancel()
		_ = s.failMultipartUpload(ctx, actor, sessionID, "database_update_failed")
		return UploadSessionDTO{}, err
	}
	return s.GetMultipartUpload(ctx, actor, publicID("upl", sessionID))
}

func (s *Service) UploadMultipartPart(ctx context.Context, actor Actor, input UploadPartInput) (UploadSessionDTO, error) {
	// 1. 先读取会话元数据，分片字节随后直接流入 MinIO，不落入内存。
	session, err := s.loadUploadSession(ctx, input.SessionID)
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
	if _, err := s.store.PutMultipartPart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID, input.PartNumber, io.LimitReader(input.Reader, expected), expected); err != nil {
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "分片写入存储失败")
	}
	// 2. 记录 ETag 及大小；重复上传同一序号是幂等覆盖。
	var etag string
	parts, err := s.store.ListMultipartParts(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
	if err == nil {
		for _, part := range parts {
			if part.PartNumber == input.PartNumber {
				etag = strings.Trim(part.ETag, "\"")
			}
		}
	}
	if etag == "" {
		return UploadSessionDTO{}, problem(503, "storage_unavailable", "无法确认分片写入结果")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO asset_upload_parts(upload_session_id,part_number,size_bytes,etag) VALUES($1,$2,$3,$4) ON CONFLICT(upload_session_id,part_number) DO UPDATE SET size_bytes=EXCLUDED.size_bytes,etag=EXCLUDED.etag,updated_at=now()`, session.ID, input.PartNumber, expected, etag); err != nil {
		return UploadSessionDTO{}, err
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
	if session.Status == "uploading" || session.Status == "completing" || session.Status == "cleanup_pending" {
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
				return UploadSessionDTO{}, problem(503, "storage_unavailable", "无法读取已上传分片状态")
			}
			for _, part := range remoteParts {
				parts = append(parts, part.PartNumber)
				received += part.Size
			}
		}
	}
	result := UploadSessionDTO{UploadSessionID: publicID("upl", session.ID), IdempotencyKey: session.IdempotencyKey, Status: session.Status, FileName: session.FileName, BusinessID: session.BusinessID, Path: session.LogicalPath, SizeBytes: session.Size, PartSizeBytes: session.PartSize, PartCount: session.PartCount, ReceivedParts: parts, ReceivedBytes: received, ExpiresAt: session.ExpiresAt.UTC()}
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
	rows, err := s.db.QueryContext(ctx, `SELECT upload_session_id::text FROM asset_upload_sessions WHERE owner_user_id=$1 AND ($2='' OR business_id=$2) AND status IN ('uploading','completing') AND expires_at>now() ORDER BY created_at DESC`, actor.UserID, actor.BusinessID)
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
	// 1. 先锁定完成状态，防止两个客户端重复合并并重复结算额度。
	session, err := s.loadUploadSession(ctx, rawID)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	if err := s.authorizeUploadSession(actor, session); err != nil {
		return UploadSessionDTO{}, err
	}
	if session.Status == "completed" {
		return s.GetMultipartUpload(ctx, actor, rawID)
	}
	if (session.Status != "uploading" && session.Status != "completing") || time.Now().UTC().After(session.ExpiresAt) {
		return UploadSessionDTO{}, problem(409, "upload_session_expired", "上传会话已过期或不可完成")
	}
	if session.Status == "uploading" {
		result, err := s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='completing',updated_at=now() WHERE upload_session_id=$1 AND status='uploading'`, session.ID)
		if err != nil {
			return UploadSessionDTO{}, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传会话状态已变化")
		}
	}
	actual, objectErr := s.store.ObjectSize(ctx, session.Bucket, session.ObjectKey)
	if objectErr != nil || actual != session.Size {
		parts, err := s.store.ListMultipartParts(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
		if err != nil {
			_ = s.restoreCompleting(ctx, session.ID, err.Error())
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "无法读取已上传分片")
		}
		if len(parts) != session.PartCount {
			_ = s.restoreCompleting(ctx, session.ID, "parts_incomplete")
			return UploadSessionDTO{}, problem(409, "upload_parts_incomplete", "分片尚未全部上传")
		}
		completeParts := make([]minio.CompletePart, 0, len(parts))
		for _, part := range parts {
			if part.PartNumber < 1 || part.PartNumber > session.PartCount {
				_ = s.restoreCompleting(ctx, session.ID, "part_number_invalid")
				return UploadSessionDTO{}, problem(409, "upload_parts_incomplete", "分片序号无效")
			}
			completeParts = append(completeParts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})
		}
		if err := s.store.CompleteMultipart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID, completeParts, session.ContentType); err != nil {
			_ = s.restoreCompleting(ctx, session.ID, err.Error())
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "合并素材失败")
		}
	}

	// 2. 合并后确认对象大小，再把预留额度转成已用额度并登记素材。
	actual, err = s.store.ObjectSize(ctx, session.Bucket, session.ObjectKey)
	if err != nil || actual != session.Size {
		if err == nil {
			err = fmt.Errorf("actual size %d", actual)
		}
		if removeErr := s.store.Remove(ctx, session.Bucket, session.ObjectKey); removeErr != nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, session.ID, truncateError(removeErr.Error()))
		} else {
			_ = s.failMultipartUpload(ctx, actor, session.ID, "upload_checksum_mismatch")
		}
		return UploadSessionDTO{}, problem(422, "upload_checksum_mismatch", "合并后的对象大小校验失败")
	}
	assetID := uuid.New()
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: begin transaction: %w", session.ID, err)
	}
	defer tx.Rollback()
	var locked uploadSessionRecord
	// 2.1 锁定当前会话时必须传入 ID，确保查询占位符与参数对应。
	if err := s.scanUploadSession(tx.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,'') FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, session.ID), &locked); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: lock session: %w", session.ID, err)
	}
	if locked.Status == "completed" && locked.AssetID.Valid {
		// 2.2 其他请求已完成时先释放事务，再查询素材，避免再次加锁等待自己。
		tx.Rollback()
		return s.GetMultipartUpload(ctx, actor, rawID)
	}
	if locked.Status != "completing" {
		return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传会话状态已变化")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assets(asset_id,workspace_id,owner_user_id,business_id,logical_path,bucket,object_key,original_name,content_type,size_bytes,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, assetID, locked.WorkspaceID, locked.OwnerID, locked.BusinessID, locked.LogicalPath, locked.Bucket, locked.ObjectKey, locked.FileName, locked.ContentType, locked.Size, now); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: insert asset: %w", session.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_reserved_bytes=GREATEST(0,storage_reserved_bytes-$2),storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`, locked.OwnerID, locked.Reserved); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: settle storage quota: %w", session.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='completed',asset_id=$2,api_quota_reserved=false,completed_at=$3,updated_at=$3 WHERE upload_session_id=$1`, locked.ID, assetID, now); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: complete session: %w", session.ID, err)
	}
	if err := audit(ctx, tx, actor.UserID, locked.WorkspaceID, "asset.multipart_complete", assetID.String(), meta, map[string]any{"assetId": publicID("ast", assetID), "businessId": locked.BusinessID, "sizeBytes": locked.Size}); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: write audit log: %w", session.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: commit transaction: %w", session.ID, err)
	}
	result, err := s.GetMultipartUpload(ctx, actor, rawID)
	if err != nil {
		return UploadSessionDTO{}, fmt.Errorf("complete multipart %s: read completed asset: %w", session.ID, err)
	}
	return result, nil
}

func (s *Service) CancelMultipartUpload(ctx context.Context, actor Actor, rawID string) (UploadSessionDTO, error) {
	// 1. 只有 MinIO 确认中止成功后才释放用户的预留额度。
	session, err := s.loadUploadSession(ctx, rawID)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	if err := s.authorizeUploadSession(actor, session); err != nil {
		return UploadSessionDTO{}, err
	}
	if session.Status == "cancelled" {
		return s.GetMultipartUpload(ctx, actor, rawID)
	}
	if session.Status == "completed" {
		return UploadSessionDTO{}, problem(409, "upload_session_completed", "已完成的上传不能取消")
	}
	if session.Status == "completing" {
		return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传正在合并，请稍后查询状态")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',updated_at=now() WHERE upload_session_id=$1 AND status IN ('initializing','uploading','cleanup_pending')`, session.ID)
	if err != nil {
		return UploadSessionDTO{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return UploadSessionDTO{}, problem(409, "upload_session_state_invalid", "上传会话状态已变化，请重新查询")
	}
	if session.Status == "initializing" {
		if s.store == nil || s.store.AbortMultipartForObject(ctx, session.Bucket, session.ObjectKey) != nil {
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "上传清理暂未完成，请稍后重试")
		}
	} else {
		exists, storageErr := s.store.ObjectExists(ctx, session.Bucket, session.ObjectKey)
		if storageErr != nil {
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "上传清理暂未完成，请稍后重试")
		}
		if exists {
			storageErr = s.store.Remove(ctx, session.Bucket, session.ObjectKey)
		} else {
			storageErr = s.store.AbortMultipart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
		}
		if storageErr != nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`, session.ID, truncateError(storageErr.Error()))
			return UploadSessionDTO{}, problem(503, "storage_unavailable", "上传清理暂未完成，请稍后重试")
		}
	}
	if err := s.releaseUploadReservation(ctx, session, "cancelled"); err != nil {
		return UploadSessionDTO{}, err
	}
	return s.GetMultipartUpload(ctx, actor, rawID)
}

func (s *Service) loadUploadSession(ctx context.Context, rawID string) (uploadSessionRecord, error) {
	id, err := parseID("upl", rawID)
	if err != nil {
		return uploadSessionRecord{}, problem(404, "upload_session_not_found", "上传会话不存在")
	}
	var result uploadSessionRecord
	err = s.scanUploadSession(s.db.QueryRowContext(ctx, `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,bucket,object_key,minio_upload_id,original_name,content_type,logical_path,expected_size_bytes,part_size_bytes,part_count,reserved_bytes,status,expires_at,asset_id,COALESCE(idempotency_key,'') FROM asset_upload_sessions WHERE upload_session_id=$1`, id), &result)
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
	if err := row.Scan(&result.ID, &result.WorkspaceID, &result.OwnerID, &result.BusinessID, &apiKey, &result.Bucket, &result.ObjectKey, &result.MinioUploadID, &result.FileName, &result.ContentType, &result.LogicalPath, &result.Size, &result.PartSize, &result.PartCount, &result.Reserved, &result.Status, &result.ExpiresAt, &result.AssetID, &result.IdempotencyKey); err != nil {
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

func (s *Service) restoreCompleting(ctx context.Context, id uuid.UUID, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asset_upload_sessions SET status='uploading',last_error=$2,updated_at=now() WHERE upload_session_id=$1 AND status='completing'`, id, truncateError(message))
	return err
}

func (s *Service) failMultipartUpload(ctx context.Context, actor Actor, id uuid.UUID, reason string) error {
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
		return err
	}
	defer tx.Rollback()
	var reserved bool
	var currentStatus string
	if err := tx.QueryRowContext(ctx, `SELECT api_quota_reserved,status FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`, session.ID).Scan(&reserved, &currentStatus); err != nil {
		return err
	}
	if currentStatus == "completed" || currentStatus == "cancelled" || currentStatus == "expired" || currentStatus == "failed" {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET storage_reserved_bytes=GREATEST(0,storage_reserved_bytes-$2),updated_at=now() WHERE user_id=$1`, session.OwnerID, session.Reserved); err != nil {
		return err
	}
	if reserved && session.APIKeyID != uuid.Nil {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET uploaded_bytes_used=GREATEST(0,uploaded_bytes_used-$2),updated_at=now() WHERE api_key_id=$1`, session.APIKeyID, session.Reserved); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_upload_sessions SET api_quota_reserved=false,status=$2,cancelled_at=CASE WHEN $2='cancelled' THEN now() ELSE cancelled_at END,updated_at=now() WHERE upload_session_id=$1 AND status<>'completed'`, session.ID, status); err != nil {
		return err
	}
	return tx.Commit()
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
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, publicID("upl", mustUUID(id)))
	}
	for _, id := range ids {
		session, loadErr := s.loadUploadSession(ctx, id)
		if loadErr != nil || s.store == nil {
			continue
		}
		if session.Status == "initializing" {
			if err := s.store.AbortMultipartForObject(ctx, session.Bucket, session.ObjectKey); err != nil {
				continue
			}
		} else {
			exists, err := s.store.ObjectExists(ctx, session.Bucket, session.ObjectKey)
			if err != nil {
				continue
			}
			if exists {
				err = s.store.Remove(ctx, session.Bucket, session.ObjectKey)
			} else {
				err = s.store.AbortMultipart(ctx, session.Bucket, session.ObjectKey, session.MinioUploadID)
			}
			if err != nil {
				continue
			}
		}
		if err := s.releaseUploadReservation(ctx, session, "expired"); err != nil {
			return err
		}
	}
	return rows.Err()
}

func mustUUID(value string) uuid.UUID {
	id, _ := uuid.Parse(value)
	return id
}
