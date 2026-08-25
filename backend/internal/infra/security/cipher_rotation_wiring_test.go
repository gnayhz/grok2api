package security

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestVersionedCipherRotationFlow 锁定运维轮换闭环（round 56 接线后
// 首次可用）：旧主密钥加密的密文 → 新主密钥 + 旧密钥作历史 → 解密
// 成功；新写入全部用新主密钥（旧 Cipher 已无法解开）。
func TestVersionedCipherRotationFlow(t *testing.T) {
	oldKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("o", 32)))
	newKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("n", 32)))

	old, err := NewCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sso-token-rotation-check"
	stored, err := old.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := NewVersionedCipher(newKey, []string{oldKey})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Decrypt(stored)
	if err != nil || got != secret {
		t.Fatalf("legacy decrypt after rotation: got=%q err=%v", got, err)
	}

	fresh, err := rotated.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Decrypt(fresh); err == nil {
		t.Fatal("new ciphertext must be bound to the new primary key only")
	}
	if again, err := rotated.Decrypt(fresh); err != nil || again != secret {
		t.Fatalf("primary decrypt: got=%q err=%v", again, err)
	}
}
