package security

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	portcrypto "github.com/chenyme/grok2api/backend/internal/port/crypto"
	"github.com/golang-jwt/jwt/v5"
	"time"
)

type adminClaims struct {
	AdminID   uint64 `json:"adminId"`
	SessionID uint64 `json:"sessionId"`
	jwt.RegisteredClaims
}

// TokenService 用 HS256 签发和校验短期管理员 access token，并持有签名密钥。
// 它实现 port/crypto.AdminTokenManager；消费方只依赖该合同。
type TokenService struct {
	secret []byte
	issuer string
}

func NewTokenService(secret string) *TokenService {
	return &TokenService{secret: []byte(secret), issuer: "grok2api"}
}

// CreateAccessToken 创建短期管理员 JWT。
func (s *TokenService) CreateAccessToken(adminID, sessionID uint64, ttl time.Duration) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	claims := adminClaims{
		AdminID: adminID, SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   fmt.Sprintf("%d", adminID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	return signed, expiresAt, err
}

// ParseAccessToken 校验管理员 JWT 并返回管理员 ID。
func (s *TokenService) ParseAccessToken(raw string) (portcrypto.AdminTokenIdentity, error) {
	claims := &adminClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("不支持的 JWT 签名算法")
		}
		return s.secret, nil
	}, jwt.WithIssuer(s.issuer))
	if err != nil || !token.Valid || claims.AdminID == 0 || claims.SessionID == 0 {
		return portcrypto.AdminTokenIdentity{}, fmt.Errorf("管理员令牌无效")
	}
	return portcrypto.AdminTokenIdentity{AdminID: claims.AdminID, SessionID: claims.SessionID}, nil
}

// RandomTokenSource 用 crypto/rand 实现不可预测 token 生成能力。
type RandomTokenSource struct{}

func (RandomTokenSource) NewOpaqueToken(bytesLength int) (string, error) {
	buf := make([]byte, bytesLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (RandomTokenSource) NewHexToken(bytesLength int) (string, error) {
	buf := make([]byte, bytesLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

var _ portcrypto.TokenSource = RandomTokenSource{}
var _ portcrypto.AdminTokenManager = (*TokenService)(nil)

// NewOpaqueToken 供 infra 内部（Provider 适配器）直接复用随机源；应用层
// 消费方（含十六进制 token 需求）通过注入的 portcrypto.TokenSource 获得
// 同一能力。
func NewOpaqueToken(bytesLength int) (string, error) {
	return RandomTokenSource{}.NewOpaqueToken(bytesLength)
}

// HashToken 返回不可逆的 SHA-256 十六进制摘要（infra 内复用；应用层使用
// pkg/tokenhash 的同一确定性原语）。
func HashToken(raw string) string {
	return tokenhash.HashToken(raw)
}
