import type { EgressRoutingTarget } from "@/features/settings/settings-api";

export type EffectiveRoutingConfig = {
  defaultTarget: EgressRoutingTarget;
  scopeTargets: Record<string, EgressRoutingTarget>;
  classTargets: Record<string, EgressRoutingTarget>;
};

/**
 * 生效解析：与后端 OperationsConfig.TargetFor（domain/egress resolveTarget）
 * 同规则的纯函数实现——类别 → 作用域 → 总出口 → 自动调度；存储值 "auto"
 * 等同未配置（该层回落下一层）。抽为纯函数供路由面板与奇偶性测试共用，
 * 防止前后端两份阶梯实现漂移。
 */
export function resolveEffectiveTarget(config: EffectiveRoutingConfig, cls?: string, scope?: string): EgressRoutingTarget {
  const classTarget = cls ? config.classTargets[cls] : undefined;
  if (classTarget && classTarget.mode !== "auto") return classTarget;
  const scopeTarget = scope ? config.scopeTargets[scope] : undefined;
  if (scopeTarget && scopeTarget.mode !== "auto") return scopeTarget;
  if (config.defaultTarget.mode !== "auto") return config.defaultTarget;
  return { mode: "auto" };
}
