package crypto

import "time"

// Cryptor 是凭据加解密的最小接口。消费方依赖此面而非具体 Cipher。
type Cryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(encoded string) (string, error)
}

// PasswordHasher 是口令哈希与校验能力。bcrypt 实现在 infra/security，
// 由组合根注入；这里只描述消费方（管理员身份）需要的面。
type PasswordHasher interface {
	HashPassword(password string) (string, error)
	VerifyPassword(hash, password string) bool
}

// AdminTokenIdentity 是解析管理员 access token 后的稳定身份值。
type AdminTokenIdentity struct {
	AdminID   uint64
	SessionID uint64
}

// AdminTokenManager 签发和校验短期管理员 access token。JWT/HS256 的具体
// 实现与密钥持有在 infra/security；消费方不接触签名机制。
type AdminTokenManager interface {
	CreateAccessToken(adminID, sessionID uint64, ttl time.Duration) (string, time.Time, error)
	ParseAccessToken(raw string) (AdminTokenIdentity, error)
}

// TokenSource 是不可预测随机 token 的生成能力（refresh token、客户端 Key
// 密钥段、会话/幂等标识）。随机源实现在 infra/security。
type TokenSource interface {
	NewOpaqueToken(bytesLength int) (string, error)
	NewHexToken(bytesLength int) (string, error)
}
