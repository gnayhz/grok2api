// Package tokenhash 是确定性的令牌摘要原语：无状态、纯函数、仅依赖标准库。
// 它不属于端口（没有可替换的行为差异），也不是业务规则；需要摘要的消费方
// 直接复用本模块，避免各自实现或伪装成接口。
package tokenhash

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashToken 返回不可逆的 SHA-256 十六进制摘要。
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
