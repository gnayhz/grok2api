import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, decodeBooleanResult, hasShape, isArrayOf, isBoolean, isNumber, isObject, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";


type EgressNodePoolRef = { id: string; name: string };

export type EgressNodeDTO = {
	id: string; name: string; enabled: boolean;
	proxyConfigured: boolean; proxyDisplay?: string; proxyFingerprint?: string; proxyPool: boolean;
	/** 旋转端点投影(外部代理池标志或账号模板代理):调度豁免与健康态隐藏以此为准。 */
	rotatingEndpoint: boolean;
	sourceId?: string; sourceName?: string; pools?: EgressNodePoolRef[];
	accountBoundProxy: boolean;
	rotationConfigured: boolean; rotationEnabled: boolean; lastRotatedAt?: string; rotationAttempts: number; lastRotationError?: string;
	degradeCount: number; lastDegradedAt?: string;
	health: number; failureCount: number; cooldownUntil?: string; lastError?: string;
	probeStatus: "unknown" | "healthy" | "unhealthy"; lastProbedAt?: string; probeLatencyMs: number; exitIp?: string; probeError?: string; probeProvider?: "ipinfo" | "cloudflare";
	ipv4Probe: EgressIPProbeDTO; ipv6Probe: EgressIPProbeDTO;
	/** 质量轴状态(质量层注入时才有:remanded 羁押/banned IP 禁;缺席=可用)。 */
	quality?: { state: "remanded" | "banned"; caseId?: string };
};

export type EgressNodeInput = {
	name: string; enabled: boolean; proxyPool?: boolean;
	proxyURL?: string; clearProxyURL?: boolean;
	rotationURL?: string; clearRotationURL?: boolean; rotationEnabled?: boolean;
};

export type EgressPoolStrategy = "affinity" | "random" | "sticky" | "rotation" | "least-used";
export type EgressPoolFallbackMode = "none" | "pool" | "direct";

export type EgressRoutingScope = "grok_build" | "grok_web" | "grok_console";
export type EgressTrafficClass = "inference" | "credential" | "billing" | "model_sync" | "video" | "probe";
type EgressRoutingTargetMode = "auto" | "direct" | "node" | "pool";
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
  proxyConfigured: isBoolean, proxyDisplay: isOptional(isString), proxyFingerprint: isOptional(isString), proxyPool: isBoolean, rotatingEndpoint: isBoolean,
  sourceId: isOptional(isString), sourceName: isOptional(isString), pools: isOptional(isArrayOf(egressNodePoolRefValidator)),
  accountBoundProxy: isBoolean,
  rotationConfigured: isBoolean, rotationEnabled: isBoolean, lastRotatedAt: isOptional(isString), rotationAttempts: isNumber, lastRotationError: isOptional(isString),
  degradeCount: isNumber, lastDegradedAt: isOptional(isString),
  health: isNumber, failureCount: isNumber, cooldownUntil: isOptional(isString), lastError: isOptional(isString),
  probeStatus: isOneOf("unknown", "healthy", "unhealthy"), lastProbedAt: isOptional(isString), probeLatencyMs: isNumber, exitIp: isOptional(isString), probeError: isOptional(isString), probeProvider: isOptional(isOneOf("ipinfo", "cloudflare")),
  ipv4Probe: isOptional(egressIPProbeValidator), ipv6Probe: isOptional(egressIPProbeValidator),
  quality: isOptional(isObject),
});

const decodeEgressNodeRaw = createObjectDecoder<EgressNodeWireDTO>("egress node", {
  id: isString, name: isString, enabled: isBoolean,
  proxyConfigured: isBoolean, proxyDisplay: isOptional(isString), proxyFingerprint: isOptional(isString), proxyPool: isBoolean, rotatingEndpoint: isBoolean,
  sourceId: isOptional(isString), sourceName: isOptional(isString), pools: isOptional(isArrayOf(egressNodePoolRefValidator)),
  accountBoundProxy: isBoolean,
  rotationConfigured: isBoolean, rotationEnabled: isBoolean, lastRotatedAt: isOptional(isString), rotationAttempts: isNumber, lastRotationError: isOptional(isString),
  degradeCount: isNumber, lastDegradedAt: isOptional(isString),
  health: isNumber, failureCount: isNumber, cooldownUntil: isOptional(isString), lastError: isOptional(isString),
  probeStatus: isOneOf("unknown", "healthy", "unhealthy"), lastProbedAt: isOptional(isString), probeLatencyMs: isNumber, exitIp: isOptional(isString), probeError: isOptional(isString), probeProvider: isOptional(isOneOf("ipinfo", "cloudflare")),
  ipv4Probe: isOptional(egressIPProbeValidator), ipv6Probe: isOptional(egressIPProbeValidator),
  quality: isOptional(isObject),
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
	strategy: isOneOf("affinity", "random", "sticky", "rotation", "least-used"),
	fallbackMode: isOneOf("none", "pool", "direct"), fallbackPoolId: isOptional(isString), fallbackPoolName: isOptional(isString),
	memberCount: isNumber, healthyCount: isNumber, quarantinedCount: isNumber, memberIds: isArrayOf(isString), preferredNodeId: isOptional(isString), rotationCursorNodeId: isOptional(isString), lastSelectedNodeId: isOptional(isString), createdAt: isString, updatedAt: isString,
});

const decodeEgressPool = createObjectDecoder<EgressPoolDTO>("egress pool", {
	id: isString, name: isString, enabled: isBoolean,
	strategy: isOneOf("affinity", "random", "sticky", "rotation", "least-used"),
	fallbackMode: isOneOf("none", "pool", "direct"), fallbackPoolId: isOptional(isString), fallbackPoolName: isOptional(isString),
	memberCount: isNumber, healthyCount: isNumber, quarantinedCount: isNumber, memberIds: isArrayOf(isString), preferredNodeId: isOptional(isString), rotationCursorNodeId: isOptional(isString), lastSelectedNodeId: isOptional(isString), createdAt: isString, updatedAt: isString,
});

type EgressPoolListWire = { items: EgressPoolDTO[] };
const decodeEgressPools = createObjectDecoder<EgressPoolListWire>("egress pools", { items: isArrayOf(egressPoolValidator) });



type ListEgressNodesInput = {
  page?: number;
  pageSize?: number;
  search?: string;
  enabled?: string;
  probe?: string;
  sortBy?: string;
  sortOrder?: SortOrder;
};

function listEgressNodes(input: ListEgressNodesInput = {}, signal?: AbortSignal): Promise<EgressNodeListDTO> {
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

export async function listAllEgressNodes(input: Omit<ListEgressNodesInput, "page" | "pageSize"> = {}, signal?: AbortSignal): Promise<EgressNodeListDTO> {
  const pageSize = 2000;
  const first = await listEgressNodes({ ...input, page: 1, pageSize }, signal);
  const items = [...first.items];
  for (let page = 2; items.length < first.total; page += 1) {
    const next = await listEgressNodes({ ...input, page, pageSize }, signal);
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

export function getEgressNodeRotationURL(id: string, signal?: AbortSignal): Promise<{ rotationURL: string }> {
	return apiRequest(`/api/admin/v1/egress-nodes/${id}/rotation-url/reveal`, { method: "POST", signal }, createObjectDecoder<{ rotationURL: string }>("egress rotation URL", { rotationURL: isString }));
}

export function getEgressNodeProxyURL(id: string, signal?: AbortSignal): Promise<{ proxyURL: string }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/proxy-url/reveal`, { method: "POST", signal }, createObjectDecoder<{ proxyURL: string }>("egress proxy URL", { proxyURL: isString }));
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

export function listEgressPools(signal?: AbortSignal): Promise<EgressPoolDTO[]> {
	return apiRequest("/api/admin/v1/egress-pools", { signal }, decodeEgressPools).then((value) => value.items);
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

export function listEgressSources(input?: ListEgressSourcesInput, signal?: AbortSignal): Promise<EgressSourceListDTO> {
  if (!input) return apiRequest("/api/admin/v1/egress-sources", { signal }, decodeEgressSourceList);
  const query = new URLSearchParams({ page: String(input.page ?? 1), pageSize: String(input.pageSize ?? 20) });
  if (input.search) query.set("search", input.search);
  return apiRequest(`/api/admin/v1/egress-sources?${query}`, { signal }, decodeEgressSourceList);
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

export function getEgressSourceURL(id: string, signal?: AbortSignal): Promise<{ url: string }> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/url/reveal`, { method: "POST", signal }, createObjectDecoder<{ url: string }>("egress source URL", { url: isString }));
}

export function getEgressSourceProxyURL(id: string, signal?: AbortSignal): Promise<{ proxyURL: string }> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/proxy-url/reveal`, { method: "POST", signal }, createObjectDecoder<{ proxyURL: string }>("egress source proxy URL", { proxyURL: isString }));
}

export function syncEgressSource(id: string): Promise<EgressImportResultDTO> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/sync`, { method: "POST" }, decodeEgressImportResult);
}

export function importEgressText(input: { name: string; content: string }): Promise<EgressImportResultDTO> {
  return apiRequest("/api/admin/v1/egress-imports", { method: "POST", body: input }, decodeEgressImportResult);
}

export function getEgressOperationsConfig(signal?: AbortSignal): Promise<EgressOperationsConfigDTO> {
  return apiRequest("/api/admin/v1/egress-operations", { signal }, decodeEgressOperationsConfig);
}

export function updateEgressOperationsConfig(input: Omit<EgressOperationsConfigDTO, "updatedAt">, signal?: AbortSignal): Promise<EgressOperationsConfigDTO> {
  return apiRequest("/api/admin/v1/egress-operations", { method: "PUT", body: input, signal }, decodeEgressOperationsConfig);
}

export function getEgressRoutingStats(): Promise<{ items: EgressRoutingStatDTO[] }> {
  return apiRequest("/api/admin/v1/egress-operations/routing-stats", {}, decodeEgressRoutingStats);
}
