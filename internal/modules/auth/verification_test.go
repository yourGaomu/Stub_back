package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/yourGaomu/short_links/back/internal/config"
)

func newVerificationTestService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	// 池中只有一个连接；持锁后误用 s.db 会超时，不能绕过 tx 更新验证码。
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	return NewService(db, nil, config.Config{EmailCodeHMACKey: "test-email-code-hmac-key"}), mock
}

func expectVerification(mock sqlmock.Sqlmock, s *Service, id uuid.UUID, purpose string, attempts int) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`SELECT verification_id, code_hash, attempt_count FROM email_verification_codes .* LIMIT 1 FOR UPDATE`).
		WithArgs("user@example.com", purpose).
		WillReturnRows(sqlmock.NewRows([]string{"verification_id", "code_hash", "attempt_count"}).
			AddRow(id, s.emailCodeDigest("user@example.com", purpose, "123456"), attempts))
}

func TestValidateEmailCodeLeavesTransactionToCaller(t *testing.T) {
	for _, state := range []string{"valid", "wrong", "exhausted", "missing", "query_failure", "update_failure"} {
		t.Run(state, func(t *testing.T) {
			s, mock := newVerificationTestService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			mock.ExpectBegin()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			id := uuid.New()
			code, attempts := "123456", 0
			failure := errors.New("database unavailable")
			switch state {
			case "wrong", "update_failure":
				code = "000000"
			case "exhausted":
				attempts = 5
			}
			query := expectVerification(mock, s, id, "register", attempts)
			if state == "missing" {
				query.WillReturnError(sql.ErrNoRows)
			} else if state == "query_failure" {
				query.WillReturnError(failure)
			}
			if state == "wrong" || state == "update_failure" {
				update := mock.ExpectExec(`UPDATE email_verification_codes SET attempt_count = attempt_count \+ 1 WHERE verification_id = \$1`).WithArgs(id)
				if state == "update_failure" {
					update.WillReturnError(failure)
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			gotID, valid, err := s.validateEmailCode(ctx, tx, "user@example.com", "register", code)
			if state == "query_failure" || state == "update_failure" {
				if !errors.Is(err, failure) {
					t.Fatalf("error = %v, want database failure", err)
				}
			} else if err != nil || valid != (state == "valid") || valid && gotID != id || !valid && gotID != uuid.Nil {
				t.Fatalf("validation = %s, %v, %v", gotID, valid, err)
			}
			// 辅助函数返回后事务仍可由调用方结束。
			mock.ExpectRollback()
			if err := tx.Rollback(); err != nil {
				t.Fatalf("helper ended caller transaction: %v", err)
			}
		})
	}
}

func TestInvalidEmailCodeCommitsAttemptsBefore422(t *testing.T) {
	for _, purpose := range []string{"register", "password_reset"} {
		for _, state := range []string{"wrong", "fifth_wrong", "exhausted", "missing", "commit_failure", "update_failure"} {
			t.Run(purpose+"/"+state, func(t *testing.T) {
				s, mock := newVerificationTestService(t)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if purpose == "password_reset" {
					mock.ExpectQuery(`SELECT user_id,default_workspace_id FROM users WHERE email = \$1`).WithArgs("user@example.com").
						WillReturnRows(sqlmock.NewRows([]string{"user_id", "workspace_id"}).AddRow(uuid.New(), uuid.New()))
				}
				mock.ExpectBegin()
				id, attempts := uuid.New(), 0
				if state == "fifth_wrong" {
					attempts = 4
				} else if state == "exhausted" {
					attempts = 5
				}
				query := expectVerification(mock, s, id, purpose, attempts)
				if state == "missing" {
					query.WillReturnError(sql.ErrNoRows)
				}
				failure := errors.New("transaction failure")
				if state != "missing" && state != "exhausted" {
					update := mock.ExpectExec(`UPDATE email_verification_codes SET attempt_count = attempt_count \+ 1 WHERE verification_id = \$1`).WithArgs(id)
					if state == "update_failure" {
						update.WillReturnError(failure)
					} else {
						update.WillReturnResult(sqlmock.NewResult(0, 1))
					}
				}
				if state == "update_failure" {
					mock.ExpectRollback()
				} else if state == "commit_failure" {
					mock.ExpectCommit().WillReturnError(failure)
				} else {
					mock.ExpectCommit()
				}
				var err error
				if purpose == "register" {
					_, err = s.Register(ctx, RegisterRequest{Email: "user@example.com", Password: "new-password", VerificationCode: "000000"}, AuditMeta{})
				} else {
					err = s.PasswordReset(ctx, PasswordResetRequest{Email: "user@example.com", NewPassword: "new-password", VerificationCode: "000000"}, AuditMeta{})
				}
				if state == "commit_failure" || state == "update_failure" {
					if !errors.Is(err, failure) {
						t.Fatalf("error = %v, want transaction failure", err)
					}
				} else {
					var p *Problem
					if !errors.As(err, &p) || p.Status != 422 || p.Code != "invalid_or_expired_verification_code" {
						t.Fatalf("error = %v, want invalid verification code 422", err)
					}
				}
			})
		}
	}
}

func TestPasswordResetValidCodeAtomicWrites(t *testing.T) {
	for _, stage := range []string{"success", "password", "sessions", "consume", "audit", "commit"} {
		t.Run(stage, func(t *testing.T) {
			s, mock := newVerificationTestService(t)
			userID, workspaceID, verificationID := uuid.New(), uuid.New(), uuid.New()
			mock.ExpectQuery(`SELECT user_id,default_workspace_id FROM users WHERE email = \$1`).WithArgs("user@example.com").
				WillReturnRows(sqlmock.NewRows([]string{"user_id", "workspace_id"}).AddRow(userID, workspaceID))
			mock.ExpectBegin()
			expectVerification(mock, s, verificationID, "password_reset", 0)
			failure := errors.New("write failed")
			for _, write := range []struct{ name, query string }{
				{"password", `UPDATE users SET password_hash`},
				{"sessions", `UPDATE user_sessions SET revoked_at`},
				{"consume", `UPDATE email_verification_codes SET consumed_at`},
				{"audit", `INSERT INTO audit_logs`},
			} {
				exec := mock.ExpectExec(write.query)
				if stage == write.name {
					exec.WillReturnError(failure)
					break
				}
				exec.WillReturnResult(sqlmock.NewResult(0, 1))
			}
			if stage == "success" {
				mock.ExpectCommit()
			} else if stage == "commit" {
				mock.ExpectCommit().WillReturnError(failure)
			} else {
				mock.ExpectRollback()
			}
			err := s.PasswordReset(context.Background(), PasswordResetRequest{Email: "user@example.com", NewPassword: "password", VerificationCode: "123456"}, AuditMeta{})
			if stage == "success" && err != nil || stage != "success" && !errors.Is(err, failure) {
				t.Fatalf("PasswordReset = %v at %s", err, stage)
			}
		})
	}
}

func TestRegisterValidCodeAtomicWrites(t *testing.T) {
	for _, stage := range []string{"success", "user", "workspace", "permission", "consume", "audit", "commit"} {
		t.Run(stage, func(t *testing.T) {
			s, mock := newVerificationTestService(t)
			verificationID := uuid.New()
			mock.ExpectBegin()
			expectVerification(mock, s, verificationID, "register", 0)
			mock.ExpectQuery(`SELECT pg_advisory_xact_lock`).WithArgs("user@example.com").WillReturnRows(sqlmock.NewRows([]string{"lock"}).AddRow(nil))
			mock.ExpectQuery(`SELECT allow_public_registration, allow_invite_registration .* FOR SHARE`).
				WillReturnRows(sqlmock.NewRows([]string{"public", "invite"}).AddRow(true, false))
			failure := errors.New("registration write failed")
			writes := []struct{ name, query string }{{"user", `INSERT INTO users`}, {"workspace", `INSERT INTO workspaces`}}
			for range defaultPermissionCodes {
				writes = append(writes, struct{ name, query string }{"permission", `INSERT INTO user_permissions`})
			}
			writes = append(writes, struct{ name, query string }{"consume", `UPDATE email_verification_codes SET consumed_at`}, struct{ name, query string }{"audit", `INSERT INTO audit_logs`})
			for _, write := range writes {
				exec := mock.ExpectExec(write.query)
				if stage == write.name {
					exec.WillReturnError(failure)
					break
				}
				exec.WillReturnResult(sqlmock.NewResult(0, 1))
			}
			if stage == "success" {
				mock.ExpectCommit()
				mock.ExpectQuery(`SELECT user_id, email, role, email_verified_at, invited_by_user_id::text, default_workspace_id, created_at FROM users`).
					WillReturnRows(sqlmock.NewRows([]string{"id", "email", "role", "verified", "invited_by", "workspace", "created"}).
						AddRow(uuid.New(), "user@example.com", "user", time.Now(), nil, uuid.New(), time.Now()))
			} else if stage == "commit" {
				mock.ExpectCommit().WillReturnError(failure)
			} else {
				mock.ExpectRollback()
			}
			_, err := s.Register(context.Background(), RegisterRequest{Email: "user@example.com", Password: "password", VerificationCode: "123456"}, AuditMeta{})
			if stage == "success" && err != nil || stage != "success" && !errors.Is(err, failure) {
				t.Fatalf("Register = %v at %s", err, stage)
			}
		})
	}
}
