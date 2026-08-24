package security

import (
	"testing"
)

func testKeyMaterial(t *testing.T) (string, string) {
	t.Helper()
	// 两把不同的 32 字节密钥(Base64)。
	return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
}

// 轮换密钥后: 旧密钥加密的存量密文仍可解(历史密钥回退), 新密文用主密钥。
func TestVersionedCipherRotatesWithoutLockout(t *testing.T) {
	oldKey, newKey := testKeyMaterial(t)
	oldCipher, err := NewCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sso-token-value"
	legacyCiphertext, err := oldCipher.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := NewVersionedCipher(newKey, []string{oldKey})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Decrypt(legacyCiphertext); err != nil || got != secret {
		t.Fatalf("legacy ciphertext undecryptable after rotation: %q %v", got, err)
	}
	fresh, err := rotated.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	newOnly, err := NewCipher(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := newOnly.Decrypt(fresh); err != nil || got != secret {
		t.Fatalf("new ciphertext must use primary key: %q %v", got, err)
	}
}

// 无历史密钥或密文来自未知密钥: 维持原错误语义。
func TestVersionedCipherFailsClosed(t *testing.T) {
	oldKey, newKey := testKeyMaterial(t)
	oldCipher, _ := NewCipher(oldKey)
	ciphertext, _ := oldCipher.Encrypt("secret")
	noLegacy, err := NewVersionedCipher(newKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noLegacy.Decrypt(ciphertext); err == nil {
		t.Fatal("unknown-key ciphertext must fail")
	}
}
