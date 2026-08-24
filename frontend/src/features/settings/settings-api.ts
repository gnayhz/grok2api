import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, decodeBooleanResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

export type SettingsConfigDTO = {
  server: { maxConcurrentRequests: number };
  providerBuild: { baseURL: string; fallbackBaseURL: string; clientVersion: string; clientIdentifier: string; tokenAuth: string; tokenAuthConfigured: boolean; userAgent: string; responseHeaderTimeout: string; streamIdleTimeout: string };
  providerWeb: {
    baseURL: string; quotaTimeout: string; chatTimeout: string; streamIdleTimeout: string; imageTimeout: string; videoTimeout: string;
    statsigMode: "manual" | "url"; statsigManualValue?: string; statsigManualConfigured: boolean; statsigSignerURL: string;
    clearanceMode: ClearanceMode; flareSolverrURL: string; clearanceTimeout: string; clearanceRefresh: string;
    mediaConcurrency: number; allowNSFW: boolean;
    recoveryBackoffBase: string; recoveryBackoffMax: string;
  };
  providerConsole: { baseURL: string; chatTimeout: string; streamIdleTimeout: string };
  batch: { importConcurrency: number; conversionConcurrency: number; syncConcurrency: number; refreshConcurrency: number; randomDelay: string };
  media: {
    maxImageBytes: number; maxTotalBytes: number; cleanupThresholdPercent: number;
    cleanupInterval: string;
  };
  frontend: { publicApiBaseURL: string };
  routing: {
    stickyTTL: string; cooldownBase: string; cooldownMax: string; capacityWait: string; maxAttempts: number; videoMaxAttempts: number; preferFreeBuild: boolean; markBuildChatDeniedAsReauth: boolean;
    accountIsolatedConnections: boolean;
    segmentedSelector: { enabled: boolean; minCandidates: number; windowSize: number };
  };
  audit: { bufferSize: number; batchSize: number; flushInterval: string; commitDelayMS: number; retentionDays?: number };
  clientKeyDefaults: { rpmLimit: number; maxConcurrent: number };
  accounts: {
    markBuildForbiddenReauth: boolean;
    buildForbiddenReauthCodes: string[];
    excludeBuildBotFlaggedFromScheduling: boolean;
    autoCleanReauthEnabled: boolean;
    autoCleanReauthInterval: string;
    autoCleanReauthMinAge: string;
    autoCleanIncludeDisabled: boolean;
  };
};

export type ClearanceMode = "manual" | "flaresolverr" | "on_demand";

export type EgressNodePoolRef = { id: string; name: string };

export type EgressNodeDTO = {
	id: string; name: string; enabled: boolean;
	proxyConfigured: boolean; proxyDisplay?: string; proxyFingerprint?: string; proxyPool: boolean;
	sourceId?: string; sourceName?: string; pools?: EgressNodePoolRef[];
	accountBoundProxy: boolean;
	rotationConfigured: boolean; rotationEnabled: boolean; lastRotatedAt?: string; rotationAttempts: number; lastRotationError?: string;
	degradeCount: number; lastDegradedAt?: string;
	health: number; failureCount: number; cooldownUntil?: string; lastError?: string;
	probeStatus: "unknown" | "healthy" | "unhealthy"; lastProbedAt?: string; probeLatencyMs: number; exitIp?: string; probeError?: string; probeProvider?: "ipinfo" | "cloudflare";
	ipv4Probe: EgressIPProbeDTO; ipv6Probe: EgressIPProbeDTO;
};

export type EgressNodeInput = {
	name: string; enabled: boolean; proxyPool?: boolean;
	proxyURL?: string; clearProxyURL?: boolean;
	rotationURL?: string; clearRotationURL?: boolean; rotationEnabled?: boolean;
};

export type EgressPoolStrategy = "affinity" | "random" | "sticky" | "rotation";
export type EgressPoolFallbackMode = "none" | "pool" | "direct";

export type EgressRoutingScope = "grok_build" | "grok_web" | "grok_console";
export type EgressTrafficClass = "inference" | "credential" | "billing" | "model_sync" | "video";
export type EgressRoutingTargetMode = "auto" | "direct" | "node" | "pool";
export type EgressRoutingTarget = { mode: EgressRoutingTargetMode; nodeId?: string; poolId?: string };

export type EgressNodeListDTO = {
  items: EgressNodeDTO[];
  page: number;
  pageSize: number;
  total: number;
  };

export type EgressSourceDTO = {
  id: string; name: string; enabled: boolean; urlConfigured: boolean; proxyConfigured: boolean;
  poolId?: string; poolName?: string; refreshIntervalSeconds: number;
  lastSyncedAt?: string; nextSyncAt?: string; lastSyncImported: number; lastSyncError?: string;
};

export type EgressSourceListDTO = {
  items: EgressSourceDTO[];
  page: number;
  pageSize: number;
  total: number;
};

export type EgressSourceInput = {
  name: string; enabled: boolean; url?: string; clearUrl?: boolean;
  proxyURL?: string; clearProxyURL?: boolean; refreshIntervalSeconds?: number; poolId?: string;
};

export type EgressOperationsConfigDTO = {
  probeProvider: "ipinfo" | "cloudflare";
  probeIntervalSeconds: number;
  defaultTarget: EgressRoutingTarget;
  scopeTargets: Partial<Record<EgressRoutingScope, EgressRoutingTarget>>;
  classTargets: Partial<Record<EgressTrafficClass, EgressRoutingTarget>>;
  updatedAt: string;
};

export type EgressRoutingStatDTO = {
  level: string; mode: string; hit: number; fallback: number; lastSeen?: string;
};

export type EgressImportResultDTO = { imported: number; skipped: number };
export type EgressIPProbeDTO = { status: "unknown" | "healthy" | "unhealthy"; testedAt?: string; latencyMs: number; exitIp?: string; error?: string };
export type EgressProbeResultDTO = { status: "unknown" | "healthy" | "unhealthy"; testedAt: string; latencyMs: number; exitIp?: string; error?: string; probeProvider?: "ipinfo" | "cloudflare"; ipv4: EgressIPProbeDTO; ipv6: EgressIPProbeDTO };
export type EgressProbeBatchResultDTO = { requested: number; healthy: number; unhealthy: number };
export type EgressUnhealthyCleanupPreviewDTO = { nodes: number; subscriptionManaged: number };

export type SettingsSnapshotDTO = {
  config: SettingsConfigDTO;
  recommendedProviderBuild: { clientVersion: string; userAgent: string };
  updatedAt: string;
  revision: string;
  restartRequired: string[];
};

const settingsConfigValidator = hasShape({
  server: hasShape({ maxConcurrentRequests: isNumber }),
  providerBuild: hasShape({ baseURL: isString, fallbackBaseURL: isString, clientVersion: isString, clientIdentifier: isString, tokenAuth: isString, tokenAuthConfigured: isBoolean, userAgent: isString, responseHeaderTimeout: isString, streamIdleTimeout: isString }),
  providerWeb: hasShape({
    baseURL: isString, quotaTimeout: isString, chatTimeout: isString, streamIdleTimeout: isOptional(isString), imageTimeout: isString, videoTimeout: isString,
    statsigMode: isOneOf("manual", "url"), statsigManualValue: isOptional(isString), statsigManualConfigured: isBoolean,
    statsigSignerURL: isString, clearanceMode: isOneOf("manual", "flaresolverr", "on_demand"), flareSolverrURL: isString,
    clearanceTimeout: isString, clearanceRefresh: isString, mediaConcurrency: isNumber, allowNSFW: isBoolean, recoveryBackoffBase: isString, recoveryBackoffMax: isString,
  }),
  providerConsole: hasShape({ baseURL: isString, chatTimeout: isString, streamIdleTimeout: isOptional(isString) }),
  batch: hasShape({ importConcurrency: isNumber, conversionConcurrency: isNumber, syncConcurrency: isNumber, refreshConcurrency: isNumber, randomDelay: isString }),
  media: hasShape({ maxImageBytes: isNumber, maxTotalBytes: isNumber, cleanupThresholdPercent: isNumber, cleanupInterval: isString }),
  frontend: hasShape({ publicApiBaseURL: isString }),
  routing: hasShape({
    stickyTTL: isString, cooldownBase: isString, cooldownMax: isString, capacityWait: isString, maxAttempts: isNumber, videoMaxAttempts: isNumber, preferFreeBuild: isBoolean, markBuildChatDeniedAsReauth: isBoolean,
    accountIsolatedConnections: isOptional(isBoolean),
    segmentedSelector: isOptional(hasShape({ enabled: isBoolean, minCandidates: isNumber, windowSize: isNumber })),
  }),
  audit: hasShape({
    bufferSize: isNumber, batchSize: isNumber, flushInterval: isString, commitDelayMS: isOptional(isNumber),
    retentionDays: isOptional(isNumber),
  }),
  clientKeyDefaults: hasShape({ rpmLimit: isNumber, maxConcurrent: isNumber }),
  // Older backends may omit accounts; withSettingsDefaults supplies a safe local default.
  accounts: isOptional(hasShape({
    markBuildForbiddenReauth: isOptional(isBoolean),
    buildForbiddenReauthCodes: isOptional(isArrayOf(isString)),
    excludeBuildBotFlaggedFromScheduling: isOptional(isBoolean),
    autoCleanReauthEnabled: isBoolean,
    autoCleanReauthInterval: isString,
    autoCleanReauthMinAge: isString,
    autoCleanIncludeDisabled: isBoolean,
  })),
});
const defaultAccountsConfig = (): SettingsConfigDTO["accounts"] => ({
  markBuildForbiddenReauth: false,
  buildForbiddenReauthCodes: ["permission-denied"],
  excludeBuildBotFlaggedFromScheduling: false,
  autoCleanReauthEnabled: false,
  autoCleanReauthInterval: "10m",
  autoCleanReauthMinAge: "1h",
  autoCleanIncludeDisabled: false,
});
function withSettingsDefaults(snapshot: SettingsSnapshotDTO): SettingsSnapshotDTO {
  const accounts = snapshot.config.accounts ?? defaultAccountsConfig();
  const segmentedSelector = snapshot.config.routing.segmentedSelector ?? { enabled: true, minCandidates: 3000, windowSize: 64 };
  return {
    ...snapshot,
    config: {
      ...snapshot.config,
      providerWeb: {
        ...snapshot.config.providerWeb,
        streamIdleTimeout: snapshot.config.providerWeb.streamIdleTimeout || "1m30s",
      },
      providerConsole: {
        ...snapshot.config.providerConsole,
        streamIdleTimeout: snapshot.config.providerConsole.streamIdleTimeout || "2m",
      },
      audit: {
        ...snapshot.config.audit,
        commitDelayMS: snapshot.config.audit.commitDelayMS ?? 5,
        retentionDays: snapshot.config.audit.retentionDays ?? 7,
      },
      routing: {
        ...snapshot.config.routing,
        markBuildChatDeniedAsReauth: snapshot.config.routing.markBuildChatDeniedAsReauth ?? false,
        accountIsolatedConnections: snapshot.config.routing.accountIsolatedConnections ?? false,
        segmentedSelector: {
          enabled: segmentedSelector.enabled ?? true,
          minCandidates: segmentedSelector.minCandidates || 3000,
          windowSize: segmentedSelector.windowSize || 64,
        },
      },
      accounts: {
        markBuildForbiddenReauth: accounts.markBuildForbiddenReauth ?? false,
        buildForbiddenReauthCodes: accounts.buildForbiddenReauthCodes ?? ["permission-denied"],
        excludeBuildBotFlaggedFromScheduling: accounts.excludeBuildBotFlaggedFromScheduling ?? false,
        autoCleanReauthEnabled: accounts.autoCleanReauthEnabled ?? false,
        autoCleanReauthInterval: accounts.autoCleanReauthInterval || "10m",
        autoCleanReauthMinAge: accounts.autoCleanReauthMinAge || "1h",
        autoCleanIncludeDisabled: accounts.autoCleanIncludeDisabled ?? false,
      },
    },
  };
}
const decodeSettingsSnapshotRaw = createObjectDecoder<SettingsSnapshotDTO>("settings", {
  config: settingsConfigValidator,
  recommendedProviderBuild: hasShape({ clientVersion: isString, userAgent: isString }),
  updatedAt: isString,
  revision: isString,
  restartRequired: isArrayOf(isString),
});
const decodeSettingsSnapshot = (value: unknown) => withSettingsDefaults(decodeSettingsSnapshotRaw(value));
const egressIPProbeValidator = hasShape({
  status: isOneOf("unknown", "healthy", "unhealthy"), testedAt: isOptional(isString), latencyMs: isNumber, exitIp: isOptional(isString), error: isOptional(isString),
});
type EgressNodeWireDTO = Omit<EgressNodeDTO, "ipv4Probe" | "ipv6Probe"> & { ipv4Probe?: EgressIPProbeDTO; ipv6Probe?: EgressIPProbeDTO };
type EgressSourceWireDTO = Omit<EgressSourceDTO, "proxyConfigured"> & { proxyConfigured?: boolean };
type EgressOperationsConfigWireDTO = Omit<EgressOperationsConfigDTO, "probeProvider" | "defaultTarget" | "scopeTargets" | "classTargets"> & {
  probeProvider?: "ipinfo" | "cloudflare";
  defaultTarget?: EgressRoutingTarget;
  scopeTargets?: Partial<Record<EgressRoutingScope, EgressRoutingTarget>>;
  classTargets?: Partial<Record<EgressTrafficClass, EgressRoutingTarget>>;
};
type EgressProbeResultWireDTO = Omit<EgressProbeResultDTO, "ipv4" | "ipv6"> & { ipv4?: EgressIPProbeDTO; ipv6?: EgressIPProbeDTO };
const unknownEgressIPProbe = (): EgressIPProbeDTO => ({ status: "unknown", latencyMs: 0 });
const withEgressNodeProbeDefaults = (value: EgressNodeWireDTO): EgressNodeDTO => ({
  ...value,
  ipv4Probe: value.ipv4Probe ?? unknownEgressIPProbe(),
  ipv6Probe: value.ipv6Probe ?? unknownEgressIPProbe(),
});
const withEgressSourceDefaults = (value: EgressSourceWireDTO): EgressSourceDTO => ({ ...value, proxyConfigured: value.proxyConfigured ?? false });
const egressNodePoolRefValidator = hasShape({ id: isString, name: isString });

const egressNodeValidator = hasShape({
  id: isString, name: isString, enabled: isBoolean,
  proxyConfigured: isBoolean, proxyDisplay: isOptional(isString), proxyFingerprint: isOptional(isString), proxyPool: isBoolean,
  sourceId: isOptional(isString), sourceName: isOptional(isString), pools: isOptional(isArrayOf(egressNodePoolRefValidator)),
  accountBoundProxy: isBoolean,
  rotationConfigured: isBoolean, rotationEnabled: isBoolean, lastRotatedAt: isOptional(isString), rotationAttempts: isNumber, lastRotationError: isOptional(isString),
  degradeCount: isNumber, lastDegradedAt: isOptional(isString),
  health: isNumber, failureCount: isNumber, cooldownUntil: isOptional(isString), lastError: isOptional(isString),
  probeStatus: isOneOf("unknown", "healthy", "unhealthy"), lastProbedAt: isOptional(isString), probeLatencyMs: isNumber, exitIp: isOptional(isString), probeError: isOptional(isString), probeProvider: isOptional(isOneOf("ipinfo", "cloudflare")),
  ipv4Probe: isOptional(egressIPProbeValidator), ipv6Probe: isOptional(egressIPProbeValidator),
});

const decodeEgressNodeRaw = createObjectDecoder<EgressNodeWireDTO>("egress node", {
  id: isString, name: isString, enabled: isBoolean,
  proxyConfigured: isBoolean, proxyDisplay: isOptional(isString), proxyFingerprint: isOptional(isString), proxyPool: isBoolean,
  sourceId: isOptional(isString), sourceName: isOptional(isString), pools: isOptional(isArrayOf(egressNodePoolRefValidator)),
  accountBoundProxy: isBoolean,
  rotationConfigured: isBoolean, rotationEnabled: isBoolean, lastRotatedAt: isOptional(isString), rotationAttempts: isNumber, lastRotationError: isOptional(isString),
  degradeCount: isNumber, lastDegradedAt: isOptional(isString),
  health: isNumber, failureCount: isNumber, cooldownUntil: isOptional(isString), lastError: isOptional(isString),
  probeStatus: isOneOf("unknown", "healthy", "unhealthy"), lastProbedAt: isOptional(isString), probeLatencyMs: isNumber, exitIp: isOptional(isString), probeError: isOptional(isString), probeProvider: isOptional(isOneOf("ipinfo", "cloudflare")),
  ipv4Probe: isOptional(egressIPProbeValidator), ipv6Probe: isOptional(egressIPProbeValidator),
});
const decodeEgressNode = (value: unknown) => withEgressNodeProbeDefaults(decodeEgressNodeRaw(value));
type EgressNodeListWireDTO = {
  items: EgressNodeWireDTO[];
  page?: number;
  pageSize?: number;
  total?: number;
  };
const decodeEgressNodeListRaw = createObjectDecoder<EgressNodeListWireDTO>("egress node list", {
  items: isArrayOf(egressNodeValidator),
  page: isOptional(isNumber),
  pageSize: isOptional(isNumber),
  total: isOptional(isNumber),
  });
const decodeEgressNodeList = (value: unknown): EgressNodeListDTO => {
  const decoded = decodeEgressNodeListRaw(value);
  return {
    ...decoded,
    items: decoded.items.map(withEgressNodeProbeDefaults),
    page: decoded.page ?? 1,
    pageSize: decoded.pageSize ?? Math.max(20, decoded.items.length),
    total: decoded.total ?? decoded.items.length,
  };
};
const egressSourceValidator = hasShape({
  id: isString, name: isString, enabled: isBoolean, urlConfigured: isBoolean,
  proxyConfigured: isOptional(isBoolean),
  refreshIntervalSeconds: isNumber, lastSyncedAt: isOptional(isString), nextSyncAt: isOptional(isString),
  lastSyncImported: isNumber, lastSyncError: isOptional(isString),
});
const decodeEgressSourceRaw = createObjectDecoder<EgressSourceWireDTO>("egress source", {
  id: isString, name: isString, enabled: isBoolean, urlConfigured: isBoolean,
  proxyConfigured: isOptional(isBoolean),
  refreshIntervalSeconds: isNumber, lastSyncedAt: isOptional(isString), nextSyncAt: isOptional(isString),
  lastSyncImported: isNumber, lastSyncError: isOptional(isString),
});
const decodeEgressSource = (value: unknown) => withEgressSourceDefaults(decodeEgressSourceRaw(value));
type EgressSourceListWireDTO = {
  items: EgressSourceWireDTO[];
  page?: number;
  pageSize?: number;
  total?: number;
};
const decodeEgressSourceListRaw = createObjectDecoder<EgressSourceListWireDTO>("egress source list", {
  items: isArrayOf(egressSourceValidator), page: isOptional(isNumber), pageSize: isOptional(isNumber), total: isOptional(isNumber),
});
const decodeEgressSourceList = (value: unknown): EgressSourceListDTO => {
  const decoded = decodeEgressSourceListRaw(value);
  return {
    ...decoded,
    items: decoded.items.map(withEgressSourceDefaults),
    page: decoded.page ?? 1,
    pageSize: decoded.pageSize ?? Math.max(20, decoded.items.length),
    total: decoded.total ?? decoded.items.length,
  };
};
const decodeEgressImportResult = createObjectDecoder<EgressImportResultDTO>("egress import result", { imported: isNumber, skipped: isNumber });
const decodeEgressProbeBatchResult = createObjectDecoder<EgressProbeBatchResultDTO>("egress probe result", { requested: isNumber, healthy: isNumber, unhealthy: isNumber });
const egressRoutingTargetValidator = hasShape({ mode: isOneOf("auto", "direct", "node", "pool"), nodeId: isOptional(isString), poolId: isOptional(isString) });
const decodeEgressOperationsConfigRaw = createObjectDecoder<EgressOperationsConfigWireDTO>("egress operations config", {
  probeProvider: isOptional(isOneOf("ipinfo", "cloudflare")), probeIntervalSeconds: isNumber,
  defaultTarget: isOptional(egressRoutingTargetValidator),
  scopeTargets: isOptional(isRecordOf(egressRoutingTargetValidator)),
  classTargets: isOptional(isRecordOf(egressRoutingTargetValidator)),
  updatedAt: isString,
});
const decodeEgressOperationsConfig = (value: unknown): EgressOperationsConfigDTO => {
  const decoded = decodeEgressOperationsConfigRaw(value);
  return {
    ...decoded,
    probeProvider: decoded.probeProvider ?? "cloudflare",
    defaultTarget: decoded.defaultTarget ?? { mode: "auto" },
    scopeTargets: decoded.scopeTargets ?? {},
    classTargets: decoded.classTargets ?? {},
  };
};
const decodeEgressRoutingStats = createObjectDecoder<{ items: EgressRoutingStatDTO[] }>("egress routing stats", {
  items: isArrayOf(hasShape({
    level: isString, mode: isString, hit: isNumber, fallback: isNumber,
    lastSeen: isOptional(isString),
  })),
});
const decodeEgressProbeResultRaw = createObjectDecoder<EgressProbeResultWireDTO>("egress probe", {
  status: isOneOf("unknown", "healthy", "unhealthy"), testedAt: isString, latencyMs: isNumber, exitIp: isOptional(isString), error: isOptional(isString), probeProvider: isOptional(isOneOf("ipinfo", "cloudflare")),
  ipv4: isOptional(egressIPProbeValidator), ipv6: isOptional(egressIPProbeValidator),
});
const decodeEgressProbeResult = (value: unknown): EgressProbeResultDTO => {
  const decoded = decodeEgressProbeResultRaw(value);
  return { ...decoded, ipv4: decoded.ipv4 ?? unknownEgressIPProbe(), ipv6: decoded.ipv6 ?? unknownEgressIPProbe() };
};

const egressPoolValidator = hasShape({
	id: isString, name: isString, enabled: isBoolean,
	strategy: isOneOf("affinity", "random", "sticky", "rotation"),
	fallbackMode: isOneOf("none", "pool", "direct"), fallbackPoolId: isOptional(isString), fallbackPoolName: isOptional(isString),
	memberCount: isNumber, healthyCount: isNumber, quarantinedCount: isNumber, memberIds: isArrayOf(isString), preferredNodeId: isOptional(isString), rotationCursorNodeId: isOptional(isString), lastSelectedNodeId: isOptional(isString), createdAt: isString, updatedAt: isString,
});

const decodeEgressPool = createObjectDecoder<EgressPoolDTO>("egress pool", {
	id: isString, name: isString, enabled: isBoolean,
	strategy: isOneOf("affinity", "random", "sticky", "rotation"),
	fallbackMode: isOneOf("none", "pool", "direct"), fallbackPoolId: isOptional(isString), fallbackPoolName: isOptional(isString),
	memberCount: isNumber, healthyCount: isNumber, quarantinedCount: isNumber, memberIds: isArrayOf(isString), preferredNodeId: isOptional(isString), rotationCursorNodeId: isOptional(isString), lastSelectedNodeId: isOptional(isString), createdAt: isString, updatedAt: isString,
});

type EgressPoolListWire = { items: EgressPoolDTO[] };
const decodeEgressPools = createObjectDecoder<EgressPoolListWire>("egress pools", { items: isArrayOf(egressPoolValidator) });


export function getSettings(): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", {}, decodeSettingsSnapshot);
}

export function updateSettings(revision: string, config: SettingsConfigDTO): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { method: "PUT", body: { revision, config } }, decodeSettingsSnapshot);
}

type ListEgressNodesInput = {
  page?: number;
  pageSize?: number;
  search?: string;
  enabled?: string;
  probe?: string;
  sortBy?: string;
  sortOrder?: SortOrder;
};

export function listEgressNodes(input: ListEgressNodesInput = {}, signal?: AbortSignal): Promise<EgressNodeListDTO> {
  const query = new URLSearchParams({ page: String(input.page ?? 1), pageSize: String(input.pageSize ?? 20) });
  if (input.search) query.set("search", input.search);
  if (input.enabled) query.set("enabled", input.enabled);
  if (input.probe) query.set("probe", input.probe);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/egress-nodes?${query}`, { signal }, decodeEgressNodeList);
}

export async function listAllEgressNodes(input: Omit<ListEgressNodesInput, "page" | "pageSize"> = {}): Promise<EgressNodeListDTO> {
  const pageSize = 2000;
  const first = await listEgressNodes({ ...input, page: 1, pageSize });
  const items = [...first.items];
  for (let page = 2; items.length < first.total; page += 1) {
    const next = await listEgressNodes({ ...input, page, pageSize });
    if (next.items.length === 0) break;
    items.push(...next.items);
  }
  return { ...first, items, page: 1, pageSize, total: items.length };
}

export function createEgressNode(input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest("/api/admin/v1/egress-nodes", { method: "POST", body: input }, decodeEgressNode);
}

export function updateEgressNode(id: string, input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "PUT", body: input }, decodeEgressNode);
}

export function getEgressNodeRotationURL(id: string): Promise<{ rotationURL: string }> {
	return apiRequest(`/api/admin/v1/egress-nodes/${id}/rotation-url/reveal`, { method: "POST" }, createObjectDecoder<{ rotationURL: string }>("egress rotation URL", { rotationURL: isString }));
}

export function getEgressNodeProxyURL(id: string): Promise<{ proxyURL: string }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/proxy-url/reveal`, { method: "POST" }, createObjectDecoder<{ proxyURL: string }>("egress proxy URL", { proxyURL: isString }));
}

export function deleteEgressNode(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function deleteEgressNodes(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/egress-nodes", { method: "DELETE", body: { ids } }, createObjectDecoder<{ deleted: number }>("egress node batch delete", { deleted: isNumber }));
}

export function updateEgressNodesEnabled(ids: string[], enabled: boolean): Promise<{ updated: number }> {
  return apiRequest("/api/admin/v1/egress-nodes/batch", { method: "PATCH", body: { ids, enabled } }, createObjectDecoder<{ updated: number }>("egress node batch update", { updated: isNumber }));
}

export function previewUnhealthyEgressNodes(): Promise<EgressUnhealthyCleanupPreviewDTO> {
  return apiRequest("/api/admin/v1/egress-nodes/cleanup-preview", {}, createObjectDecoder<EgressUnhealthyCleanupPreviewDTO>("egress node cleanup preview", {
    nodes: isNumber, subscriptionManaged: isNumber,
  }));
}

export function cleanupUnhealthyEgressNodes(): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/egress-nodes/cleanup", { method: "POST" }, createObjectDecoder<{ deleted: number }>("egress node cleanup", { deleted: isNumber }));
}

export function refreshEgressClearance(id: string): Promise<{ refreshed: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/refresh-clearance`, { method: "POST" }, decodeBooleanResult<{ refreshed: boolean }>("refreshed"));
}

export function rotateEgressNode(id: string): Promise<{ queued: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/rotate`, { method: "POST" }, createObjectDecoder<{ queued: boolean }>("egress node rotate", { queued: isBoolean }));
}

export function testEgressNode(id: string): Promise<EgressProbeResultDTO> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/test`, { method: "POST" }, decodeEgressProbeResult);
}

export function batchSetEgressRotation(ids: string[], template: string): Promise<{ updated: number; skipped: number }> {
  return apiRequest("/api/admin/v1/egress-nodes/batch-rotation", { method: "POST", body: { ids, template } }, createObjectDecoder<{ updated: number; skipped: number }>("egress batch rotation", { updated: isNumber, skipped: isNumber }));
}

export function testEgressNodes(ids?: string[]): Promise<EgressProbeBatchResultDTO> {
  return apiRequest("/api/admin/v1/egress-nodes/test", { method: "POST", body: { ids: ids ?? [] } }, decodeEgressProbeBatchResult);
}

export type EgressPoolDTO = {
	id: string; name: string; enabled: boolean;
	strategy: EgressPoolStrategy;
	fallbackMode: EgressPoolFallbackMode; fallbackPoolId?: string; fallbackPoolName?: string;
	memberCount: number; healthyCount: number; quarantinedCount: number; memberIds: string[];
	preferredNodeId?: string;
	rotationCursorNodeId?: string;
	lastSelectedNodeId?: string;
	createdAt: string; updatedAt: string;
};

export type EgressPoolInput = {
	name: string; enabled: boolean;
	strategy: EgressPoolStrategy;
	fallbackMode: EgressPoolFallbackMode; fallbackPoolId?: string;
};

export function listEgressPools(): Promise<EgressPoolDTO[]> {
	return apiRequest("/api/admin/v1/egress-pools", {}, decodeEgressPools).then((value) => value.items);
}

/** 池内节点调度统计：验证策略分布的进程内存计数，重启/清零归零。 */
export type EgressPoolStatDTO = {
	nodeId: string; selections: number; failures: number; lastSelectedAt?: string;
};

export function getEgressPoolStats(id: string): Promise<{ items: EgressPoolStatDTO[]; since: string }> {
	return apiRequest(`/api/admin/v1/egress-pools/${id}/stats`, {}, createObjectDecoder<{ items: EgressPoolStatDTO[]; since: string }>("egress pool stats", {
		items: isArrayOf(hasShape({ nodeId: isString, selections: isNumber, failures: isNumber, lastSelectedAt: isOptional(isString) })),
		since: isString,
	}));
}

export function resetEgressPoolStats(id: string): Promise<void> {
	return apiRequest(`/api/admin/v1/egress-pools/${id}/stats`, { method: "DELETE" }, createObjectDecoder<{ reset: boolean }>("egress pool stats reset", { reset: isBoolean })).then(() => undefined);
}
/** 设置池内成员首选顺序（小者先；首选优先/节点轮询的“首”）。 */
export function setEgressPoolMemberPriority(id: string, nodeId: string, priority: number): Promise<void> {
	return apiRequest(`/api/admin/v1/egress-pools/${id}/members/${nodeId}/priority`, { method: "PUT", body: { priority } }, createObjectDecoder<{ updated: boolean }>("egress pool member priority", { updated: isBoolean })).then(() => undefined);
}

export function createEgressPool(input: EgressPoolInput): Promise<EgressPoolDTO> {
	return apiRequest("/api/admin/v1/egress-pools", { method: "POST", body: input }, decodeEgressPool);
}

export function updateEgressPool(id: string, input: EgressPoolInput): Promise<EgressPoolDTO> {
	return apiRequest(`/api/admin/v1/egress-pools/${id}`, { method: "PUT", body: input }, decodeEgressPool);
}

export function deleteEgressPool(id: string): Promise<{ deleted: boolean }> {
	return apiRequest(`/api/admin/v1/egress-pools/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function setEgressPoolMembers(poolId: string, nodeIds: string[]): Promise<{ updated: boolean }> {
	return apiRequest(`/api/admin/v1/egress-pools/${poolId}/members`, { method: "PUT", body: { ids: nodeIds } }, createObjectDecoder<{ updated: boolean }>("set pool members", { updated: isBoolean }));
}

type ListEgressSourcesInput = {
  page?: number;
  pageSize?: number;
  search?: string;
};

export function listEgressSources(input?: ListEgressSourcesInput): Promise<EgressSourceListDTO> {
  if (!input) return apiRequest("/api/admin/v1/egress-sources", {}, decodeEgressSourceList);
  const query = new URLSearchParams({ page: String(input.page ?? 1), pageSize: String(input.pageSize ?? 20) });
  if (input.search) query.set("search", input.search);
  return apiRequest(`/api/admin/v1/egress-sources?${query}`, {}, decodeEgressSourceList);
}

export function createEgressSource(input: EgressSourceInput): Promise<EgressSourceDTO> {
  return apiRequest("/api/admin/v1/egress-sources", { method: "POST", body: input }, decodeEgressSource);
}

export function updateEgressSource(id: string, input: EgressSourceInput): Promise<EgressSourceDTO> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}`, { method: "PUT", body: input }, decodeEgressSource);
}

export function deleteEgressSource(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function getEgressSourceURL(id: string): Promise<{ url: string }> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/url/reveal`, { method: "POST" }, createObjectDecoder<{ url: string }>("egress source URL", { url: isString }));
}

export function getEgressSourceProxyURL(id: string): Promise<{ proxyURL: string }> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/proxy-url/reveal`, { method: "POST" }, createObjectDecoder<{ proxyURL: string }>("egress source proxy URL", { proxyURL: isString }));
}

export function syncEgressSource(id: string): Promise<EgressImportResultDTO> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/sync`, { method: "POST" }, decodeEgressImportResult);
}

export function importEgressText(input: { name: string; content: string }): Promise<EgressImportResultDTO> {
  return apiRequest("/api/admin/v1/egress-imports", { method: "POST", body: input }, decodeEgressImportResult);
}

export function getEgressOperationsConfig(): Promise<EgressOperationsConfigDTO> {
  return apiRequest("/api/admin/v1/egress-operations", {}, decodeEgressOperationsConfig);
}

export function updateEgressOperationsConfig(input: Omit<EgressOperationsConfigDTO, "updatedAt">): Promise<EgressOperationsConfigDTO> {
  return apiRequest("/api/admin/v1/egress-operations", { method: "PUT", body: input }, decodeEgressOperationsConfig);
}

export function getEgressRoutingStats(): Promise<{ items: EgressRoutingStatDTO[] }> {
  return apiRequest("/api/admin/v1/egress-operations/routing-stats", {}, decodeEgressRoutingStats);
}