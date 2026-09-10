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
      enabled: true, maxAttempts: 2, onExhausted: "fail_closed", accountCooldown: "12h",
      evidenceTimeout: "3.5s", createdTimeout: "5s", idleAccountCooldown: "15m",
      ...retryOverrides,
    },
  };
  return cfg;
}

describe("audit retention migration", () => {
  it("keeps zero and fractional durations exact and writes only the canonical field", () => {
    for (const period of ["0s", "36h0m0.000000001s", "8760h0m0s"]) {
      const cfg = baseConfig({});
      cfg.audit.retentionPeriod = period;
      cfg.audit.retentionSource = "runtime";
      const form = model.toSettingsForm(cfg);
      assert.equal(model.settingsSchema.safeParse(form).success, true);
      const saved = model.toSettingsDTO(form);
      assert.equal(saved.audit.retentionPeriod, period);
      assert.equal("retentionDays" in saved.audit, false);
      assert.equal("retentionSource" in saved.audit, false);
    }
  });
  it("reads legacy whole days including explicit zero", () => {
    for (const days of [0, 7, 365]) {
      const cfg = baseConfig({});
      cfg.audit.retentionDays = days;
      assert.equal(model.toSettingsDTO(model.toSettingsForm(cfg)).audit.retentionPeriod, `${days * 24}h`);
    }
  });
});
