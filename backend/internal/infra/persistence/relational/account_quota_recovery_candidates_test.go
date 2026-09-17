package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestListQuotaRecoveryWindowsReturnsRawCandidates 锁定存储不再编码恢复资格：
// 查询只保留 remaining <= 0 这一 owner 谓词的超集粗筛，Console 计费快照与没有
// reset 期限的用量窗口都按原始行返回，由调用方按 owner 规则判定。
func TestListQuotaRecoveryWindowsReturnsRawCandidates(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			writer, reader := NewAccountRepository(a), NewAccountRepository(b)
			now := time.Now().UTC().Truncate(time.Microsecond)
			reset := now.Add(2 * time.Hour)
			console, _, err := writer.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "recovery-console", SourceKey: "recovery-console",
				EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			web, _, err := writer.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "recovery-web", SourceKey: "recovery-web",
				EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := saveQuotaWindowsFixture(writer, ctx, console.ID, "", now, []account.QuotaWindow{
				{AccountID: console.ID, Mode: "console_image", Remaining: 0, Total: 20, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now},
				{AccountID: console.ID, Mode: "billing", Remaining: 0, Total: 20, ResetAt: &reset, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now},
				{AccountID: console.ID, Mode: "console_video", Remaining: 9, Total: 20, ResetAt: &reset, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now},
			}); err != nil {
				t.Fatal(err)
			}
			if err := saveQuotaWindowsFixture(writer, ctx, web.ID, account.WebTierBasic, now, []account.QuotaWindow{
				{AccountID: web.ID, Mode: "fast", Remaining: 0, Total: 20, ResetAt: &reset, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now},
				{AccountID: web.ID, Mode: "auto", Remaining: 0, Total: 7, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now},
				{AccountID: web.ID, Mode: "expert", Remaining: 12, Total: 20, ResetAt: &reset, Source: account.QuotaSourceUpstream, SyncedAt: &now, UpdatedAt: now},
			}); err != nil {
				t.Fatal(err)
			}
			values, err := reader.ListQuotaRecoveryWindows(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			byMode := make(map[string]account.QuotaWindow, len(values))
			for _, value := range values {
				if value.Remaining > 0 {
					t.Fatalf("candidate query returned an available window: %#v", value)
				}
				byMode[value.Mode] = value
			}
			if len(values) != 4 || len(byMode) != 4 {
				t.Fatalf("candidates = %#v", values)
			}
			image, ok := byMode["console_image"]
			if !ok || image.Provider != account.ProviderConsole || image.ResetAt != nil {
				t.Fatalf("Console usage window without reset timestamp = %#v", image)
			}
			if billing, ok := byMode["billing"]; !ok || billing.Provider != account.ProviderConsole || billing.ResetAt == nil || !billing.ResetAt.Equal(reset) {
				t.Fatalf("Console billing snapshot candidate = %#v", billing)
			}
			if fast, ok := byMode["fast"]; !ok || fast.Provider != account.ProviderWeb || fast.ResetAt == nil {
				t.Fatalf("Web candidate = %#v", fast)
			}
			if auto, ok := byMode["auto"]; !ok || auto.Provider != account.ProviderWeb || auto.ResetAt != nil {
				t.Fatalf("Web candidate without reset deadline = %#v", auto)
			}
		})
	}
}
