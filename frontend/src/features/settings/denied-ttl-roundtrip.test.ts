import assert from "node:assert/strict";
import { describe, it, before } from "node:test";

let model: typeof import("./settings-model.ts");

before(async () => {
  if (typeof (globalThis as Record<string, unknown>).window === "undefined") {
    (globalThis as Record<string, unknown>).window = { location: { origin: "http://127.0.0.1:3000" } };
  }
  model = await import("./settings-model.ts");
});

// 与 request-retry-roundtrip.test.ts 同源的最小完整 DTO,补上 accountRisk。
function fullConfigDTO(deniedTTL: string) {
  return {
    server: { maxConcurrentRequests: 1024 },
    providerBuild: {
      baseURL: "https://build.example", fallbackBaseURL: "https://api.example", clientVersion: "1.0.4", clientIdentifier: "grok-shell",
      tokenAuth: "xai-grok-cli", tokenAuthConfigured: false, userAgent: "grok-shell/1.0.4", responseHeaderTimeout: "5m", streamIdleTimeout: "2m",
    },
    providerWeb: {
      baseURL: "https://web.example", quotaTimeout: "10s", chatTimeout: "2m", streamIdleTimeout: "2m",
      imageTimeout: "5m", videoTimeout: "30m", statsigMode: "url", statsigManualConfigured: false,
      statsigSignerURL: "https://signer.example", clearanceMode: "on_demand", flareSolverrURL: "https://fs.example", clearanceTimeout: "30s", clearanceRefresh: "30m",
      mediaConcurrency: 2, allowNSFW: false, recoveryBackoffBase: "1s", recoveryBackoffMax: "10m",
    },
    providerConsole: { baseURL: "https://console.example", chatTimeout: "2m", streamIdleTimeout: "2m" },
    batch: { importConcurrency: 2, conversionConcurrency: 4, syncConcurrency: 4, refreshConcurrency: 2, randomDelay: "0ms" },
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
    },
    accountRisk: {
      enabled: true, method: "ssoProbe", concurrency: 2, timeout: "30s", onDenied: "flag",
      patrolEnabled: true, patrolInterval: "6h", patrolBucketDays: 30, patrolBatchSize: 50,
      deniedConfirmations: 2, deniedTTL, probeProxyURL: "", buildProbeEnabled: true,
    },
  };
}

describe("accountRisk.deniedTTL 表单加载与往返", () => {
  it("后端默认语义 0s 加载即校验通过(否则保存被静默拦截)", () => {
    const form = model.toSettingsForm(fullConfigDTO("0s") as never);
    const parsed = model.settingsSchema.safeParse(form);
    assert.equal(parsed.success, true, `deniedTTL=0s 必须能通过表单校验: ${parsed.success ? "" : JSON.stringify(parsed.error.issues)}`);
  });

  it("0s 往返保持默认语义不被抹掉", () => {
    const form = model.toSettingsForm(fullConfigDTO("0s") as never);
    const dto = model.toSettingsDTO(form);
    assert.equal(dto.accountRisk?.deniedTTL, "0s");
  });

  it("显式时长正常往返且边界生效", () => {
    const form = model.toSettingsForm(fullConfigDTO("2h") as never);
    assert.equal(model.settingsSchema.safeParse(form).success, true);
    assert.equal(model.toSettingsDTO(form).accountRisk?.deniedTTL, "2h");
    form.accountRisk.deniedTTL = { value: 30, unit: "m" };
    assert.equal(model.settingsSchema.safeParse(form).success, false, "低于 1h 应被拒绝");
    form.accountRisk.deniedTTL = { value: 800, unit: "h" };
    assert.equal(model.settingsSchema.safeParse(form).success, false, "超过 720h 应被拒绝");
  });
});