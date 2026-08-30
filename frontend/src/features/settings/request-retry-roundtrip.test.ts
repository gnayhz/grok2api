import assert from "node:assert/strict";
import { describe, it, before } from "node:test";

// settings-api -> runtime-config 在模块顶层读 window；node:test 无 DOM。
// tests-alias-register.mjs 已提供带 location 的最小 window 桩；此处仅在
// 桩缺失（独立运行本文件且未走注册器）时兜底，且不得覆盖已有桩——
// 空 window 缺 location 会让 runtime-config 在导入时抛错。
let model: typeof import("./settings-model.ts");

before(async () => {
  if (typeof (globalThis as Record<string, unknown>).window === "undefined") {
    (globalThis as Record<string, unknown>).window = { location: { origin: "http://127.0.0.1:3000" } };
  }
  model = await import("./settings-model.ts");
});

// toSettingsForm 无条件读取全部配置节（server/provider*/batch/media/
// frontend/routing/audit/clientKeyDefaults/accounts），requestRetry 三节
// 走 ?? 默认。此前的夹具只给 requestRetry——该文件在裸 node 里从未跑
// 通过（toSettingsForm 读 providerBuild.responseHeaderTimeout 即抛错），
// 属"写了但从没验证过"的假测试。这里补全最小完整 DTO。
function fullConfigDTO() {
  return {
    server: { maxConcurrentRequests: 0 },
    providerBuild: {
      baseURL: "https://build.example", fallbackBaseURL: "", clientVersion: "0.0.0", clientIdentifier: "id",
      tokenAuth: "", tokenAuthConfigured: false, userAgent: "ua", responseHeaderTimeout: "10s", streamIdleTimeout: "2m",
    },
    providerWeb: {
      baseURL: "https://web.example", quotaTimeout: "10s", chatTimeout: "2m", streamIdleTimeout: "2m",
      imageTimeout: "5m", videoTimeout: "30m", statsigMode: "url", statsigManualConfigured: false,
      statsigSignerURL: "", clearanceMode: "off", flareSolverrURL: "", clearanceTimeout: "30s", clearanceRefresh: "30m",
      mediaConcurrency: 2, allowNSFW: false, recoveryBackoffBase: "1s", recoveryBackoffMax: "10m",
    },
    providerConsole: { baseURL: "https://console.example", chatTimeout: "2m", streamIdleTimeout: "2m" },
    batch: { importConcurrency: 2, conversionConcurrency: 4, syncConcurrency: 4, refreshConcurrency: 2, randomDelay: "0ms" },
    media: { maxImageBytes: 10, maxTotalBytes: 20, cleanupThresholdPercent: 80, cleanupInterval: "1h" },
    frontend: { publicApiBaseURL: "" },
    routing: {
      stickyTTL: "30m", cooldownBase: "30s", cooldownMax: "10m", capacityWait: "5s", maxAttempts: 6,
      videoMaxAttempts: 0, preferFreeBuild: false, markBuildChatDeniedAsReauth: false,
      accountIsolatedConnections: false, segmentedSelector: { enabled: false, minCandidates: 8, windowSize: 16 },
    },
    audit: { bufferSize: 256, batchSize: 32, flushInterval: "1s", commitDelayMS: 100, retentionDays: 7 },
    clientKeyDefaults: { rpmLimit: 120, maxConcurrent: 8 },
    accounts: {
      markBuildForbiddenReauth: false, buildForbiddenReauthCodes: [], excludeBuildBotFlaggedFromScheduling: false,
      autoCleanReauthEnabled: false, autoCleanReauthInterval: "1h", autoCleanReauthMinAge: "24h", autoCleanIncludeDisabled: false,
    },
    requestRetry: {
      enabled: true, maxAttempts: 2, onExhausted: "fail_closed", accountCooldown: "12h", sameAccountRetry: true,
      evidenceTimeout: "3.5s", createdTimeout: "5s", idleAccountCooldown: "15m",
    },
  };
}

describe("requestRetry settings round-trip", () => {
  it("round-trips the nine-knob guard surface without drift", () => {
    const config = fullConfigDTO();
    config.requestRetry!.maxAttempts = 3;
    const form = model.toSettingsForm(config as never);
    assert.equal(form.requestRetry.maxAttempts, 3);
    assert.equal(form.requestRetry.onExhausted, "fail_closed");
    const back = model.toSettingsDTO(form);
    assert.deepEqual(back.requestRetry, config.requestRetry);
  });

  it("rejects the budget cap violation at the schema layer", () => {
    const config = fullConfigDTO();
    config.requestRetry!.maxAttempts = 4;
    const form = model.toSettingsForm(config as never);
    const result = model.settingsSchema.safeParse(form);
    assert.equal(result.success, false);
  });

  it("coerces unknown exhaustion policy to fail-closed", () => {
    const config = fullConfigDTO();
    config.requestRetry!.onExhausted = "nonsense";
    const form = model.toSettingsForm(config as never);
    assert.equal(form.requestRetry.onExhausted, "fail_closed");
  });
});
