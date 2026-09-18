import { apiRequest } from "@/shared/api/client";
import { createValidatedDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";

export type ResourceKind = "account" | "node";
export type ResourceTarget = { id: string; name: string };
export type ResourceSample = {
  sample: string; outcome: string; failure?: string; thinking: boolean; completed: boolean; usage_reported: boolean;
  input_tokens: number; path_key?: string; path_family?: number; path_verified: boolean;
  attempt: { id: string; account_id: string; model: string; path: { node_id?: string; status: string } };
};
export type ResourceGroup = {
  control_account: string; control_node: string; account_id: string; node_id: string;
  control: ResourceSample[]; samples: ResourceSample[]; after?: ResourceSample;
  outcome: string; reason: string; control_delta: number; delta: number;
};
export type ResourceReport = {
  version: string; kind: ResourceKind; resource_id: string; outcome: "healthy" | "degraded" | "inconclusive";
  reason: string; calls: number; max_calls: number; groups: ResourceGroup[];
};
export type ResourceCheck = {
  id: string; kind: ResourceKind; resource_id: string; model: string;
  state: "pending" | "running" | "done" | "failed" | "cancelled";
  created_at: string; finished_at?: string; report?: ResourceReport;
};
const sampleShape = hasShape({
  sample: isString, outcome: isString, failure: isOptional(isString), thinking: isBoolean, completed: isBoolean, usage_reported: isBoolean,
  input_tokens: isNumber, path_key: isOptional(isString), path_family: isOptional(isNumber), path_verified: isBoolean,
  attempt: hasShape({ id: isString, account_id: isString, model: isString, path: hasShape({ node_id: isOptional(isString), status: isString }) }),
});
const groupShape = hasShape({ control_account: isString, control_node: isString, account_id: isString, node_id: isString,
  control: isArrayOf(sampleShape), samples: isArrayOf(sampleShape), after: isOptional(sampleShape), outcome: isString, reason: isString, control_delta: isNumber, delta: isNumber });
export const resourceChecksDecoder = createValidatedDecoder<{ items: ResourceCheck[] }>("resource checks", hasShape({ items: isArrayOf(hasShape({
  id: isString, kind: isOneOf("account", "node"), resource_id: isString, model: isString,
  state: isOneOf("pending", "running", "done", "failed", "cancelled"), created_at: isString, finished_at: isOptional(isString),
  report: isOptional(hasShape({ version: isString, kind: isOneOf("account", "node"), resource_id: isString, outcome: isOneOf("healthy", "degraded", "inconclusive"), reason: isString, calls: isNumber, max_calls: isNumber, groups: isArrayOf(groupShape) })),
})) }));
export type ResourceSubmission = { resource_id: string; id?: string; error?: string };
export function fetchResourceChecks(kind: ResourceKind, ids: string[], signal?: AbortSignal): Promise<ResourceCheck[]> {
  const query = new URLSearchParams({ kind, resource_ids: ids.join(",") });
  return apiRequest(`/api/admin/v1/quality/resource-checks?${query}`, { signal }, resourceChecksDecoder).then(value => value.items);
}
export function startResourceChecks(kind: ResourceKind, ids: string[], model: string, signal?: AbortSignal): Promise<ResourceSubmission[]> {
  return apiRequest("/api/admin/v1/quality/resource-checks", { method: "POST", body: { kind, resource_ids: ids, model }, signal },
    createValidatedDecoder<{ items: ResourceSubmission[] }>("resource check submission", hasShape({ items: isArrayOf(hasShape({ resource_id: isString, id: isOptional(isString), error: isOptional(isString) })) }))).then(value => value.items);
}
