package security

import "golang.org/x/crypto/bcrypt"

// BCryptPasswordHasher 用 bcrypt 实现口令哈希与校验能力；实现
// port/crypto.PasswordHasher，由组合根注入管理员身份用例。
type BCryptPasswordHasher struct{}

func NewBCryptPasswordHasher() *BCryptPasswordHasher { return &BCryptPasswordHasher{} }

func (BCryptPasswordHasher) HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func (BCryptPasswordHasher) VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
