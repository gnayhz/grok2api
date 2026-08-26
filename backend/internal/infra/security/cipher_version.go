package security

// 密钥版本化:主密钥解密失败时按序尝试历史密钥(密文本身不携带版本
// 标记,存量裸格式与新密文格式一致)。运维轮换密钥后, 把旧密钥配置为
// 历史密钥即可让存量凭据继续可解, 同时新密文全部使用新主密钥。
// 密钥轮换的完整闭环(后台再加密迁移)由独立运维任务承担, 本层先保证
// "换钥不锁死"。

import (
	"fmt"
	"strings"
)

// NewVersionedCipher 创建带历史密钥回退的加密器。legacyKeys 中的密文仅在
// 主密钥解密失败时按序尝试;每把历史密钥同样是 Base64 的 32 字节。
type VersionedCipher struct {
	primary *Cipher
	legacy  []*Cipher
}

func NewVersionedCipher(encodedKey string, legacyKeys []string) (*VersionedCipher, error) {
	primary, err := NewCipher(encodedKey)
	if err != nil {
		return nil, err
	}
	result := &VersionedCipher{primary: primary}
	for _, key := range legacyKeys {
		key = strings.TrimSpace(key)
		if key == "" || key == encodedKey {
			continue
		}
		legacy, err := NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("历史密钥无效: %w", err)
		}
		result.legacy = append(result.legacy, legacy)
	}
	return result, nil
}

// Encrypt 始终使用主密钥;输出与 Cipher 一致(裸格式), 存量读写不受影响。
func (c *VersionedCipher) Encrypt(plaintext string) (string, error) {
	return c.primary.Encrypt(plaintext)
}

// Decrypt 先用主密钥;失败且配置了历史密钥时按序回退。所有密钥都失败时
// 返回主密钥的错误(GCM 认证失败与密文损坏不可区分, 与既有语义一致)。
func (c *VersionedCipher) Decrypt(encoded string) (string, error) {
	plain, primaryErr := c.primary.Decrypt(encoded)
	if primaryErr == nil || len(c.legacy) == 0 || encoded == "" {
		return plain, primaryErr
	}
	for _, legacy := range c.legacy {
		if plain, err := legacy.Decrypt(encoded); err == nil {
			return plain, nil
		}
	}
	return "", primaryErr
}
