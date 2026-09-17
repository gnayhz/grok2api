package security

import (
	"testing"
	"time"

	portcrypto "github.com/chenyme/grok2api/backend/internal/port/crypto"
)

func TestTokenServiceIssuesAndParsesAccessTokens(t *testing.T) {
	service := NewTokenService("12345678901234567890123456789012")
	raw, expiresAt, err := service.CreateAccessToken(7, 9, time.Minute)
	if err != nil || raw == "" {
		t.Fatalf("CreateAccessToken = %q, %v", raw, err)
	}
	if !time.Now().UTC().Before(expiresAt) {
		t.Fatalf("expiresAt %v not in the future", expiresAt)
	}
	identity, err := service.ParseAccessToken(raw)
	if err != nil || identity.AdminID != 7 || identity.SessionID != 9 {
		t.Fatalf("ParseAccessToken = %+v, %v", identity, err)
	}
	if _, err := service.ParseAccessToken("garbage"); err == nil {
		t.Fatal("garbage token parsed")
	}
}

func TestRandomTokenSourceShapes(t *testing.T) {
	var source portcrypto.TokenSource = RandomTokenSource{}
	opaque, err := source.NewOpaqueToken(18)
	if err != nil || len(opaque) != 24 {
		t.Fatalf("NewOpaqueToken = %q, %v", opaque, err)
	}
	hex, err := source.NewHexToken(6)
	if err != nil || len(hex) != 12 {
		t.Fatalf("NewHexToken = %q, %v", hex, err)
	}
	if HashToken("x") == HashToken("y") {
		t.Fatal("HashToken collision")
	}
}

func TestBCryptPasswordHasherVerifiesLegacyHashes(t *testing.T) {
	hasher := NewBCryptPasswordHasher()
	hash, err := hasher.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !hasher.VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("correct password rejected")
	}
	if hasher.VerifyPassword(hash, "wrong") {
		t.Fatal("wrong password accepted")
	}
}
