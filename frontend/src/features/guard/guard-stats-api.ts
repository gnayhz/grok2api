import { apiRequest } from "@/shared/api/client";
import { createValidatedDecoder, hasShape, isArrayOf, isNumber, isObject, isOptional, isString } from "@/shared/api/decoder";

// 守卫特征统计:进程内累计,重启归零。与后端 GuardStatsSnapshot 对应。
export type GuardSignalStat = {
  signal: string;
  triggered: number;
  requests: number;
  rescued: number;
  failed: number;
  lastSeen?: string;
};

// 守卫豁免统计:按原因记录守卫未介入的请求(协议无证据通道/模型不
// 支持推理等)。"为什么这批降智请求没被拦"的第一反应应该是看这里。
export type GuardExemptStat = {
  reason: string;
  count: number;
  lastSeen?: string;
};

export type GuardStats = {
  signals: GuardSignalStat[];
  exempts?: GuardExemptStat[];
  retrial: {
    sameAccountRetryUsed: number;
    sameAccountRetryRescued: number;
    exhaustedDeliverLast: number;
    exhaustedRejected: number;
  };
  canary: Record<string, number>;
  since?: string;
};

const statsValidator = hasShape({
  signals: isArrayOf(hasShape({
    signal: isString,
    triggered: isNumber,
    requests: isNumber,
    rescued: isNumber,
    failed: isNumber,
    lastSeen: isOptional(isString),
  })),
  exempts: isOptional(isArrayOf(hasShape({
    reason: isString,
    count: isNumber,
    lastSeen: isOptional(isString),
  }))),
  retrial: hasShape({
    sameAccountRetryUsed: isNumber,
    sameAccountRetryRescued: isNumber,
    exhaustedDeliverLast: isNumber,
    exhaustedRejected: isNumber,
  }),
  canary: isObject,
  since: isOptional(isString),
});

const decoder = createValidatedDecoder<GuardStats>("guard stats", statsValidator);

export function getGuardStats(): Promise<GuardStats> {
  return apiRequest("/api/admin/v1/guard-stats", {}, decoder);
}
