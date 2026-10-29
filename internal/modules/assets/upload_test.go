package assets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

type singleUploadTestStore struct {
	assetObjectStore
	putErr      error
	putBytes    []byte
	contentType string
	removeErr   error
	removeCalls int
}

func (s *singleUploadTestStore) Put(_ context.Context, _, _, contentType string, body io.Reader, size int64) error {
	s.contentType = contentType
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.putBytes = data
	if int64(len(data)) != size {
		return errors.New("object size mismatch")
	}
	return s.putErr
}

func (s *singleUploadTestStore) Remove(context.Context, string, string) error {
	s.removeCalls++
	return s.removeErr
}

func TestUploadStoresBytesAndCommitsQuotaWithAsset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &singleUploadTestStore{}
	service := NewService(db, store)
	userID, workspaceID := uuid.New(), uuid.New()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT max_single_file_bytes FROM users WHERE user_id=$1`)).
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"max_single_file_bytes"}).AddRow(100))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users WHERE user_id=$1 FOR UPDATE`)).
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"role", "max_single_file_bytes", "storage_used_bytes", "storage_reserved_bytes", "storage_quota_bytes"}).AddRow("user", 100, 0, 0, 1000))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT bucket_name FROM business_integrations WHERE business_id=$1 AND status='active'`)).
		WithArgs("canvas").WillReturnRows(sqlmock.NewRows([]string{"bucket_name"}).AddRow("canvas-bucket"))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM user_business_access").
		WithArgs(userID, "canvas").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`)).
		WithArgs(userID, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO assets(asset_id,workspace_id,owner_user_id,business_id,logical_path,bucket,object_key,original_name,content_type,size_bytes,checksum_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO audit_logs\\(actor_user_id,workspace_id,event_type,target_type,target_id").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	file := makeTestFileHeader(t, "payload.bin", []byte("payload"))
	result, err := service.Upload(context.Background(), Actor{UserID: userID, WorkspaceID: workspaceID}, UploadInput{BusinessID: "canvas", Path: "uploads", File: file}, Meta{})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.SizeBytes != 7 || result.BusinessID != "canvas" || result.Path != "uploads" {
		t.Fatalf("upload result = %#v", result)
	}
	if !bytes.Equal(store.putBytes, []byte("payload")) {
		t.Fatalf("stored bytes = %q, want payload", store.putBytes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadStorageFailureRollsBackQuotaTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &singleUploadTestStore{putErr: errors.New("MinIO unavailable")}
	service := NewService(db, store)
	userID := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT max_single_file_bytes FROM users WHERE user_id=$1`)).
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"max_single_file_bytes"}).AddRow(100))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users WHERE user_id=$1 FOR UPDATE`)).
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"role", "max_single_file_bytes", "storage_used_bytes", "storage_reserved_bytes", "storage_quota_bytes"}).AddRow("user", 100, 0, 0, 1000))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT bucket_name FROM business_integrations WHERE business_id=$1 AND status='active'`)).
		WithArgs("canvas").WillReturnRows(sqlmock.NewRows([]string{"bucket_name"}).AddRow("canvas-bucket"))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM user_business_access").
		WithArgs(userID, "canvas").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`)).
		WithArgs(userID, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	_, err = service.Upload(context.Background(), Actor{UserID: userID, WorkspaceID: uuid.New()}, UploadInput{BusinessID: "canvas", Path: "uploads", File: makeTestFileHeader(t, "payload.bin", []byte("payload"))}, Meta{})
	var problem *Problem
	if !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "storage_unavailable" {
		t.Fatalf("error = %v, want 503 storage_unavailable", err)
	}
	if store.removeCalls != 1 {
		t.Fatalf("Remove calls = %d, want 1 cleanup attempt", store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadRemovesObjectWhenAssetRegistrationFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &singleUploadTestStore{}
	service := NewService(db, store)
	userID := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT max_single_file_bytes FROM users WHERE user_id=$1`)).
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"max_single_file_bytes"}).AddRow(100))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users WHERE user_id=$1 FOR UPDATE`)).
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"role", "max_single_file_bytes", "storage_used_bytes", "storage_reserved_bytes", "storage_quota_bytes"}).AddRow("user", 100, 0, 0, 1000))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT bucket_name FROM business_integrations WHERE business_id=$1 AND status='active'`)).
		WithArgs("canvas").WillReturnRows(sqlmock.NewRows([]string{"bucket_name"}).AddRow("canvas-bucket"))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM user_business_access").
		WithArgs(userID, "canvas").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET storage_used_bytes=storage_used_bytes+$2,updated_at=now() WHERE user_id=$1`)).
		WithArgs(userID, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO assets(asset_id,workspace_id,owner_user_id,business_id,logical_path,bucket,object_key,original_name,content_type,size_bytes,checksum_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`)).
		WillReturnError(errors.New("asset insert failed"))
	mock.ExpectRollback()

	_, err = service.Upload(context.Background(), Actor{UserID: userID, WorkspaceID: uuid.New()}, UploadInput{BusinessID: "canvas", Path: "uploads", File: makeTestFileHeader(t, "payload.bin", []byte("payload"))}, Meta{})
	if err == nil || !strings.Contains(err.Error(), "asset insert failed") {
		t.Fatalf("error = %v, want asset insert failure", err)
	}
	if store.removeCalls != 1 {
		t.Fatalf("Remove calls = %d, want 1 compensation attempt", store.removeCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginMultipartUploadRejectsInvalidMetadataBeforeDatabase(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := NewService(db, nil)
	actor := Actor{UserID: uuid.New(), WorkspaceID: uuid.New(), Role: "user", BusinessID: "canvas"}
	tests := []struct {
		name  string
		input BeginUploadInput
		code  string
	}{
		{name: "missing business", input: BeginUploadInput{SizeBytes: 1, FileName: "a.bin"}, code: "invalid_upload"},
		{name: "wrong API key business", input: BeginUploadInput{BusinessID: "other", SizeBytes: 1, FileName: "a.bin"}, code: "api_key_business_denied"},
		{name: "invalid path", input: BeginUploadInput{BusinessID: "canvas", Path: "../private", SizeBytes: 1, FileName: "a.bin"}, code: "invalid_path"},
		{name: "invalid filename", input: BeginUploadInput{BusinessID: "canvas", SizeBytes: 1, FileName: "../a.bin"}, code: "invalid_file_name"},
		{name: "oversized idempotency key", input: BeginUploadInput{BusinessID: "canvas", SizeBytes: 1, FileName: "a.bin", IdempotencyKey: string(bytes.Repeat([]byte{'k'}, 129))}, code: "invalid_idempotency_key"},
		{name: "too many parts", input: BeginUploadInput{BusinessID: "canvas", SizeBytes: 10001 * multipartPartSize, FileName: "a.bin"}, code: "file_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.BeginMultipartUpload(context.Background(), actor, test.input)
			var problem *Problem
			if !errors.As(err, &problem) || problem.Code != test.code {
				t.Fatalf("error = %v, want code %q", err, test.code)
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func makeTestFileHeader(t *testing.T, name string, data []byte) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if err := request.ParseMultipartForm(8 << 20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = request.MultipartForm.RemoveAll() })
	return request.MultipartForm.File["file"][0]
}
