package assets

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

type cancelTestObjectStore struct {
	assetObjectStore
	exists           bool
	existsErr        error
	abortErr         error
	abortObjectErr   error
	removeErr        error
	abortCalls       int
	abortObjectCalls int
	removeCalls      int
	putPartErr       error
	putPartCalls     int
	putPartBytes     []byte
	partETag         string
	parts            []minio.ObjectPart
	objectSize       int64
	objectSizeErr    error
	objectSizeSteps  []objectSizeResult
	completeErr      error
	completeCalls    int
}

type objectSizeResult struct {
	size int64
	err  error
}

func (s *cancelTestObjectStore) ObjectExists(context.Context, string, string) (bool, error) {
	return s.exists, s.existsErr
}

func (s *cancelTestObjectStore) AbortMultipart(context.Context, string, string, string) error {
	s.abortCalls++
	return s.abortErr
}

func (s *cancelTestObjectStore) AbortMultipartForObject(context.Context, string, string) error {
	s.abortObjectCalls++
	return s.abortObjectErr
}

func (s *cancelTestObjectStore) Remove(context.Context, string, string) error {
	s.removeCalls++
	return s.removeErr
}

func (s *cancelTestObjectStore) ObjectSize(context.Context, string, string) (int64, error) {
	if len(s.objectSizeSteps) > 0 {
		result := s.objectSizeSteps[0]
		s.objectSizeSteps = s.objectSizeSteps[1:]
		return result.size, result.err
	}
	return s.objectSize, s.objectSizeErr
}

func (s *cancelTestObjectStore) PutMultipartPart(_ context.Context, _, _, _ string, _ int, body io.Reader, _ int64) (string, error) {
	s.putPartCalls++
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	s.putPartBytes = data
	return s.partETag, s.putPartErr
}

func (s *cancelTestObjectStore) ListMultipartParts(context.Context, string, string, string) ([]minio.ObjectPart, error) {
	return s.parts, nil
}

func (s *cancelTestObjectStore) CompleteMultipart(context.Context, string, string, string, []minio.CompletePart, string) error {
	s.completeCalls++
	return s.completeErr
}

func TestCancelMultipartUploadReleasesReservationAfterStorageCleanup(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	store := &cancelTestObjectStore{}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "uploading")
	expectReservationRelease(mock, session, "cancelled")

	result, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	if err != nil {
		t.Fatalf("CancelMultipartUpload() error = %v", err)
	}
	if result.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
	if store.abortCalls != 1 || result.UploadSessionID != rawID {
		t.Fatalf("AbortMultipart calls = %d, session = %q; want 1 and %q", store.abortCalls, result.UploadSessionID, rawID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadKeepsReservationWhenStorageCleanupFails(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	store := &cancelTestObjectStore{abortErr: errors.New("MinIO unavailable")}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "uploading")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`)).
		WithArgs(session.ID, "MinIO unavailable").WillReturnResult(sqlmock.NewResult(0, 1))

	_, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "storage_unavailable" {
		t.Fatalf("error = %v, want 503 storage_unavailable", err)
	}
	if store.abortCalls != 1 {
		t.Fatalf("AbortMultipart calls = %d, want 1", store.abortCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadWithoutObjectStoreDoesNotPanic(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	service := NewService(db, nil)

	expectCancelTransaction(mock, session, "uploading")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET last_error='object_store_unavailable',updated_at=now() WHERE upload_session_id=$1 AND status='cleanup_pending'`)).
		WithArgs(session.ID).WillReturnResult(sqlmock.NewResult(0, 1))

	_, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "storage_unavailable" {
		t.Fatalf("error = %v, want 503 storage_unavailable", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadKeepsReservationWhenObjectCheckFails(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	store := &cancelTestObjectStore{existsErr: errors.New("MinIO StatObject failed")}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "uploading")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`)).
		WithArgs(session.ID, "MinIO StatObject failed").WillReturnResult(sqlmock.NewResult(0, 1))

	_, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	assertStorageUnavailable(t, err)
	if store.abortCalls != 0 || store.removeCalls != 0 {
		t.Fatalf("cleanup calls = abort:%d remove:%d, want neither after failed object check", store.abortCalls, store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadCleansInitializingSessionAndReleasesReservation(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "initializing", nil)
	defer db.Close()
	store := &cancelTestObjectStore{}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "initializing")
	expectReservationRelease(mock, session, "cancelled")
	result, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	if err != nil {
		t.Fatalf("CancelMultipartUpload() error = %v", err)
	}
	if result.Status != "cancelled" || store.abortObjectCalls != 1 || store.removeCalls != 0 {
		t.Fatalf("result = %#v, cleanup calls = abort-object:%d remove:%d", result, store.abortObjectCalls, store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadCanCleanStaleCompletingSession(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "completing", nil)
	defer db.Close()
	store := &cancelTestObjectStore{exists: true}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "completing")
	expectReservationRelease(mock, session, "cancelled")
	result, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	if err != nil {
		t.Fatalf("CancelMultipartUpload() error = %v", err)
	}
	if result.Status != "cancelled" || store.abortCalls != 0 || store.removeCalls != 1 {
		t.Fatalf("result = %#v, cleanup calls = abort:%d remove:%d", result, store.abortCalls, store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadKeepsReservationWhenMergedObjectRemovalFails(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	store := &cancelTestObjectStore{exists: true, removeErr: errors.New("MinIO RemoveObject failed")}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "uploading")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='cleanup_pending',last_error=$2,updated_at=now() WHERE upload_session_id=$1`)).
		WithArgs(session.ID, "MinIO RemoveObject failed").WillReturnResult(sqlmock.NewResult(0, 1))

	_, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	assertStorageUnavailable(t, err)
	if store.abortCalls != 0 || store.removeCalls != 1 {
		t.Fatalf("cleanup calls = abort:%d remove:%d, want abort:0 remove:1", store.abortCalls, store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelMultipartUploadReturnsSuccessWithoutReloadingAfterRelease(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	store := &cancelTestObjectStore{exists: true}
	service := NewService(db, store)

	expectCancelTransaction(mock, session, "uploading")
	expectReservationRelease(mock, session, "cancelled")

	result, err := service.CancelMultipartUpload(context.Background(), actor, rawID)
	if err != nil {
		t.Fatalf("CancelMultipartUpload() error = %v", err)
	}
	if result.Status != "cancelled" || result.UploadSessionID != rawID {
		t.Fatalf("cancel result = %#v, want cancelled session %q", result, rawID)
	}
	if store.abortCalls != 0 || store.removeCalls != 1 {
		t.Fatalf("cleanup calls = abort:%d remove:%d, want abort:0 remove:1", store.abortCalls, store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadMultipartPartRejectsWrongLengthBeforeStorage(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	store := &cancelTestObjectStore{}
	service := NewService(db, store)
	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, "uploading", `FOR SHARE`)
	mock.ExpectRollback()

	_, err := service.UploadMultipartPart(context.Background(), actor, UploadPartInput{SessionID: rawID, PartNumber: 1, Reader: strings.NewReader("bad"), ContentLength: 3})
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 422 || problem.Code != "upload_part_invalid" {
		t.Fatalf("error = %v, want 422 upload_part_invalid", err)
	}
	if store.putPartCalls != 0 {
		t.Fatalf("PutMultipartPart calls = %d, want 0", store.putPartCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadMultipartPartStreamsExactBytesAndReportsProgress(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	session.Size = 6
	session.PartSize = 4
	session.PartCount = 2
	session.Reserved = 6
	store := &cancelTestObjectStore{partETag: "etag-1", parts: []minio.ObjectPart{{PartNumber: 1, ETag: "etag-1", Size: 4}}, objectSizeErr: errors.New("multipart object not completed")}
	service := NewService(db, store)
	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, "uploading", `FOR SHARE`)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO asset_upload_parts(upload_session_id,part_number,size_bytes,etag) VALUES($1,$2,$3,$4) ON CONFLICT(upload_session_id,part_number) DO UPDATE SET size_bytes=EXCLUDED.size_bytes,etag=EXCLUDED.etag,updated_at=now()`)).
		WithArgs(session.ID, 1, int64(4), "etag-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectLoadUploadSession(mock, session, "uploading", nil)

	result, err := service.UploadMultipartPart(context.Background(), actor, UploadPartInput{SessionID: rawID, PartNumber: 1, Reader: strings.NewReader("data"), ContentLength: 4})
	if err != nil {
		t.Fatalf("UploadMultipartPart() error = %v", err)
	}
	if !bytes.Equal(store.putPartBytes, []byte("data")) || result.ReceivedBytes != 4 || len(result.ReceivedParts) != 1 || result.ReceivedParts[0] != 1 {
		t.Fatalf("stored bytes = %q, result = %#v", store.putPartBytes, result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadMultipartPartRejectsShortBodyEvenIfStoreReturnsETag(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	session.Size = 6
	session.PartSize = 4
	session.PartCount = 2
	store := &cancelTestObjectStore{partETag: "etag-1"}
	service := NewService(db, store)
	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, "uploading", `FOR SHARE`)
	mock.ExpectRollback()

	_, err := service.UploadMultipartPart(context.Background(), actor, UploadPartInput{SessionID: rawID, PartNumber: 1, Reader: strings.NewReader("xx"), ContentLength: 4})
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 422 || problem.Code != "upload_part_invalid" {
		t.Fatalf("error = %v, want 422 upload_part_invalid", err)
	}
	if store.putPartCalls != 1 || len(store.putPartBytes) != 2 {
		t.Fatalf("storage calls = %d, bytes = %q, want one call and two bytes", store.putPartCalls, store.putPartBytes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadMultipartPartRejectsExtraBytesBeyondDeclaredLength(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	session.Size = 6
	session.PartSize = 4
	session.PartCount = 2
	store := &cancelTestObjectStore{partETag: "etag-1"}
	service := NewService(db, store)
	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, "uploading", `FOR SHARE`)
	mock.ExpectRollback()

	_, err := service.UploadMultipartPart(context.Background(), actor, UploadPartInput{SessionID: rawID, PartNumber: 1, Reader: strings.NewReader("dataX"), ContentLength: 4})
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 422 || problem.Code != "upload_part_invalid" {
		t.Fatalf("error = %v, want 422 upload_part_invalid", err)
	}
	if !bytes.Equal(store.putPartBytes, []byte("data")) {
		t.Fatalf("stored bytes = %q, want only the declared four-byte prefix", store.putPartBytes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteMultipartUploadDoesNotDeleteWhenPostMergeSizeCheckFails(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	storeErr := errors.New("temporary StatObject timeout")
	store := &cancelTestObjectStore{
		objectSizeErr: storeErr,
		parts:         []minio.ObjectPart{{PartNumber: 1, ETag: "part-1", Size: session.PartSize}, {PartNumber: 2, ETag: "part-2", Size: session.Size - session.PartSize}},
	}
	service := NewService(db, store)
	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, "uploading", `FOR UPDATE`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='completing',updated_at=now() WHERE upload_session_id=$1 AND status='uploading'`)).
		WithArgs(session.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	_, err := service.CompleteMultipartUpload(context.Background(), actor, rawID, Meta{})
	assertStorageUnavailable(t, err)
	if store.completeCalls != 1 || store.removeCalls != 0 {
		t.Fatalf("storage calls = complete:%d remove:%d, want complete:1 remove:0", store.completeCalls, store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteMultipartUploadMergesAndSettlesQuotaOnce(t *testing.T) {
	db, mock, session, actor, rawID := newCancelTestService(t, "uploading", nil)
	defer db.Close()
	actor.Role = "admin"
	store := &cancelTestObjectStore{
		objectSize:      session.Size,
		objectSizeSteps: []objectSizeResult{{err: errors.New("object not merged")}, {size: session.Size}},
		parts: []minio.ObjectPart{
			{PartNumber: 1, ETag: "part-1", Size: session.PartSize},
			{PartNumber: 2, ETag: "part-2", Size: session.Size - session.PartSize},
		},
	}
	service := NewService(db, store)

	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, "uploading", `FOR UPDATE`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='completing',updated_at=now() WHERE upload_session_id=$1 AND status='uploading'`)).
		WithArgs(session.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO assets(asset_id,workspace_id,owner_user_id,business_id,logical_path,bucket,object_key,original_name,content_type,size_bytes,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET storage_reserved_bytes=GREATEST(0,storage_reserved_bytes-$2),storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`)).
		WithArgs(session.OwnerID, session.Reserved).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='completed',asset_id=$2,api_quota_reserved=false,completed_at=$3,updated_at=$3 WHERE upload_session_id=$1`)).
		WithArgs(session.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO audit_logs\\(actor_user_id,workspace_id,event_type,target_type,target_id").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := service.CompleteMultipartUpload(context.Background(), actor, rawID, Meta{})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload() error = %v", err)
	}
	if result.Status != "completed" || result.Asset == nil || result.ReceivedBytes != session.Size || store.completeCalls != 1 {
		t.Fatalf("result = %#v, complete calls = %d", result, store.completeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func newCancelTestService(t *testing.T, status string, cancelledAt *time.Time) (*sql.DB, sqlmock.Sqlmock, uploadSessionRecord, Actor, string) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	session := uploadSessionRecord{
		ID: uuid.New(), WorkspaceID: uuid.New(), OwnerID: uuid.New(), BusinessID: "canvas",
		Bucket: "canvas", ObjectKey: "workspace/assets/upload", MinioUploadID: "minio-upload-id",
		FileName: "large.bin", ContentType: "application/octet-stream", LogicalPath: "uploads/large.bin",
		Size: 64 * 1024 * 1024, PartSize: 32 * 1024 * 1024, PartCount: 2, Reserved: 64 * 1024 * 1024,
		Status: status, ExpiresAt: time.Now().Add(time.Hour),
	}
	if cancelledAt != nil {
		session.CancelledAt = sql.NullTime{Time: *cancelledAt, Valid: true}
	}
	actor := Actor{UserID: session.OwnerID, WorkspaceID: session.WorkspaceID, Role: "user"}
	return db, mock, session, actor, publicID("upl", session.ID)
}

func expectLoadUploadSession(mock sqlmock.Sqlmock, session uploadSessionRecord, status string, cancelledAt *time.Time) {
	var cancelledValue any
	if cancelledAt != nil {
		cancelledValue = *cancelledAt
	}
	rows := sqlmock.NewRows([]string{
		"upload_session_id", "workspace_id", "owner_user_id", "business_id", "api_key_id", "bucket", "object_key", "minio_upload_id",
		"original_name", "content_type", "logical_path", "expected_size_bytes", "part_size_bytes", "part_count", "reserved_bytes",
		"status", "expires_at", "asset_id", "idempotency_key", "cancelled_at",
	}).AddRow(
		session.ID, session.WorkspaceID, session.OwnerID, session.BusinessID, nil, session.Bucket, session.ObjectKey, session.MinioUploadID,
		session.FileName, session.ContentType, session.LogicalPath, session.Size, session.PartSize, session.PartCount, session.Reserved,
		status, session.ExpiresAt, nil, "", cancelledValue,
	)
	mock.ExpectQuery("SELECT upload_session_id,workspace_id,owner_user_id,business_id").WithArgs(session.ID).WillReturnRows(rows)
}

func expectCancelTransaction(mock sqlmock.Sqlmock, session uploadSessionRecord, status string) {
	mock.ExpectBegin()
	expectUploadSessionQuery(mock, session, status, `FOR UPDATE`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET status='cleanup_pending',cancelled_at=COALESCE(cancelled_at,now()),updated_at=now() WHERE upload_session_id=$1 AND status IN ('initializing','uploading','completing','cleanup_pending')`)).
		WithArgs(session.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func expectUploadSessionQuery(mock sqlmock.Sqlmock, session uploadSessionRecord, status, suffix string) {
	var cancelledValue any
	if session.CancelledAt.Valid {
		cancelledValue = session.CancelledAt.Time
	}
	rows := sqlmock.NewRows([]string{
		"upload_session_id", "workspace_id", "owner_user_id", "business_id", "api_key_id", "bucket", "object_key", "minio_upload_id",
		"original_name", "content_type", "logical_path", "expected_size_bytes", "part_size_bytes", "part_count", "reserved_bytes",
		"status", "expires_at", "asset_id", "idempotency_key", "cancelled_at",
	}).AddRow(
		session.ID, session.WorkspaceID, session.OwnerID, session.BusinessID, nil, session.Bucket, session.ObjectKey, session.MinioUploadID,
		session.FileName, session.ContentType, session.LogicalPath, session.Size, session.PartSize, session.PartCount, session.Reserved,
		status, session.ExpiresAt, nil, "", cancelledValue,
	)
	mock.ExpectQuery("SELECT upload_session_id,workspace_id,owner_user_id,business_id.*" + regexp.QuoteMeta(suffix)).WithArgs(session.ID).WillReturnRows(rows)
}

func expectReservationRelease(mock sqlmock.Sqlmock, session uploadSessionRecord, status string) {
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT api_quota_reserved,status FROM asset_upload_sessions WHERE upload_session_id=$1 FOR UPDATE`)).
		WithArgs(session.ID).WillReturnRows(sqlmock.NewRows([]string{"api_quota_reserved", "status"}).AddRow(false, "cleanup_pending"))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET storage_reserved_bytes=GREATEST(0,storage_reserved_bytes-$2),updated_at=now() WHERE user_id=$1`)).
		WithArgs(session.OwnerID, session.Reserved).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE asset_upload_sessions SET api_quota_reserved=false,status=$2,cancelled_at=CASE WHEN $2='cancelled' THEN now() ELSE cancelled_at END,last_error=NULLIF($3,''),updated_at=now() WHERE upload_session_id=$1 AND status<>'completed'`)).
		WithArgs(session.ID, status, "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func assertStorageUnavailable(t *testing.T, err error) {
	t.Helper()
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "storage_unavailable" {
		t.Fatalf("error = %v, want 503 storage_unavailable", err)
	}
}

var _ assetObjectStore = (*cancelTestObjectStore)(nil)
