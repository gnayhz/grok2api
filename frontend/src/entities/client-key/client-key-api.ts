import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
import { createObjectDecoder, createValidatedDecoder, createPaginatedDecoder, decodeBooleanResult, decodeCountResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

export type ClientKeyDTO = {
  id: string;
  name: string;
  prefix: string;
  enabled: boolean;
  expiresAt?: string;
  rpmLimit: number;
  maxConcurrent: number;
  billingLimitUsdTicks: number;
  billedUsageUsdTicks: number;
  reservedUsageUsdTicks?: number;
  allowModelAliases: boolean;
  modelScope: "all" | "restricted";
  allowedModelIds: string[];
  providerScope?: ProviderScopeValue[];
  tierScope?: TierScopeValue[];
  lastUsedAt?: string;
};

export type ClientKeyInput = {
  name: string;
  enabled: boolean;
  expiresAt: string;
  rpmLimit: number;
  maxConcurrent: number;
  billingLimitUsdTicks: number;
  allowModelAliases: boolean;
  modelScope: "all" | "restricted";
  allowedModelIds: string[];
  providerScope: ProviderScopeValue[];
  tierScope: TierScopeValue[];
};

export type ProviderScopeValue = "all" | "grok_build" | "grok_web" | "grok_console";
export type TierScopeValue = "all" | "free" | "super";

export type CreateKeyResponseDTO = { key: ClientKeyDTO; secret: string };

const clientKeyValidator = hasShape({
  id: isString, name: isString, prefix: isString, enabled: isBoolean, expiresAt: isOptional(isString),
  rpmLimit: isNumber, maxConcurrent: isNumber, billingLimitUsdTicks: isNumber, billedUsageUsdTicks: isNumber, reservedUsageUsdTicks: isOptional(isNumber),
  allowModelAliases: isBoolean, modelScope: isOneOf("all", "restricted"), allowedModelIds: isArrayOf(isString), providerScope: isOptional(isArrayOf(isOneOf("all", "grok_build", "grok_web", "grok_console"))), tierScope: isOptional(isArrayOf(isOneOf("all", "free", "super"))), lastUsedAt: isOptional(isString),
});
const decodeClientKey = createValidatedDecoder<ClientKeyDTO>("client key", clientKeyValidator);
const decodeClientKeyPage = createPaginatedDecoder<ClientKeyDTO>(clientKeyValidator);
const decodeCreatedClientKey = createObjectDecoder<CreateKeyResponseDTO>("created client key", { key: clientKeyValidator, secret: isString });
const decodeSecret = createObjectDecoder<{ secret: string }>("client key secret", { secret: isString });

type ListClientKeysInput = {
  page: number;
  pageSize: number;
  search?: string;
  status?: string;
  modelScope?: string;
  sortBy?: string;
  sortOrder?: SortOrder;
};

export function listClientKeys(input: ListClientKeysInput, signal?: AbortSignal): Promise<PaginatedDTO<ClientKeyDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.modelScope) query.set("modelScope", input.modelScope);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/client-keys?${query}`, { signal }, decodeClientKeyPage);
}

export function createClientKey(input: ClientKeyInput, signal?: AbortSignal): Promise<CreateKeyResponseDTO> {
  return apiRequest("/api/admin/v1/client-keys", { signal, method: "POST", body: input }, decodeCreatedClientKey);
}

export function getClientKeySecret(id: string, signal?: AbortSignal): Promise<{ secret: string }> {
  return apiRequest(`/api/admin/v1/client-keys/${id}/secret`, { signal }, decodeSecret);
}

export function updateClientKey(id: string, input: Partial<ClientKeyInput>, signal?: AbortSignal): Promise<ClientKeyDTO> {
  return apiRequest(`/api/admin/v1/client-keys/${id}`, { signal, method: "PATCH", body: input }, decodeClientKey);
}

export function deleteClientKey(id: string, signal?: AbortSignal): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/client-keys/${id}`, { signal, method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function updateClientKeysEnabled(ids: string[], enabled: boolean, signal?: AbortSignal): Promise<{ updated: number }> {
  return apiRequest("/api/admin/v1/client-keys/batch", { signal, method: "PATCH", body: { ids, enabled } }, decodeCountResult<{ updated: number }>("updated"));
}

export function deleteClientKeys(ids: string[], signal?: AbortSignal): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/client-keys", { signal, method: "DELETE", body: { ids } }, decodeCountResult<{ deleted: number }>("deleted"));
}
