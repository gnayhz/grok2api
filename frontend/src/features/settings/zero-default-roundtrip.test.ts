import assert from "node:assert/strict";
import { describe, it, before } from "node:test";
import type { SettingsConfigDTO } from "@/features/settings/settings-api";

let model: typeof import("./settings-model.ts");

before(async () => {
  if (typeof (globalThis as Record<string, unknown>).window === "undefined") {
    (globalThis as Record<string, unknown>).window = { location: { origin: "http://127.0.0.1:3000" } };
  }
  model = await import("./settings-model.ts");
});

type RetryKey = "accountCooldown" | "evidenceTimeout" | "createdTimeout" | "idleAccountCooldown";
type TestConfig = SettingsConfigDTO & {
  requestRetry: NonNullable<SettingsConfigDTO["requestRetry"]>;
  accountRisk: NonNullable<SettingsConfigDTO["accountRisk"]>;
};

function baseConfig(retryOverrides: Partial<Record<RetryKey, string>>): TestConfig {
  const cfg: TestConfig = {
    server: { maxConcurrentRequests: 1024 },
    providerBuild: {
      baseURL: "https://build.example", fallbackBaseURL: "https://api.example", clientVersion: "1.0.4", clientIdentifier: "grok-shell",
      tokenAuth: "xai-grok-cli", tokenAuthConfigured: false, userAgent: "ua", responseHeaderTimeout: "5m", streamIdleTimeout: "2m",
    },
    providerWeb: {
      baseURL: "https://web.example", quotaTimeout: "10s", chatTimeout: "2m", streamIdleTimeout: "2m",
      imageTimeout: "5m", videoTimeout: "30m", statsigMode: "url", statsigManualConfigured: false,
      statsigSignerURL: "https://signer.example", clearanceMode: "on_demand", flareSolverrURL: "https://fs.example", clearanceTimeout: "30s", clearanceRefresh: "30m",
      mediaConcurrency: 2, allowNSFW: false, recoveryBackoffBase: "1s", recoveryBackoffMax: "10m",
    },
    providerConsole: { baseURL: "https://console.example", chatTimeout: "2m", streamIdleTimeout: "2m" },
    batch: { importConcurrency: 2, conversionConcurrency: 4, syncConcurrency: 4, refreshConcurrency: 2, randomDelay: "100ms" },
    media: { maxImageBytes: 10485760, maxTotalBytes: 20971520, cleanupThresholdPercent: 80, cleanupInterval: "1h" },
    frontend: { publicApiBaseURL: "" },
    routing: {
      stickyTTL: "30m", cooldownBase: "30s", cooldownMax: "10m", capacityWait: "5s", maxAttempts: 6,
      videoMaxAttempts: 0, preferFreeBuild: false, markBuildChatDeniedAsReauth: false,
      accountIsolatedConnections: false, segmentedSelector: { enabled: false, minCandidates: 3000, windowSize: 64 },
    },
    audit: { bufferSize: 256, batchSize: 32, flushInterval: "1s", commitDelayMS: 10, retentionDays: 7 },
    clientKeyDefaults: { rpmLimit: 120, maxConcurrent: 8 },
    accounts: {
      markBuildForbiddenReauth: false, buildForbiddenReauthCodes: ["permission-denied"], excludeBuildBotFlaggedFromScheduling: false,
      autoCleanReauthEnabled: false, autoCleanReauthInterval: "1h", autoCleanReauthMinAge: "24h", autoCleanIncludeDisabled: false,
    },
    requestRetry: {
      enabled: true, maxAttempts: 2, onExhausted: "fail_closed", accountCooldown: "12h", sameAccountRetry: true,
      evidenceTimeout: "3.5s", createdTimeout: "5s", idleAccountCooldown: "15m",
      ...retryOverrides,
    },
    accountRisk: {
      enabled: true, method: "ssoProbe", concurrency: 2, timeout: "30s", onDenied: "flag",
      patrolEnabled: true, patrolInterval: "6h", patrolBucketDays: 30, patrolBatchSize: 50,
      deniedConfirmations: 2, deniedTTL: "24h", probeProxyURL: "", buildProbeEnabled: true,
    },
  };
  return cfg;
}

function withDeniedTTL(cfg: TestConfig, deniedTTL: string): TestConfig {
  cfg.accountRisk.deniedTTL = deniedTTL;
  return cfg;
}

describe("0=默认 语义时长字段:载荷加载与往返", () => {
  it("后端 0=默认 字段载荷为 0s 时表单校验通过且往返不丢", () => {
    const cases: Array<[string, TestConfig]> = [
      ["requestRetry.accountCooldown", baseConfig({ accountCooldown: "0s" })],
      ["requestRetry.evidenceTimeout", baseConfig({ evidenceTimeout: "0s" })],
      ["requestRetry.createdTimeout", baseConfig({ createdTimeout: "0s" })],
      ["requestRetry.idleAccountCooldown", baseConfig({ idleAccountCooldown: "0s" })],
      ["accountRisk.deniedTTL", withDeniedTTL(baseConfig({}), "0s")],
    ];
    for (const [name, cfg] of cases) {
      const form = model.toSettingsForm(cfg);
      const parsed = model.settingsSchema.safeParse(form);
      assert.equal(parsed.success, true, name + " =0s 必须通过表单校验");
      const dto = model.toSettingsDTO(form);
      const dtoValue = name.startsWith("accountRisk.")
        ? dto.accountRisk?.deniedTTL
        : dto.requestRetry?.[name.split(".")[1] as RetryKey];
      assert.equal(dtoValue, "0s", name + " 往返必须保持 0s(默认语义)");
    }
  });

  it("非零值边界仍生效", () => {
    const bad = model.toSettingsForm(baseConfig({ evidenceTimeout: "0.5s" }));
    assert.equal(model.settingsSchema.safeParse(bad).success, false, "低于 1s 仍应拒绝");
    const badCooldown = model.toSettingsForm(baseConfig({ accountCooldown: "30s" }));
    assert.equal(model.settingsSchema.safeParse(badCooldown).success, false, "低于 1m 仍应拒绝");
    const ok = model.toSettingsForm(baseConfig({ evidenceTimeout: "3.5s" }));
    assert.equal(model.settingsSchema.safeParse(ok).success, true);
  });
});