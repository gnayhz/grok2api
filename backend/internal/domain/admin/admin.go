package admin

import "time"

// Admin 表示系统唯一管理员。
type Admin struct {
	ID           uint64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session 表示可轮换和删除的管理员刷新会话。
type Session struct {
	ID               uint64
	AdminID          uint64
	RefreshTokenHash string
	ExpiresAt        time.Time
	LastUsedAt       *time.Time
	CreatedAt        time.Time
}

// PasswordRef identifies the exact password material verified by an operation.
// Bcrypt creates fresh salted material for every change, including reuse of the
// same plaintext. Persistence must compare it while acquiring the admin lock.
type PasswordRef struct {
	AdminID uint64
	Hash    string `json:"-"`
}

func (a Admin) PasswordRef() PasswordRef {
	return PasswordRef{AdminID: a.ID, Hash: a.PasswordHash}
}

const MaxSessions = 100
