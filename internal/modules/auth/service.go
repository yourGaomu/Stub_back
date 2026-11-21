package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/yourGaomu/short_links/back/internal/config"
)

const (
	codeLifetime = 10 * time.Minute
	sessionTTL   = 7 * 24 * time.Hour
)

type Service struct {
	db     *sql.DB
	redis  *redis.Client
	cfg    config.Config
	mailer EmailSender
}

func NewService(db *sql.DB, redisClient *redis.Client, cfg config.Config) *Service {
	return &Service{db: db, redis: redisClient, cfg: cfg, mailer: newSMTPSender(cfg)}
}

func (s *Service) RequestEmailCode(ctx context.Context, input EmailCodeRequest, sourceIP string) (EmailCodeResponse, error) {
	// 1. 校验邮箱与用途，邀请码只允许用于注册验证码。
	email, err := normalizeEmail(input.Email)
	if err != nil || (input.Purpose != "register" && input.Purpose != "password_reset") || (input.InviteCode != "" && input.Purpose != "register") {
		return EmailCodeResponse{}, problem(422, "invalid_email_or_purpose", "邮箱或验证码用途无效")
	}

	// 2. 读取注册开关并预校验邀请码，但此阶段不核销邀请码。
	var allowPublic, allowInvite bool
	if err := s.db.QueryRowContext(ctx, `SELECT allow_public_registration, allow_invite_registration FROM system_settings WHERE settings_id = 1`).Scan(&allowPublic, &allowInvite); err != nil {
		return EmailCodeResponse{}, fmt.Errorf("读取注册设置失败: %w", err)
	}
	if input.Purpose == "register" {
		if input.InviteCode != "" {
			if !allowInvite || !s.inviteCanRegister(ctx, input.InviteCode, email) {
				return EmailCodeResponse{}, problem(422, "invalid_invite", "邀请码不可用")
			}
		} else if !allowPublic {
			return EmailCodeResponse{}, problem(422, "registration_unavailable", "当前不开放注册")
		}
	}

	// 3. 对已注册邮箱和不存在的重置邮箱统一返回 accepted，降低账号枚举风险。
	var accountExists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&accountExists); err != nil {
		return EmailCodeResponse{}, fmt.Errorf("查询邮箱状态失败: %w", err)
	}
	suppressed := (input.Purpose == "register" && accountExists) || (input.Purpose == "password_reset" && !accountExists)

	// 4. 使用 Redis 的过期计数执行邮件发送频率限制。
	if limited, err := s.emailRateLimited(ctx, email, sourceIP); err != nil {
		return EmailCodeResponse{}, fmt.Errorf("检查邮件发送限流失败: %w", err)
	} else if limited {
		return EmailCodeResponse{}, problem(429, "email_send_limited", "请求过于频繁，请稍后重试")
	}

	// 5. 对抑制发送的请求写入同形态限速记录，但不保存可用验证码。
	now := time.Now().UTC()
	if suppressed {
		if _, err := s.insertEmailCodeRecord(ctx, email, input.Purpose, nil, "suppressed", sourceIP, now); err != nil {
			return EmailCodeResponse{}, err
		}
		return emailCodeAccepted(now), nil
	}
	if s.mailer == nil {
		_, _ = s.insertEmailCodeRecord(ctx, email, input.Purpose, nil, "failed", sourceIP, now)
		return EmailCodeResponse{}, problem(503, "email_delivery_unavailable", "邮件服务暂不可用")
	}

	// 6. 先发出高熵随机验证码，成功后只将 HMAC 摘要写入数据库。
	code, err := generateNumericCode(6)
	if err != nil {
		return EmailCodeResponse{}, fmt.Errorf("生成验证码失败: %w", err)
	}
	// 6. 先写入不可验证的 pending 记录，再发信；SMTP 成功后才激活验证码。
	digest := s.emailCodeDigest(email, input.Purpose, code)
	verificationID, err := s.insertEmailCodeRecord(ctx, email, input.Purpose, &digest, "pending", sourceIP, now)
	if err != nil {
		return EmailCodeResponse{}, err
	}
	if err := s.mailer.SendVerificationCode(ctx, email, input.Purpose, code); err != nil {
		_ = s.setEmailCodeDelivery(ctx, verificationID, "failed")
		return EmailCodeResponse{}, problem(503, "email_delivery_unavailable", "邮件服务暂不可用")
	}
	if err := s.setEmailCodeDelivery(ctx, verificationID, "sent"); err != nil {
		return EmailCodeResponse{}, fmt.Errorf("激活邮箱验证码失败: %w", err)
	}
	return emailCodeAccepted(now), nil
}

func (s *Service) Register(ctx context.Context, input RegisterRequest, meta AuditMeta) (User, error) {
	// 1. 校验用户输入并在事务外计算慢速密码摘要。
	email, err := normalizeEmail(input.Email)
	if err != nil || input.VerificationCode == "" || len(input.Password) < 1 || len(input.Password) > 1024 {
		return User{}, problem(422, "invalid_request", "注册参数无效")
	}
	passwordHash, err := HashPassword(input.Password)
	if err != nil {
		return User{}, err
	}

	// 2. 锁定并校验邮箱验证码；失败次数与消费状态在同一事务处理。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("开始注册事务失败: %w", err)
	}
	defer tx.Rollback()
	verificationID, valid, err := s.validateEmailCode(ctx, tx, email, "register", input.VerificationCode)
	if err != nil {
		return User{}, err
	}
	if !valid {
		if err := tx.Commit(); err != nil {
			return User{}, fmt.Errorf("提交验证码校验失败: %w", err)
		}
		return User{}, problem(422, "invalid_or_expired_verification_code", "验证码无效或已过期")
	}
	var registrationLock any
	if err := tx.QueryRowContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, email).Scan(&registrationLock); err != nil {
		return User{}, fmt.Errorf("获取注册邮箱锁失败: %w", err)
	}

	// 3. 在事务内解析注册渠道、邀请码角色和邀请归属。
	var allowPublic, allowInvite bool
	if err := tx.QueryRowContext(ctx, `SELECT allow_public_registration, allow_invite_registration FROM system_settings WHERE settings_id = 1 FOR SHARE`).Scan(&allowPublic, &allowInvite); err != nil {
		return User{}, fmt.Errorf("读取注册设置失败: %w", err)
	}
	role := "user"
	var invitedBy *uuid.UUID
	var inviteID *uuid.UUID
	if input.InviteCode != "" {
		if !allowInvite {
			return User{}, problem(422, "registration_unavailable", "当前不开放邀请码注册")
		}
		invite, err := s.lockInvite(ctx, tx, input.InviteCode)
		if err != nil {
			return User{}, err
		}
		if invite.BoundEmail.Valid && invite.BoundEmail.String != email {
			return User{}, problem(409, "invite_unavailable", "邀请码不可用")
		}
		role, invitedBy, inviteID = invite.Role, &invite.CreatorID, &invite.ID
	} else if !allowPublic {
		return User{}, problem(422, "registration_unavailable", "当前不开放注册")
	}

	// 4. 原子创建账号、默认工作空间、普通用户权限和邀请码核销记录。
	now := time.Now().UTC()
	userID, workspaceID := uuid.New(), uuid.New()
	var invitedByValue any
	if invitedBy != nil {
		invitedByValue = *invitedBy
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (user_id, email, password_hash, role, email_verified_at, invited_by_user_id, default_workspace_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $5, $5)`, userID, email, passwordHash, role, now, invitedByValue, workspaceID); err != nil {
		if isUniqueViolation(err) {
			return User{}, problem(409, "account_registration_conflict", "无法完成注册，请检查验证码或更换邮箱")
		}
		return User{}, fmt.Errorf("创建用户失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces (workspace_id, owner_user_id, name, created_at, updated_at) VALUES ($1, $2, '默认空间', $3, $3)`, workspaceID, userID, now); err != nil {
		return User{}, fmt.Errorf("创建默认工作空间失败: %w", err)
	}
	if role == "user" {
		if err := createDefaultUserPermissions(ctx, tx, userID); err != nil {
			return User{}, err
		}
	}
	if inviteID != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE invite_codes SET redeemed_at = $2, redeemed_by_user_id = $3 WHERE invite_code_id = $1 AND redeemed_at IS NULL AND revoked_at IS NULL`, *inviteID, now, userID); err != nil {
			return User{}, fmt.Errorf("核销邀请码失败: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE email_verification_codes SET consumed_at = $2 WHERE verification_id = $1`, verificationID, now); err != nil {
		return User{}, fmt.Errorf("消费邮箱验证码失败: %w", err)
	}
	registerDetails := map[string]any{"role": role, "registeredViaInvite": inviteID != nil}
	if inviteID != nil {
		registerDetails["inviteId"] = publicID("inv", *inviteID)
		registerDetails["invitedByUserId"] = publicID("usr", *invitedBy)
		if err := writeAuditMeta(ctx, tx, &userID, &workspaceID, "invite.redeem", "invite", inviteID.String(), "success", meta, map[string]any{
			"assignedRole":     role,
			"redeemedByUserId": publicID("usr", userID),
		}); err != nil {
			return User{}, err
		}
	}
	if err := writeAuditMeta(ctx, tx, &userID, &workspaceID, "auth.register", "user", userID.String(), "success", meta, registerDetails); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("提交用户注册失败: %w", err)
	}
	return s.GetUser(ctx, userID)
}

func (s *Service) Login(ctx context.Context, input LoginRequest, clientAppID string, meta AuditMeta) (LoginResponse, error) {
	// 1. 校验客户端应用标识与凭据格式，统一返回账号密码错误。
	email, err := normalizeEmail(input.Email)
	if err != nil || !validClientApp(clientAppID) || input.Password == "" {
		if err != nil {
			email = ""
		}
		if !validClientApp(clientAppID) {
			return s.loginFailure(ctx, email, clientAppID, "invalid_client_app", 422, "invalid_client_app", "客户端应用标识无效", meta)
		}
		return s.loginFailure(ctx, email, clientAppID, "invalid_credentials", 401, "invalid_credentials", "邮箱或密码错误", meta)
	}
	var userID, workspaceID uuid.UUID
	var role, passwordHash string
	var emailVerifiedAt, createdAt time.Time
	var invitedBy sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT user_id, default_workspace_id, email, password_hash, role, email_verified_at, invited_by_user_id::text, created_at
		FROM users WHERE email = $1`, email).Scan(&userID, &workspaceID, &email, &passwordHash, &role, &emailVerifiedAt, &invitedBy, &createdAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !VerifyPassword(input.Password, passwordHash)) {
		return s.loginFailure(ctx, email, clientAppID, "invalid_credentials", 401, "invalid_credentials", "邮箱或密码错误", meta)
	}
	if err != nil {
		return LoginResponse{}, fmt.Errorf("查询登录账号失败: %w", err)
	}
	if emailVerifiedAt.IsZero() {
		return s.loginFailure(ctx, email, clientAppID, "email_not_verified", 403, "email_not_verified", "请先完成邮箱验证", meta)
	}

	// 2. 签发不可猜测的 Bearer Token，数据库仅保存 SHA-256 摘要。
	token, err := randomToken(32)
	if err != nil {
		return LoginResponse{}, fmt.Errorf("生成登录令牌失败: %w", err)
	}
	now := time.Now().UTC()
	sessionID := uuid.New()
	deviceInstanceID, deviceName, platform := nullableTrim(input.DeviceInstanceID), nullableTrim(input.DeviceName), nullableTrim(input.Platform)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoginResponse{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_sessions (session_id, user_id, token_hash, client_app_id, device_instance_id, device_name, platform, source_ip, user_agent, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::inet, $9, $10, $10, $11)`,
		sessionID, userID, sha256Hex(token), clientAppID, deviceInstanceID, deviceName, platform, meta.SourceIP, meta.UserAgent, now, now.Add(sessionTTL))
	if err != nil {
		return LoginResponse{}, fmt.Errorf("创建登录会话失败: %w", err)
	}
	if err := writeAuditMeta(ctx, tx, &userID, &workspaceID, "auth.login", "session", sessionID.String(), "success", meta); err != nil {
		return LoginResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return LoginResponse{}, err
	}
	user := User{
		ID: userID, UserID: publicID("usr", userID), Email: email, Role: role,
		EmailVerifiedAt: emailVerifiedAt.UTC(), DefaultWorkspaceID: publicID("ws", workspaceID), CreatedAt: createdAt.UTC(),
	}
	if invitedBy.Valid {
		value := "usr_" + invitedBy.String
		user.InvitedByUserID = &value
	}
	return LoginResponse{
		AccessToken: token, TokenType: "Bearer", ExpiresAt: now.Add(sessionTTL), User: user,
		Session: Session{SessionID: publicID("ses", sessionID), ClientAppID: clientAppID,
			DeviceInstanceID: stringPtr(deviceInstanceID), DeviceName: stringPtr(deviceName), Platform: stringPtr(platform),
			CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(sessionTTL), IsCurrent: true, Online: true},
	}, nil
}

func (s *Service) loginFailure(ctx context.Context, email, clientAppID, failureCode string, status int, code, message string, meta AuditMeta) (LoginResponse, error) {
	// 1. 认证失败先写长期审计，再统一返回对应错误；凭据本身永不入库。
	if len(clientAppID) > 64 {
		clientAppID = clientAppID[:64]
	}
	details, err := json.Marshal(map[string]string{"failureCode": failureCode, "clientAppId": clientAppID})
	if err != nil {
		return LoginResponse{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_logs(event_type,target_type,target_id,occurred_at,source_ip,user_agent,result,details,request_id) VALUES('auth.login.failed','user_email',NULLIF($1,''),now(),NULLIF($2,'')::inet,$3,'failure',$4::jsonb,NULLIF($5,''))`, email, meta.SourceIP, meta.UserAgent, string(details), meta.RequestID)
	if err != nil {
		return LoginResponse{}, fmt.Errorf("记录登录失败审计事件失败: %w", err)
	}
	return LoginResponse{}, problem(status, code, message)
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, string, error) {
	// 1. 只使用 Token 摘要查找未撤销且未过期的会话。
	if token == "" {
		return User{}, "", problem(401, "unauthorized", "请先登录")
	}
	var userID, workspaceID, sessionID uuid.UUID
	var email, role string
	var verifiedAt, createdAt time.Time
	var invitedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT u.user_id, u.default_workspace_id, s.session_id, u.email, u.role, u.email_verified_at, u.invited_by_user_id::text, u.created_at
		FROM user_sessions s JOIN users u ON u.user_id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`, sha256Hex(token)).
		Scan(&userID, &workspaceID, &sessionID, &email, &role, &verifiedAt, &invitedBy, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", problem(401, "session_expired_or_revoked", "登录已失效，请重新登录")
	}
	if err != nil {
		return User{}, "", fmt.Errorf("验证登录会话失败: %w", err)
	}
	user := User{ID: userID, UserID: publicID("usr", userID), Email: email, Role: role,
		EmailVerifiedAt: verifiedAt.UTC(), DefaultWorkspaceID: publicID("ws", workspaceID), CreatedAt: createdAt.UTC()}
	if invitedBy.Valid {
		value := "usr_" + invitedBy.String
		user.InvitedByUserID = &value
	}
	return user, sessionID.String(), nil
}

func (s *Service) Logout(ctx context.Context, userID uuid.UUID, sessionID string, meta AuditMeta) error {
	// 1. 仅撤销当前已认证会话，保持其他设备登录状态不变。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workspaceID uuid.UUID
	err = tx.QueryRowContext(ctx, `UPDATE user_sessions s SET revoked_at=now() FROM users u WHERE s.session_id=$1 AND s.user_id=$2 AND s.user_id=u.user_id AND s.revoked_at IS NULL RETURNING u.default_workspace_id`, sessionID, userID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(401, "session_expired_or_revoked", "登录已失效，请重新登录")
	}
	if err != nil {
		return err
	}
	if err := writeAuditMeta(ctx, tx, &userID, &workspaceID, "auth.logout", "session", sessionID, "success", meta); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	var user User
	var workspace uuid.UUID
	var invitedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, email, role, email_verified_at, invited_by_user_id::text, default_workspace_id, created_at
		FROM users WHERE user_id = $1`, id).
		Scan(&user.ID, &user.Email, &user.Role, &user.EmailVerifiedAt, &invitedBy, &workspace, &user.CreatedAt)
	if err != nil {
		return User{}, err
	}
	user.UserID = publicID("usr", user.ID)
	user.DefaultWorkspaceID = publicID("ws", workspace)
	user.EmailVerifiedAt = user.EmailVerifiedAt.UTC()
	user.CreatedAt = user.CreatedAt.UTC()
	if invitedBy.Valid {
		value := "usr_" + invitedBy.String
		user.InvitedByUserID = &value
	}
	return user, nil
}

func (s *Service) Heartbeat(ctx context.Context, userID uuid.UUID, sessionID string) (Session, error) {
	// 1. 仅延长在线状态心跳，不延长固定的 7 天 Token 生命周期。
	var session Session
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx, `
		UPDATE user_sessions SET last_seen_at = now()
		WHERE session_id = $1 AND user_id = $2 AND revoked_at IS NULL AND expires_at > now()
		RETURNING session_id, client_app_id, device_instance_id, device_name, platform, created_at, last_seen_at, expires_at`, sessionID, userID).
		Scan(&id, &session.ClientAppID, &session.DeviceInstanceID, &session.DeviceName, &session.Platform,
			&session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, problem(404, "session_not_found", "会话不存在")
	}
	if err != nil {
		return Session{}, fmt.Errorf("更新会话心跳失败: %w", err)
	}
	session.SessionID = publicID("ses", id)
	session.IsCurrent = true
	session.Online = true
	return session, nil
}

func (s *Service) RevokeSession(ctx context.Context, actorID, userID uuid.UUID, sessionID string, meta AuditMeta) error {
	// 1. 使用用户 ID 限定更新范围，不允许通过猜测会话 ID 撤销他人会话。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workspaceID uuid.UUID
	err = tx.QueryRowContext(ctx, `UPDATE user_sessions s SET revoked_at=COALESCE(s.revoked_at,now()) FROM users u WHERE s.session_id=$1 AND s.user_id=$2 AND s.user_id=u.user_id RETURNING u.default_workspace_id`, sessionID, userID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "session_not_found", "会话不存在")
	}
	if err != nil {
		return fmt.Errorf("撤销会话失败: %w", err)
	}
	if err := writeAuditMeta(ctx, tx, &actorID, &workspaceID, "session.revoke", "session", sessionID, "success", meta); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID, currentID string, page, pageSize int) ([]Session, int64, error) {
	// 1. 分别查询总数和分页结果；会话始终限制在当前用户范围内。
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM user_sessions WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, client_app_id, device_instance_id, device_name, platform, created_at, last_seen_at, expires_at, revoked_at
		FROM user_sessions WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, userID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]Session, 0)
	for rows.Next() {
		var id uuid.UUID
		var item Session
		if err := rows.Scan(&id, &item.ClientAppID, &item.DeviceInstanceID, &item.DeviceName, &item.Platform,
			&item.CreatedAt, &item.LastSeenAt, &item.ExpiresAt, &item.RevokedAt); err != nil {
			return nil, 0, err
		}
		item.SessionID = publicID("ses", id)
		item.IsCurrent = id.String() == currentID
		item.Online = item.RevokedAt == nil && item.ExpiresAt.After(time.Now()) && item.LastSeenAt.After(time.Now().Add(-10*time.Minute))
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (s *Service) PasswordReset(ctx context.Context, input PasswordResetRequest, meta AuditMeta) error {
	// 1. 对不存在的邮箱也返回成功，避免密码重置接口枚举账号。
	email, err := normalizeEmail(input.Email)
	if err != nil || input.VerificationCode == "" || input.NewPassword == "" || len(input.NewPassword) > 1024 {
		return problem(422, "invalid_request", "重置参数无效")
	}
	var userID, workspaceID uuid.UUID
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,default_workspace_id FROM users WHERE email = $1`, email).Scan(&userID, &workspaceID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	hash, err := HashPassword(input.NewPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	verificationID, valid, err := s.validateEmailCode(ctx, tx, email, "password_reset", input.VerificationCode)
	if err != nil {
		return err
	}
	if !valid {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交验证码校验失败: %w", err)
		}
		return problem(422, "invalid_or_expired_verification_code", "验证码无效或已过期")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = $2, updated_at = $3 WHERE user_id = $1`, userID, hash, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE email_verification_codes SET consumed_at = $2 WHERE verification_id = $1`, verificationID, now); err != nil {
		return err
	}
	if err := writeAuditMeta(ctx, tx, &userID, &workspaceID, "auth.password_reset", "user", userID.String(), "success", meta); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) validateEmailCode(ctx context.Context, tx *sql.Tx, email, purpose, code string) (uuid.UUID, bool, error) {
	// 1. 只在调用方事务内锁定并比较验证码；事务提交或回滚由业务入口负责。
	var id uuid.UUID
	var digest string
	var attempts int
	err := tx.QueryRowContext(ctx, `
		SELECT verification_id, code_hash, attempt_count FROM email_verification_codes
		WHERE email = $1 AND purpose = $2 AND delivery_status = 'sent' AND consumed_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, email, purpose).Scan(&id, &digest, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	if attempts >= 5 {
		return uuid.Nil, false, nil
	}
	provided := s.emailCodeDigest(email, purpose, code)
	if !hmac.Equal([]byte(digest), []byte(provided)) {
		if _, err := tx.ExecContext(ctx, `UPDATE email_verification_codes SET attempt_count = attempt_count + 1 WHERE verification_id = $1`, id); err != nil {
			return uuid.Nil, false, err
		}
		return uuid.Nil, false, nil
	}
	return id, true, nil
}

type lockedInvite struct {
	ID         uuid.UUID
	CreatorID  uuid.UUID
	Role       string
	BoundEmail sql.NullString
}

func (s *Service) lockInvite(ctx context.Context, tx *sql.Tx, code string) (lockedInvite, error) {
	// 1. 邀请码摘要查询并加行锁，保证兑换和撤销不会并发双重成功。
	var invite lockedInvite
	err := tx.QueryRowContext(ctx, `
		SELECT invite_code_id, created_by_user_id, assigned_role, bound_email
		FROM invite_codes WHERE code_hash = $1 AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > now() FOR UPDATE`,
		s.secretDigest(s.cfg.ShortCodeHMACKey, code)).
		Scan(&invite.ID, &invite.CreatorID, &invite.Role, &invite.BoundEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedInvite{}, problem(409, "invite_unavailable", "邀请码不可用")
	}
	if err != nil {
		return lockedInvite{}, err
	}
	return invite, nil
}

func (s *Service) inviteCanRegister(ctx context.Context, code, email string) bool {
	var boundEmail sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT bound_email FROM invite_codes WHERE code_hash = $1 AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		s.secretDigest(s.cfg.ShortCodeHMACKey, code)).Scan(&boundEmail)
	return err == nil && (!boundEmail.Valid || boundEmail.String == email)
}

func (s *Service) emailRateLimited(ctx context.Context, email, sourceIP string) (bool, error) {
	// 1. 每邮箱 60 秒冷却、每邮箱每小时 5 次、每 IP 每小时 20 次。
	mailKey := s.secretDigest(s.cfg.EmailCodeHMACKey, email)
	ipKey := s.secretDigest(s.cfg.EmailCodeHMACKey, sourceIP)
	result, err := emailRateLimitScript.Run(ctx, s.redis,
		[]string{"email-code:cooldown:" + mailKey, "email-code:hourly:email:" + mailKey, "email-code:hourly:ip:" + ipKey},
		1, 60, 5, 3600, 20, 3600).Int()
	return result == 1, err
}

var emailRateLimitScript = redis.NewScript(`
local limited = 0
for i = 1, #KEYS do
  local count = redis.call('INCR', KEYS[i])
  local max = tonumber(ARGV[(i - 1) * 2 + 1])
  local ttl = tonumber(ARGV[(i - 1) * 2 + 2])
  if count == 1 then redis.call('EXPIRE', KEYS[i], ttl) end
  if count > max then limited = 1 end
end
return limited
`)

func (s *Service) insertEmailCodeRecord(ctx context.Context, email, purpose string, digest *string, status, sourceIP string, now time.Time) (uuid.UUID, error) {
	// 1. 用一个事务废止旧验证码并保存本次发送结果，避免同邮箱并行保留多个有效码。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback()
	if digest != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE email_verification_codes SET consumed_at = $3
			WHERE email = $1 AND purpose = $2 AND consumed_at IS NULL AND delivery_status = 'sent'`, email, purpose, now); err != nil {
			return uuid.Nil, err
		}
	}
	verificationID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO email_verification_codes (verification_id, email, purpose, code_hash, delivery_status, created_at, expires_at, source_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::inet)`, verificationID, email, purpose, digest, status, now, now.Add(codeLifetime), sourceIP); err != nil {
		return uuid.Nil, fmt.Errorf("写入邮箱验证码记录失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return verificationID, nil
}

func (s *Service) setEmailCodeDelivery(ctx context.Context, id uuid.UUID, status string) error {
	// 1. 仅允许 pending 转换到 sent 或 failed，避免重复请求重新激活旧码。
	if status == "failed" {
		_, err := s.db.ExecContext(ctx, `UPDATE email_verification_codes SET delivery_status='failed',code_hash=NULL WHERE verification_id=$1 AND delivery_status='pending'`, id)
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE email_verification_codes SET delivery_status='sent' WHERE verification_id=$1 AND delivery_status='pending' AND consumed_at IS NULL`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("验证码记录已失效，不能激活")
	}
	return nil
}

func (s *Service) emailCodeDigest(email, purpose, code string) string {
	return s.secretDigest(s.cfg.EmailCodeHMACKey, email+"\x00"+purpose+"\x00"+code)
}

func (s *Service) secretDigest(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func emailCodeAccepted(now time.Time) EmailCodeResponse {
	return EmailCodeResponse{Accepted: true, ExpiresInSeconds: int(codeLifetime.Seconds()), ResendAvailableAt: now.Add(time.Minute)}
}

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || len(value) > 320 {
		return "", errors.New("invalid email")
	}
	return value, nil
}

func generateNumericCode(length int) (string, error) {
	// 1. 每一位通过 crypto/rand 的无偏采样生成。
	var result strings.Builder
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		result.WriteByte(byte('0' + n.Int64()))
	}
	return result.String(), nil
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validClientApp(value string) bool {
	switch value {
	case "canvas_app", "shortlink_web", "shortlink_app":
		return true
	default:
		return false
	}
}

func nullableTrim(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return strings.TrimSpace(*value)
}

func stringPtr(value any) *string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

func publicID(prefix string, id uuid.UUID) string { return prefix + "_" + id.String() }

func problem(status int, code, message string) error {
	return &Problem{Status: status, Code: code, Message: message}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
