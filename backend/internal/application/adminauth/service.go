package adminauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidCredentials = errors.New("管理员账号或密码错误")
	ErrInvalidSession     = errors.New("管理员会话无效")
	ErrBootstrapRequired  = errors.New("首次启动需要设置管理员账号和密码")
	ErrInvalidPassword    = errors.New("新密码长度必须在 8 到 72 字节之间")
	ErrLoginRateLimited   = errors.New("管理员登录尝试过于频繁")
	ErrRuntimeUnavailable = errors.New("管理员认证运行态暂不可用")
)

// LoginRateLimitedError 携带限流器返回的固定窗口剩余时间，供传输层
// 写出 Retry-After；errors.Is(err, ErrLoginRateLimited) 语义保持不变。
type LoginRateLimitedError struct{ RetryAfter time.Duration }

func (e *LoginRateLimitedError) Error() string { return ErrLoginRateLimited.Error() }
func (e *LoginRateLimitedError) Unwrap() error { return ErrLoginRateLimited }

// maxPasswordBytes 与 bcrypt 的硬上限对齐（x/crypto v0.55 起 Generate
// -FromPassword 对超长密码返回 ErrPasswordTooLong 而非静默截断）。
// 超限必须在服务层显式拒绝并归类为 ErrInvalidPassword，否则传输层
// 会把 bcrypt 的原始错误落到 500 分支（round 61 活体复现）。
const maxPasswordBytes = 72

func validPasswordLength(password string) bool {
	return len(password) >= minPasswordBytes && len(password) <= maxPasswordBytes
}

const minPasswordBytes = 8

// refreshRotationGrace：轮换重用判定的宽限窗。窗内旧 token 再次呈上
// 视为良性重复刷新（同 token 网络重试、多标签页并发刷新竞速）——不吊销
// 家族；超窗重用才是窃取信号（OAuth BCP RFC 6819 §5.2.11），吊销整个
// token family。判据是 Rotate 落写的 last_used_at。
const refreshRotationGrace = 30 * time.Second

type Tokens struct {
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
}

// Service 负责编排单管理员登录、JWT 和 refresh session 生命周期。
type Service struct {
	admins            repository.AdminRepository
	sessions          repository.AdminSessionRepository
	tokens            *security.TokenService
	accessTTL         time.Duration
	refreshTTL        time.Duration
	loginLimiter      repository.RateLimiter
	dummyPasswordHash string
}

func NewService(admins repository.AdminRepository, sessions repository.AdminSessionRepository, tokens *security.TokenService, accessTTL, refreshTTL time.Duration) *Service {
	dummyHash, _ := security.HashPassword("grok2api-invalid-admin-password")
	return &Service{admins: admins, sessions: sessions, tokens: tokens, accessTTL: accessTTL, refreshTTL: refreshTTL, dummyPasswordHash: dummyHash}
}

func (s *Service) SetLoginRateLimiter(limiter repository.RateLimiter) { s.loginLimiter = limiter }

// Bootstrap 在数据库没有管理员时创建唯一管理员。
func (s *Service) Bootstrap(ctx context.Context, username, password string) error {
	count, err := s.admins.Count(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if strings.TrimSpace(username) == "" || !validPasswordLength(password) {
		return ErrBootstrapRequired
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.admins.Create(ctx, admin.Admin{Username: strings.TrimSpace(username), PasswordHash: hash})
	return err
}

// Login 校验密码并创建新的可撤销 refresh session。
func (s *Service) Login(ctx context.Context, username, password, remoteAddress string) (admin.Admin, Tokens, error) {
	username = strings.TrimSpace(username)
	if err := s.checkLoginRate(ctx, username, remoteAddress); err != nil {
		return admin.Admin{}, Tokens{}, err
	}
	value, err := s.admins.GetByUsername(ctx, username)
	if err != nil {
		_ = security.VerifyPassword(s.dummyPasswordHash, password)
		if !errors.Is(err, repository.ErrNotFound) {
			return admin.Admin{}, Tokens{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		return admin.Admin{}, Tokens{}, ErrInvalidCredentials
	}
	if !security.VerifyPassword(value.PasswordHash, password) {
		return admin.Admin{}, Tokens{}, ErrInvalidCredentials
	}
	tokens, _, err := s.createSession(ctx, value.ID)
	return value, tokens, err
}

// Refresh 轮换 refresh token，旧 token 立即失效。
func (s *Service) Refresh(ctx context.Context, rawRefreshToken string) (Tokens, error) {
	hash := security.HashToken(rawRefreshToken)
	session, err := s.sessions.GetByTokenHash(ctx, hash)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return Tokens{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		// 当前 hash 未命中：回查上一代。命中 = 已轮换的旧 token 被再次
		// 呈上（顺序重用）——宽限窗内视为良性重复刷新，超窗按 OAuth BCP
		// 吊销整个 token family。未命中则是真正的未知 token。
		stale, staleErr := s.sessions.GetByPreviousTokenHash(ctx, hash)
		if staleErr == nil {
			s.revokeSessionIfLateReuse(ctx, stale)
			return Tokens{}, ErrInvalidSession
		}
		if !errors.Is(staleErr, repository.ErrNotFound) {
			return Tokens{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, staleErr)
		}
		return Tokens{}, ErrInvalidSession
	}
	if !time.Now().UTC().Before(session.ExpiresAt) {
		return Tokens{}, ErrInvalidSession
	}
	adminValue, err := s.admins.GetByID(ctx, session.AdminID)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return Tokens{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		return Tokens{}, ErrInvalidSession
	}
	accessToken, accessExpiresAt, err := s.tokens.CreateAccessToken(adminValue.ID, session.ID, s.accessTTL)
	if err != nil {
		return Tokens{}, err
	}
	refreshToken, err := security.NewOpaqueToken(32)
	if err != nil {
		return Tokens{}, err
	}
	refreshExpiresAt := time.Now().UTC().Add(s.refreshTTL)
	if err := s.sessions.Rotate(ctx, session.ID, hash, security.HashToken(refreshToken), refreshExpiresAt); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// 轮换冲突 = 并发竞争中另一方刚刚完成轮换（同 token 重复刷新
			// 竞速）。session 变量是竞速前的旧快照——按 ID 重取最新行再判
			// 宽限（赢家的 last_used_at 是此刻）。宽限窗内视为良性；超窗按
			// OAuth BCP（RFC 6819 §5.2.11 / security-topics §4.14.2）吊销
			// 整个 token family。
			if fresh, freshErr := s.sessions.GetByID(ctx, session.ID); freshErr == nil {
				s.revokeSessionIfLateReuse(ctx, fresh)
			} else if !errors.Is(freshErr, repository.ErrNotFound) {
				return Tokens{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, freshErr)
			}
			return Tokens{}, ErrInvalidSession
		}
		if errors.Is(err, repository.ErrNotFound) {
			return Tokens{}, ErrInvalidSession
		}
		return Tokens{}, err
	}
	return Tokens{AccessToken: accessToken, AccessTokenExpiresAt: accessExpiresAt, RefreshToken: refreshToken, RefreshTokenExpiresAt: refreshExpiresAt}, nil
}

// revokeSessionIfLateReuse 处理轮换后旧 token 重用：session.LastUsedAt
// 是该会话最近一次 Rotate 的时间。距其不足宽限窗的重用视为良性重复
// 刷新（不吊销——赢家 token 存活，重复方自行以新 token 重试）；超窗
// 重用按窃取处理，吊销整个 token family。
func (s *Service) revokeSessionIfLateReuse(ctx context.Context, session admin.Session) {
	if session.LastUsedAt != nil && time.Since(*session.LastUsedAt) < refreshRotationGrace {
		return
	}
	_ = s.sessions.Revoke(ctx, session.ID)
}

// Logout 撤销当前 refresh session。
func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	session, err := s.sessions.GetByTokenHash(ctx, security.HashToken(rawRefreshToken))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	if err := s.sessions.Revoke(ctx, session.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	return nil
}

// AuthenticateAccess 校验 access token 并读取管理员。
func (s *Service) AuthenticateAccess(ctx context.Context, rawAccessToken string) (admin.Admin, error) {
	identity, err := s.tokens.ParseAccessToken(rawAccessToken)
	if err != nil {
		return admin.Admin{}, ErrInvalidSession
	}
	session, err := s.sessions.GetByID(ctx, identity.SessionID)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return admin.Admin{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		return admin.Admin{}, ErrInvalidSession
	}
	if session.AdminID != identity.AdminID || !time.Now().UTC().Before(session.ExpiresAt) {
		return admin.Admin{}, ErrInvalidSession
	}
	value, err := s.admins.GetByID(ctx, identity.AdminID)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return admin.Admin{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		return admin.Admin{}, ErrInvalidSession
	}
	return value, nil
}

// ChangePassword 修改密码并撤销管理员的全部 refresh session。
func (s *Service) ChangePassword(ctx context.Context, adminID uint64, currentPassword, newPassword string) error {
	if !validPasswordLength(newPassword) {
		return ErrInvalidPassword
	}
	value, err := s.admins.GetByID(ctx, adminID)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		return ErrInvalidCredentials
	}
	if !security.VerifyPassword(value.PasswordHash, currentPassword) {
		return ErrInvalidCredentials
	}
	hash, err := security.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.admins.UpdatePasswordAndRevokeSessions(ctx, adminID, hash)
}

func (s *Service) createSession(ctx context.Context, adminID uint64) (Tokens, admin.Session, error) {
	refreshToken, err := security.NewOpaqueToken(32)
	if err != nil {
		return Tokens{}, admin.Session{}, err
	}
	refreshExpiresAt := time.Now().UTC().Add(s.refreshTTL)
	session, err := s.sessions.Create(ctx, admin.Session{AdminID: adminID, RefreshTokenHash: security.HashToken(refreshToken), ExpiresAt: refreshExpiresAt})
	if err != nil {
		return Tokens{}, admin.Session{}, err
	}
	accessToken, accessExpiresAt, err := s.tokens.CreateAccessToken(adminID, session.ID, s.accessTTL)
	if err != nil {
		_ = s.sessions.Revoke(ctx, session.ID)
		return Tokens{}, admin.Session{}, err
	}
	return Tokens{AccessToken: accessToken, AccessTokenExpiresAt: accessExpiresAt, RefreshToken: refreshToken, RefreshTokenExpiresAt: refreshExpiresAt}, session, err
}

func (s *Service) checkLoginRate(ctx context.Context, username, remoteAddress string) error {
	if s.loginLimiter == nil {
		return nil
	}
	now := time.Now().UTC()
	keys := []struct {
		key   string
		limit int
	}{
		{key: "admin-login:ip:" + security.HashToken(strings.TrimSpace(remoteAddress)), limit: 30},
		{key: "admin-login:user:" + security.HashToken(strings.ToLower(username)), limit: 12},
	}
	for _, item := range keys {
		allowed, retryAfter, err := s.loginLimiter.Allow(ctx, item.key, item.limit, now)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		if !allowed {
			return &LoginRateLimitedError{RetryAfter: retryAfter}
		}
	}
	return nil
}
