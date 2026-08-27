import { z } from "zod";

import { defaultAccountRiskConfig, defaultEgressRotationConfig, defaultRequestRetryConfig, type SettingsConfigDTO } from "@/features/settings/settings-api";

export type DurationUnit = "s" | "m" | "h" | "d";
export type DurationValue = { value: number; unit: DurationUnit };
export type ByteSizeUnit = "MiB" | "GiB";
export type ByteSizeValue = { value: number; unit: ByteSizeUnit };

export const MAX_ROUTING_ATTEMPTS = 65535;
export const UNLIMITED_ROUTING_ATTEMPTS = -1;


const durationSchema = z.object({ value: z.number().positive(), unit: z.enum(["s", "m", "h", "d"]) });
// 0 是有意义值(关闭)的时长字段(如 earlyHeaderAbort)。
const nonNegativeDurationSchema = z.object({ value: z.number().nonnegative(), unit: z.enum(["s", "m", "h", "d"]) });
const positiveInteger = z.number().int().positive();
const byteSizeSchema = z.object({ value: z.number().positive(), unit: z.enum(["MiB", "GiB"]) });
const routingTTLDuration = durationSchema.refine((value) => durationSeconds(value) <= 30 * 86_400);
const routingCooldownDuration = durationSchema.refine((value) => durationSeconds(value) <= 86_400);
const routingCapacityWaitDuration = durationSchema.refine((value) => durationSeconds(value) <= 30);
const auditFlushDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 0.01 && seconds <= 60;
});
const consoleChatDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 5 && seconds <= 30 * 60;
});
const buildResponseHeaderDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 30 && seconds <= 30 * 60;
});
const buildStreamIdleDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 30 && seconds <= 10 * 60;
});
const providerStreamIdleDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 30 && seconds <= 10 * 60;
});
const forbiddenCodePattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;

function parseForbiddenCodes(value: string): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const item of value.split(/[\n,]/)) {
    const code = item.trim().toLowerCase();
    if (code === "" || seen.has(code)) continue;
    seen.add(code);
    result.push(code);
  }
  return result;
}

function validPublicAPIBaseURL(value: string): boolean {
  const trimmed = value.trim();
  if (trimmed.length === 0) return true;
  try {
    const parsed = new URL(trimmed);
    if (parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") return false;
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}

export const settingsSchema = z.object({
  server: z.object({
    maxConcurrentRequests: positiveInteger.max(100_000),
  }),
  providerBuild: z.object({
    baseURL: z.url(),
    fallbackBaseURL: z.url().refine((value) => value.startsWith("https://")),
    clientVersion: z.string().trim().min(1),
    clientIdentifier: z.string().trim().min(1),
    tokenAuth: z.string().trim().min(1),
    tokenAuthConfigured: z.boolean(),
    userAgent: z.string().trim().min(1),
    responseHeaderTimeout: buildResponseHeaderDuration,
    streamIdleTimeout: buildStreamIdleDuration,
  }),
  providerWeb: z.object({
    baseURL: z.url().refine((value) => value.startsWith("https://")),
    statsigMode: z.enum(["manual", "url"]),
    statsigManualValue: z.string().trim().max(4096),
    statsigManualConfigured: z.boolean(),
    statsigSignerURL: z.string().trim().max(2048),
    clearanceMode: z.enum(["manual", "flaresolverr", "on_demand"]),
    flareSolverrURL: z.string().trim().max(2048),
    clearanceTimeout: durationSchema.refine((value) => durationSeconds(value) >= 10 && durationSeconds(value) <= 300),
    clearanceRefresh: durationSchema.refine((value) => durationSeconds(value) >= 60 && durationSeconds(value) <= 86_400),
    quotaTimeout: durationSchema, chatTimeout: durationSchema, streamIdleTimeout: providerStreamIdleDuration, imageTimeout: durationSchema, videoTimeout: durationSchema,
    mediaConcurrency: positiveInteger.max(64), allowNSFW: z.boolean(),
    recoveryBackoffBase: durationSchema, recoveryBackoffMax: durationSchema,
  }).superRefine((value, context) => {
    if (durationSeconds(value.streamIdleTimeout) > durationSeconds(value.chatTimeout)) {
      context.addIssue({ code: "custom", path: ["streamIdleTimeout"], message: "invalid" });
    }
    if (durationSeconds(value.recoveryBackoffMax) < durationSeconds(value.recoveryBackoffBase)) {
      context.addIssue({ code: "custom", path: ["recoveryBackoffMax"], message: "invalid" });
    }
    if (value.statsigMode === "manual" && !value.statsigManualConfigured && value.statsigManualValue.length === 0) {
      context.addIssue({ code: "custom", path: ["statsigManualValue"], message: "required" });
    }
    if (value.statsigManualValue.length > 0 && !validStatsigID(value.statsigManualValue)) {
      context.addIssue({ code: "custom", path: ["statsigManualValue"], message: "invalid" });
    }
    if (value.statsigMode === "url") {
      if (!validStatsigSignerURL(value.statsigSignerURL)) {
        context.addIssue({ code: "custom", path: ["statsigSignerURL"], message: "invalid" });
      }
    }
    if (value.clearanceMode !== "manual" && !validHTTPURL(value.flareSolverrURL)) {
      context.addIssue({ code: "custom", path: ["flareSolverrURL"], message: "invalid" });
    }
  }),
  providerConsole: z.object({
    baseURL: z.url().refine((value) => value.startsWith("https://")),
    chatTimeout: consoleChatDuration,
    streamIdleTimeout: providerStreamIdleDuration,
  }).refine((value) => durationSeconds(value.streamIdleTimeout) <= durationSeconds(value.chatTimeout), {
    path: ["streamIdleTimeout"], message: "invalid",
  }),
  batch: z.object({
    importConcurrency: positiveInteger.max(50),
    conversionConcurrency: positiveInteger.max(50),
    syncConcurrency: positiveInteger.max(50),
    refreshConcurrency: positiveInteger.max(50),
    randomDelay: z.number().int().min(0).max(5_000),
  }),
  media: z.object({
    maxImageSize: byteSizeSchema.refine((value) => byteSizeBytes(value) >= 1 << 20 && byteSizeBytes(value) <= 32 << 20),
    maxTotalSize: byteSizeSchema.refine((value) => byteSizeBytes(value) <= 2 ** 40),
    cleanupThresholdPercent: z.number().int().min(50).max(95),
    cleanupInterval: durationSchema.refine((value) => durationSeconds(value) >= 60 && durationSeconds(value) <= 86_400),
  }).refine((value) => byteSizeBytes(value.maxTotalSize) >= byteSizeBytes(value.maxImageSize), { path: ["maxTotalSize"] }),
  frontend: z.object({
    publicApiBaseURL: z.string().trim().max(2048).refine((value) => validPublicAPIBaseURL(value), { message: "invalid" }),
  }),
  routing: z.object({
    stickyTTL: routingTTLDuration,
    cooldownBase: routingCooldownDuration,
    cooldownMax: routingCooldownDuration,
    capacityWait: routingCapacityWaitDuration,
    maxAttempts: z.union([z.literal(UNLIMITED_ROUTING_ATTEMPTS), positiveInteger.max(65535)]),
    videoMaxAttempts: z.union([z.literal(UNLIMITED_ROUTING_ATTEMPTS), positiveInteger.max(65535)]),
    preferFreeBuild: z.boolean(),
    markBuildChatDeniedAsReauth: z.boolean(),
    accountIsolatedConnections: z.boolean(),
    segmentedSelector: z.object({
      enabled: z.boolean(),
      minCandidates: z.number().int().min(100).max(1_000_000),
      windowSize: z.number().int().min(8).max(256),
    }),
  }).refine((value) => durationSeconds(value.cooldownMax) >= durationSeconds(value.cooldownBase), { path: ["cooldownMax"] })
    .refine((value) => value.segmentedSelector.windowSize <= value.segmentedSelector.minCandidates, { path: ["segmentedSelector", "windowSize"] }),
  audit: z.object({
    bufferSize: positiveInteger.max(262_144),
    batchSize: positiveInteger.max(4_096),
    flushInterval: auditFlushDuration,
    commitDelayMS: positiveInteger.max(50),
    retentionDays: z.number().int().min(0).max(365),
  })
    .refine((value) => value.batchSize <= value.bufferSize, { path: ["batchSize"] }),
  clientKeyDefaults: z.object({ rpmLimit: positiveInteger.max(100_000), maxConcurrent: positiveInteger.max(1_024) }),
  accounts: z.object({
    markBuildForbiddenReauth: z.boolean(),
    buildForbiddenReauthCodes: z.string().superRefine((value, context) => {
      const codes = parseForbiddenCodes(value);
      if (codes.length === 0 || codes.length > 32 || codes.some((code) => !forbiddenCodePattern.test(code))) {
        context.addIssue({ code: "custom", message: "invalid" });
      }
    }),
    excludeBuildBotFlaggedFromScheduling: z.boolean(),
    autoCleanReauthEnabled: z.boolean(),
    autoCleanReauthInterval: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 60 && seconds <= 3_600;
    }),
    autoCleanReauthMinAge: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 60 && seconds <= 30 * 86_400;
    }),
    autoCleanIncludeDisabled: z.boolean(),
  }),
  // 实时路由守卫(质量扣留/截止预算)。边界与后端 validateRequestRetry 对齐。
  requestRetry: z.object({
    enabled: z.boolean(),
    createdTimeout: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 1 && seconds <= 120;
    }),
    evidenceTimeout: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 3 && seconds <= 300;
    }),
    holdTimeout: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 0.2 && seconds <= 30;
    }),
    earlyHeaderAbort: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 3 && seconds <= 60);
    }),
    maxAttempts: z.number().int().min(1).max(6),
    minOutputTokens: z.number().int().min(1).max(256),
    sameAccountRetry: z.boolean(),
    onExhausted: z.enum(["fail_closed", "fail_open"]),
    accountCooldown: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 60 && seconds <= 168 * 3_600;
    }),
    idleAccountCooldown: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 60 && seconds <= 168 * 3_600);
    }),
  }),
  // 账号风险归因(RSC 检测/处置)。边界与后端 AccountRiskRSCConfig 校验对齐。
  accountRisk: z.object({
    enabled: z.boolean(),
    method: z.enum(["ssoProbe", "homepage"]),
    concurrency: z.number().int().min(1).max(8),
    timeout: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 5 && seconds <= 60;
    }),
    onDenied: z.enum(["flag", "disable", "markOnly"]),
    patrolEnabled: z.boolean(),
    patrolBucketDays: z.number().int().min(7).max(90),
    patrolInterval: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 60 && seconds <= 6 * 3600;
    }),
    patrolBatchSize: z.number().int().min(1).max(200),
    buildProbeEnabled: z.boolean(),
  }),
  // 出口换 IP 轮换调度。边界与后端 EgressRotationConfig 校验对齐。
  egressRotation: z.object({
    enabled: z.boolean(),
    minNodeInterval: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 10 && seconds <= 86_400);
    }),
    maxAttemptsPerQuarantine: z.number().int().min(0).max(100),
    maxGlobalPerHour: z.number().int().min(0).max(10_000),
    webhookTimeout: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 1 && seconds <= 600);
    }),
    webhookRetries: z.number().int().min(0).max(10),
    settleDelay: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || seconds <= 3_600;
    }),
    probeTimeout: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 1 && seconds <= 3_600);
    }),
    probeInterval: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 1 && seconds <= 600);
    }),
    canaryModelPublicId: z.string().trim().max(255),
    canaryCreatedTimeout: nonNegativeDurationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds === 0 || (seconds >= 1 && seconds <= 600);
    }),
  }),
});

export type SettingsForm = z.infer<typeof settingsSchema>;

export function toSettingsForm(config: SettingsConfigDTO): SettingsForm {
  const requestRetry = config.requestRetry ?? defaultRequestRetryConfig();
  const egressRotation = config.egressRotation ?? defaultEgressRotationConfig();
  const accountRisk = config.accountRisk ?? defaultAccountRiskConfig();
  return {
    server: config.server,
    providerBuild: { ...config.providerBuild, responseHeaderTimeout: parseDuration(config.providerBuild.responseHeaderTimeout), streamIdleTimeout: parseDuration(config.providerBuild.streamIdleTimeout) },
    providerWeb: {
      ...config.providerWeb,
      statsigManualValue: "",
      clearanceTimeout: parseDuration(config.providerWeb.clearanceTimeout), clearanceRefresh: parseDuration(config.providerWeb.clearanceRefresh),
      quotaTimeout: parseDuration(config.providerWeb.quotaTimeout), chatTimeout: parseDuration(config.providerWeb.chatTimeout), streamIdleTimeout: parseDuration(config.providerWeb.streamIdleTimeout),
      imageTimeout: parseDuration(config.providerWeb.imageTimeout), videoTimeout: parseDuration(config.providerWeb.videoTimeout),
      recoveryBackoffBase: parseDuration(config.providerWeb.recoveryBackoffBase), recoveryBackoffMax: parseDuration(config.providerWeb.recoveryBackoffMax),
    },
    providerConsole: { ...config.providerConsole, chatTimeout: parseDuration(config.providerConsole.chatTimeout), streamIdleTimeout: parseDuration(config.providerConsole.streamIdleTimeout) },
    batch: { ...config.batch, randomDelay: parseDurationMilliseconds(config.batch.randomDelay) },
    media: {
      maxImageSize: parseByteSize(config.media.maxImageBytes), maxTotalSize: parseByteSize(config.media.maxTotalBytes),
      cleanupThresholdPercent: config.media.cleanupThresholdPercent,
      cleanupInterval: parseDuration(config.media.cleanupInterval),
    },
    frontend: {
      publicApiBaseURL: config.frontend.publicApiBaseURL,
    },
    routing: {
      stickyTTL: parseDuration(config.routing.stickyTTL), cooldownBase: parseDuration(config.routing.cooldownBase),
      cooldownMax: parseDuration(config.routing.cooldownMax), capacityWait: parseDuration(config.routing.capacityWait), maxAttempts: config.routing.maxAttempts, videoMaxAttempts: !config.routing.videoMaxAttempts || config.routing.videoMaxAttempts === 0 ? 999 : config.routing.videoMaxAttempts,
      preferFreeBuild: config.routing.preferFreeBuild,
      markBuildChatDeniedAsReauth: config.routing.markBuildChatDeniedAsReauth,
      accountIsolatedConnections: config.routing.accountIsolatedConnections,
      segmentedSelector: config.routing.segmentedSelector,
    },
    audit: {
      bufferSize: config.audit.bufferSize,
      batchSize: config.audit.batchSize,
      flushInterval: parseDuration(config.audit.flushInterval),
      commitDelayMS: config.audit.commitDelayMS,
      retentionDays: config.audit.retentionDays ?? 7,
    },
    clientKeyDefaults: config.clientKeyDefaults,
    accounts: {
      markBuildForbiddenReauth: config.accounts.markBuildForbiddenReauth,
      buildForbiddenReauthCodes: config.accounts.buildForbiddenReauthCodes.join("\n"),
      excludeBuildBotFlaggedFromScheduling: config.accounts.excludeBuildBotFlaggedFromScheduling,
      autoCleanReauthEnabled: config.accounts.autoCleanReauthEnabled,
      autoCleanReauthInterval: parseDuration(config.accounts.autoCleanReauthInterval),
      autoCleanReauthMinAge: parseDuration(config.accounts.autoCleanReauthMinAge),
      autoCleanIncludeDisabled: config.accounts.autoCleanIncludeDisabled,
    },
    requestRetry: {
      enabled: requestRetry.enabled,
      maxAttempts: requestRetry.maxAttempts,
      holdTimeout: parseDuration(requestRetry.holdTimeout),
      minOutputTokens: requestRetry.minOutputTokens,
      onExhausted: requestRetry.onExhausted === "fail_open" ? "fail_open" : "fail_closed",
      accountCooldown: parseDuration(requestRetry.accountCooldown),
      earlyHeaderAbort: parseDuration(requestRetry.earlyHeaderAbort),
      sameAccountRetry: requestRetry.sameAccountRetry,
      evidenceTimeout: parseDuration(requestRetry.evidenceTimeout),
      createdTimeout: parseDuration(requestRetry.createdTimeout),
      idleAccountCooldown: parseDuration(requestRetry.idleAccountCooldown),
    },
    egressRotation: {
      enabled: egressRotation.enabled,
      maxAttemptsPerQuarantine: egressRotation.maxAttemptsPerQuarantine,
      minNodeInterval: parseDuration(egressRotation.minNodeInterval),
      maxGlobalPerHour: egressRotation.maxGlobalPerHour,
      webhookTimeout: parseDuration(egressRotation.webhookTimeout),
      webhookRetries: egressRotation.webhookRetries,
      settleDelay: parseDuration(egressRotation.settleDelay),
      probeTimeout: parseDuration(egressRotation.probeTimeout),
      probeInterval: parseDuration(egressRotation.probeInterval),
      canaryModelPublicId: egressRotation.canaryModelPublicId,
      canaryCreatedTimeout: parseDuration(egressRotation.canaryCreatedTimeout),
    },
    accountRisk: {
      enabled: accountRisk.enabled,
      method: accountRisk.method === "homepage" ? "homepage" : "ssoProbe",
      concurrency: accountRisk.concurrency,
      timeout: parseDuration(accountRisk.timeout),
      onDenied: accountRisk.onDenied === "disable" || accountRisk.onDenied === "markOnly" ? accountRisk.onDenied : "flag",
      patrolEnabled: accountRisk.patrolEnabled,
      patrolBucketDays: accountRisk.patrolBucketDays,
      patrolInterval: parseDuration(accountRisk.patrolInterval || "15m"),
      patrolBatchSize: accountRisk.patrolBatchSize || 50,
      buildProbeEnabled: accountRisk.buildProbeEnabled ?? false,
    },
  };
}

export function toSettingsDTO(config: SettingsForm): SettingsConfigDTO {
  return {
    server: config.server,
    providerBuild: { ...config.providerBuild, responseHeaderTimeout: formatDuration(config.providerBuild.responseHeaderTimeout), streamIdleTimeout: formatDuration(config.providerBuild.streamIdleTimeout) },
    providerWeb: {
      ...config.providerWeb,
      quotaTimeout: formatDuration(config.providerWeb.quotaTimeout), chatTimeout: formatDuration(config.providerWeb.chatTimeout), streamIdleTimeout: formatDuration(config.providerWeb.streamIdleTimeout),
      imageTimeout: formatDuration(config.providerWeb.imageTimeout), videoTimeout: formatDuration(config.providerWeb.videoTimeout),
      clearanceTimeout: formatDuration(config.providerWeb.clearanceTimeout), clearanceRefresh: formatDuration(config.providerWeb.clearanceRefresh),
      recoveryBackoffBase: formatDuration(config.providerWeb.recoveryBackoffBase), recoveryBackoffMax: formatDuration(config.providerWeb.recoveryBackoffMax),
    },
    providerConsole: { ...config.providerConsole, chatTimeout: formatDuration(config.providerConsole.chatTimeout), streamIdleTimeout: formatDuration(config.providerConsole.streamIdleTimeout) },
    batch: { ...config.batch, randomDelay: `${config.batch.randomDelay}ms` },
    media: {
      maxImageBytes: byteSizeBytes(config.media.maxImageSize), maxTotalBytes: byteSizeBytes(config.media.maxTotalSize),
      cleanupThresholdPercent: config.media.cleanupThresholdPercent,
      cleanupInterval: formatDuration(config.media.cleanupInterval),
    },
    frontend: {
      publicApiBaseURL: config.frontend.publicApiBaseURL.trim(),
    },
    routing: {
      stickyTTL: formatDuration(config.routing.stickyTTL), cooldownBase: formatDuration(config.routing.cooldownBase),
      cooldownMax: formatDuration(config.routing.cooldownMax), capacityWait: formatDuration(config.routing.capacityWait), maxAttempts: config.routing.maxAttempts, videoMaxAttempts: !config.routing.videoMaxAttempts || config.routing.videoMaxAttempts === 0 ? 999 : config.routing.videoMaxAttempts,
      preferFreeBuild: config.routing.preferFreeBuild,
      markBuildChatDeniedAsReauth: config.routing.markBuildChatDeniedAsReauth,
      accountIsolatedConnections: config.routing.accountIsolatedConnections,
      segmentedSelector: config.routing.segmentedSelector,
    },
    audit: {
      bufferSize: config.audit.bufferSize,
      batchSize: config.audit.batchSize,
      flushInterval: formatDuration(config.audit.flushInterval),
      commitDelayMS: config.audit.commitDelayMS,
      retentionDays: config.audit.retentionDays,
    },
    clientKeyDefaults: config.clientKeyDefaults,
    accounts: {
      markBuildForbiddenReauth: config.accounts.markBuildForbiddenReauth,
      buildForbiddenReauthCodes: parseForbiddenCodes(config.accounts.buildForbiddenReauthCodes),
      excludeBuildBotFlaggedFromScheduling: config.accounts.excludeBuildBotFlaggedFromScheduling,
      autoCleanReauthEnabled: config.accounts.autoCleanReauthEnabled,
      autoCleanReauthInterval: formatDuration(config.accounts.autoCleanReauthInterval),
      autoCleanReauthMinAge: formatDuration(config.accounts.autoCleanReauthMinAge),
      autoCleanIncludeDisabled: config.accounts.autoCleanIncludeDisabled,
    },
    requestRetry: {
      enabled: config.requestRetry.enabled,
      maxAttempts: config.requestRetry.maxAttempts,
      holdTimeout: formatDuration(config.requestRetry.holdTimeout),
      minOutputTokens: config.requestRetry.minOutputTokens,
      onExhausted: config.requestRetry.onExhausted,
      accountCooldown: formatDuration(config.requestRetry.accountCooldown),
      earlyHeaderAbort: formatNonNegativeDuration(config.requestRetry.earlyHeaderAbort),
      sameAccountRetry: config.requestRetry.sameAccountRetry,
      evidenceTimeout: formatDuration(config.requestRetry.evidenceTimeout),
      createdTimeout: formatDuration(config.requestRetry.createdTimeout),
      idleAccountCooldown: formatNonNegativeDuration(config.requestRetry.idleAccountCooldown),
    },
    egressRotation: {
      enabled: config.egressRotation.enabled,
      maxAttemptsPerQuarantine: config.egressRotation.maxAttemptsPerQuarantine,
      minNodeInterval: formatNonNegativeDuration(config.egressRotation.minNodeInterval),
      maxGlobalPerHour: config.egressRotation.maxGlobalPerHour,
      webhookTimeout: formatNonNegativeDuration(config.egressRotation.webhookTimeout),
      webhookRetries: config.egressRotation.webhookRetries,
      settleDelay: formatNonNegativeDuration(config.egressRotation.settleDelay),
      probeTimeout: formatNonNegativeDuration(config.egressRotation.probeTimeout),
      probeInterval: formatNonNegativeDuration(config.egressRotation.probeInterval),
      canaryModelPublicId: config.egressRotation.canaryModelPublicId.trim(),
      canaryCreatedTimeout: formatNonNegativeDuration(config.egressRotation.canaryCreatedTimeout),
    },
    accountRisk: {
      enabled: config.accountRisk.enabled,
      method: config.accountRisk.method,
      concurrency: config.accountRisk.concurrency,
      timeout: formatDuration(config.accountRisk.timeout),
      onDenied: config.accountRisk.onDenied,
      patrolEnabled: config.accountRisk.patrolEnabled,
      patrolBucketDays: config.accountRisk.patrolBucketDays,
      patrolInterval: formatDuration(config.accountRisk.patrolInterval),
      patrolBatchSize: config.accountRisk.patrolBatchSize,
      buildProbeEnabled: config.accountRisk.buildProbeEnabled,
    },
  };
}

export function isDurationUnit(value: string): value is DurationUnit {
  return value === "s" || value === "m" || value === "h" || value === "d";
}

export function isByteSizeUnit(value: string): value is ByteSizeUnit {
  return value === "MiB" || value === "GiB";
}

function byteSizeBytes(value: ByteSizeValue): number {
  return Math.round(value.value * (value.unit === "GiB" ? 2 ** 30 : 2 ** 20));
}

function parseByteSize(bytes: number): ByteSizeValue {
  if (bytes >= 2 ** 30 && bytes % 2 ** 30 === 0) return { value: bytes / 2 ** 30, unit: "GiB" };
  return { value: bytes / 2 ** 20, unit: "MiB" };
}

function durationSeconds(value: DurationValue): number {
  const factors: Record<DurationUnit, number> = { s: 1, m: 60, h: 3_600, d: 86_400 };
  return value.value * factors[value.unit];
}

function formatDuration(value: DurationValue): string {
  if (value.unit === "d") return `${value.value * 24}h`;
  return `${value.value}${value.unit}`;
}

// 0 是有意义值(关闭/默认)的时长字段:0 必须序列化为 "0s" 而不是被抹掉。
function formatNonNegativeDuration(value: DurationValue): string {
  if (durationSeconds(value) === 0) return "0s";
  return formatDuration(value);
}

function parseDuration(value: string): DurationValue {
  const simple = value.match(/^(\d+(?:\.\d+)?)(ms|s|m|h)$/);
  if (simple) {
    const amount = Number(simple[1]);
    if (simple[2] === "ms") return { value: amount / 1000, unit: "s" };
    if (simple[2] === "h" && amount >= 24 && amount % 24 === 0) return { value: amount / 24, unit: "d" };
    if (isDurationUnit(simple[2])) return { value: amount, unit: simple[2] };
  }

  const factors: Record<string, number> = { ns: 0.000001, us: 0.001, "µs": 0.001, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  const parts = [...value.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)];
  if (parts.map((part) => part[0]).join("") !== value || parts.length === 0) return { value: 1, unit: "s" };
  const milliseconds = parts.reduce((total, part) => total + Number(part[1]) * factors[part[2]], 0);
  const units: Array<[DurationUnit, number]> = [["d", 86_400_000], ["h", 3_600_000], ["m", 60_000], ["s", 1000]];
  for (const [unit, factor] of units) {
    const amount = milliseconds / factor;
    if (amount >= 1 && Number.isInteger(amount)) return { value: amount, unit };
  }
  return { value: milliseconds / 1000, unit: "s" };
}

function parseDurationMilliseconds(value: string): number {
  return Math.round(durationSeconds(parseDuration(value)) * 1000);
}

function validStatsigID(value: string): boolean {
  try {
    const normalized = value.trim().replace(/-/g, "+").replace(/_/g, "/");
    const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
    return atob(padded).length === 70;
  } catch {
    return false;
  }
}

function validStatsigSignerURL(value: string): boolean {
  try {
    const parsed = new URL(value);
    if (parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") return false;
    const internal = internalSignerHostname(parsed.hostname);
    if (internal) return parsed.protocol === "http:" || parsed.protocol === "https:";
    return parsed.protocol === "https:" && (parsed.port === "" || parsed.port === "443");
  } catch {
    return false;
  }
}

function validHTTPURL(value: string): boolean {
  try {
    const parsed = new URL(value);
    if (parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") return false;
    const internal = internalSignerHostname(parsed.hostname);
    if (internal) return parsed.protocol === "http:" || parsed.protocol === "https:";
    return parsed.protocol === "https:" && (parsed.port === "" || parsed.port === "443");
  } catch {
    return false;
  }
}

/** Performs fast client-side proxy validation; the backend remains authoritative. */
function validProxyURL(value: string): boolean {
  const trimmed = value.trim();
  if (trimmed.length === 0) return true;
  if (trimmed.length > 8192 || [...trimmed].some((char) => {
    const code = char.charCodeAt(0);
    return code <= 0x1f || code === 0x7f;
  })) return false;
  if ((trimmed.match(/\{account\}/g) ?? []).length > 1) return false;
  const scheme = trimmed.slice(0, trimmed.indexOf(":")).toLowerCase();
  if (["trojan", "vless", "ss", "vmess"].includes(scheme) && trimmed.includes("{account}")) return false;
  if (scheme === "vmess") return validVMessURL(trimmed);
  if (scheme === "ss") return validShadowsocksURL(trimmed);
  try {
    const parseValue = trimmed.replaceAll("{account}", "grok2api_account_placeholder");
    const parsed = new URL(parseValue);
    if (!parsed.host || !parsed.hostname) return false;
    const parsedScheme = parsed.protocol.replace(/:$/, "").toLowerCase();
    if (["trojan", "vless"].includes(parsedScheme)) {
      if (!parsed.username || parsed.password || !parsed.port) return false;
      const transport = (parsed.searchParams.get("type") ?? parsed.searchParams.get("network") ?? "tcp").toLowerCase();
      if (!["tcp", "none", "ws", "websocket"].includes(transport)) return false;
      const security = (parsed.searchParams.get("security") ?? "").toLowerCase();
      if (!["", "none", "tls"].includes(security)) return false;
      if (parsed.searchParams.get("flow")) return false;
      if (parsedScheme === "vless" && !/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(parsed.username)) return false;
      if (parsedScheme === "vless" && !["", "none"].includes((parsed.searchParams.get("encryption") ?? "").toLowerCase())) return false;
      if (!["", "none"].includes((parsed.searchParams.get("headerType") ?? "").toLowerCase())) return false;
      if (!validTunnelBoolean(parsed.searchParams, ["allowInsecure", "insecure", "skip-cert-verify"])) return false;
      if (parsed.pathname !== "" && parsed.pathname !== "/") return false;
      const webSocket = transport === "ws" || transport === "websocket";
      if (!webSocket && (parsed.searchParams.has("host") || parsed.searchParams.has("path"))) return false;
      if (webSocket) {
        const host = parsed.searchParams.get("host") ?? parsed.searchParams.get("sni") ?? parsed.searchParams.get("peer") ?? parsed.hostname;
        if (!validWebSocketHost(host)) return false;
      }
      return true;
    }
    if (!["http", "https", "socks4", "socks4a", "socks5", "socks5h"].includes(parsedScheme)) return false;
    if (parsed.search || parsed.hash || (parsed.pathname !== "" && parsed.pathname !== "/")) return false;
    if (trimmed.includes("{account}")) {
      if (!parsed.username.includes("grok2api_account_placeholder")) return false;
    }
    return true;
  } catch {
    return false;
  }
}

function validShadowsocksURL(value: string): boolean {
  try {
    let payload = value.slice("ss://".length).split("#", 1)[0];
    const queryIndex = payload.indexOf("?");
    if (queryIndex >= 0) {
      if ([...new URLSearchParams(payload.slice(queryIndex + 1)).keys()].length !== 0) return false;
      payload = payload.slice(0, queryIndex);
    }
    let credentials: string;
    let server: string;
    const separator = payload.lastIndexOf("@");
    if (separator >= 0) {
      credentials = decodeURIComponent(payload.slice(0, separator));
      if (!credentials.includes(":")) credentials = decodeBase64Text(credentials);
      server = payload.slice(separator + 1);
    } else {
      const decoded = decodeBase64Text(payload);
      const legacySeparator = decoded.lastIndexOf("@");
      if (legacySeparator < 0) return false;
      credentials = decoded.slice(0, legacySeparator);
      server = decoded.slice(legacySeparator + 1);
    }
    const credentialSeparator = credentials.indexOf(":");
    if (credentialSeparator <= 0 || credentialSeparator === credentials.length - 1) return false;
    const method = credentials.slice(0, credentialSeparator).trim().toLowerCase();
    if (!["aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"].includes(method)) return false;
    const parsed = new URL(`ss://${server}`);
    return parsed.username === "" && parsed.password === "" && parsed.hostname !== "" && parsed.port !== ""
      && (parsed.pathname === "" || parsed.pathname === "/") && parsed.search === "" && parsed.hash === "";
  } catch {
    return false;
  }
}

function decodeBase64Text(value: string): string {
  let payload = value.replaceAll("-", "+").replaceAll("_", "/");
  payload += "=".repeat((4 - (payload.length % 4)) % 4);
  const bytes = Uint8Array.from(atob(payload), (character) => character.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

function validTunnelBoolean(query: URLSearchParams, names: string[]): boolean {
  for (const name of names) {
    if (!query.has(name)) continue;
    return ["", "0", "1", "false", "true", "no", "yes"].includes((query.get(name) ?? "").trim().toLowerCase());
  }
  return true;
}

function validWebSocketHost(value: string): boolean {
  try {
    const parsed = new URL(`http://${value.trim()}`);
    return parsed.hostname !== "" && parsed.username === "" && parsed.password === ""
      && (parsed.pathname === "" || parsed.pathname === "/") && parsed.search === "" && parsed.hash === "";
  } catch {
    return false;
  }
}

function validVMessURL(value: string): boolean {
  try {
    let payload = value.slice("vmess://".length).split("#", 1)[0].replaceAll("-", "+").replaceAll("_", "/");
    payload += "=".repeat((4 - (payload.length % 4)) % 4);
    const bytes = Uint8Array.from(atob(payload), (character) => character.charCodeAt(0));
    const config = JSON.parse(new TextDecoder().decode(bytes)) as Record<string, unknown>;
    const id = String(config.id ?? "");
    const port = Number(config.port);
    const network = String(config.net ?? "tcp").toLowerCase();
    const tls = String(config.tls ?? "").toLowerCase();
    const cipher = String(config.scy ?? config.security ?? "auto").toLowerCase();
    const headerType = String(config.type ?? "").toLowerCase();
    const alterID = Number(config.aid ?? 0);
    const webSocket = network === "ws" || network === "websocket";
    const host = String(config.host ?? config.sni ?? config.add ?? "");
    return typeof config.add === "string" && config.add.trim() !== "" && Number.isInteger(port) && port > 0 && port <= 65535
      && /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(id)
      && Number.isInteger(alterID) && alterID >= 0 && alterID <= 65535
      && ["tcp", "ws", "websocket"].includes(network) && ["", "none", "tls"].includes(tls)
      && ["auto", "aes-128-gcm", "chacha20-poly1305", "none"].includes(cipher)
      && ["", "none"].includes(headerType)
      && validJSONTunnelBoolean(config.allowInsecure)
      && (webSocket ? validWebSocketHost(host) : String(config.path ?? "") === "" && String(config.host ?? "") === "");
  } catch {
    return false;
  }
}

function validJSONTunnelBoolean(value: unknown): boolean {
  if (value == null || typeof value === "boolean") return true;
  if (typeof value === "number") return value === 0 || value === 1;
  if (typeof value === "string") return ["", "0", "1", "false", "true", "no", "yes"].includes(value.trim().toLowerCase());
  return false;
}

/** Subscription fetch proxies must never use per-account lease placeholders. */
export function validSubscriptionProxyURL(value: string): boolean {
  return !value.includes("{account}") && validProxyURL(value);
}

// 订阅地址与后端 normalizeSubscriptionURL 同口径:HTTP(S)、有主机名、无
// 片段、无控制字符、长度 <= 8192。
export function validSubscriptionURL(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed || trimmed.length > 8192) return false;
  // eslint-disable-next-line no-control-regex -- 与后端控制字符校验同口径 (0x00-0x1f, 0x7f)
  if (/[\u0000-\u001f\u007f]/.test(trimmed)) return false;
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return false;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return false;
  if (!parsed.hostname) return false;
  return !parsed.hash;
}

function internalSignerHostname(value: string): boolean {
  const host = value.toLowerCase().replace(/^\[|\]$/g, "").replace(/\.$/, "");
  if (host === "localhost" || host.endsWith(".localhost") || host.endsWith(".local") || host.endsWith(".internal")) return true;
  if (!host.includes(".")) {
    if (host.includes(":")) return host === "::1" || /^(?:fc|fd|fe[89ab])/i.test(host);
    return /^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$/i.test(host);
  }
  const octets = host.split(".").map(Number);
  if (octets.length !== 4 || octets.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) return false;
  return octets[0] === 10 || octets[0] === 127 || octets[0] === 169 && octets[1] === 254 || octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31 || octets[0] === 192 && octets[1] === 168;
}
