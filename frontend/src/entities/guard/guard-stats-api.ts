import { apiRequest } from "@/shared/api/client";
import { createValidatedDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOptional, isString } from "@/shared/api/decoder";

// 守卫特征统计:进程内累计,重启归零。与后端 GuardStatsSnapshot 对应。
type GuardSignalStat = {
  signal: string;
  triggered: number;
  requests: number;
  rescued: number;
  failed: number;
  lastSeen?: string;
};

// 守卫豁免统计:按原因记录守卫未介入的请求(协议无证据通道/模型不
// 支持推理等)。"为什么这批降智请求没被拦"的第一反应应该是看这里。
type GuardExemptStat = {
  reason: string;
  count: number;
  lastSeen?: string;
};

// 守卫当前生效配置投影:热更即时反映。面板据此回答"守卫现在是否在场"。
type GuardEffective = {
  enabled: boolean;
  maxAttempts: number;
  onExhausted: string;
  guardedModels?: string[];
  updatedAt: string;
};

type GuardStats = {
  signals: GuardSignalStat[];
  exempts?: GuardExemptStat[];
  retrial: {
    exhaustedRejected: number;
  };
  since?: string;
  effective?: GuardEffective;
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
    exhaustedRejected: isNumber,
  }),
  since: isOptional(isString),
  effective: isOptional(hasShape({
    enabled: isBoolean,
    maxAttempts: isNumber,
    onExhausted: isString,
    guardedModels: isOptional(isArrayOf(isString)),
    updatedAt: isString,
  })),
});

const decoder = createValidatedDecoder<GuardStats>("guard stats", statsValidator);

export function getGuardStats(): Promise<GuardStats> {
  return apiRequest("/api/admin/v1/guard-stats", {}, decoder);
}