import type { EgressRoutingTarget } from "@/features/settings/settings-api";

export type EffectiveRoutingConfig = {
  defaultTarget: EgressRoutingTarget;
  scopeTargets: Record<string, EgressRoutingTarget>;
  classTargets: Record<string, EgressRoutingTarget>;
};

/**
 * 生效解析：与后端 OperationsConfig.TargetFor（domain/egress resolveTarget）
 * 同规则——类别 → 作用域 → 总出口 → 自动调度。Configured() 是 mode != ""：
 * 显式 auto 是该层的最终选择（自动调度），会挡住下一层；缺省/空 mode 才回落。
 * 前端曾把 auto 当未配置回落，预览与真实出口相反（默认 direct + 作用域 auto
 * 时，后端走自动调度，UI 却显示 direct）。
 */
export function resolveEffectiveTarget(config: EffectiveRoutingConfig, cls?: string, scope?: string): EgressRoutingTarget {
  if (cls) {
    const classTarget = config.classTargets[cls];
    if (classTarget?.mode) return classTarget.mode === "auto" ? { mode: "auto" } : classTarget;
  }
  if (scope) {
    const scopeTarget = config.scopeTargets[scope];
    if (scopeTarget?.mode) return scopeTarget.mode === "auto" ? { mode: "auto" } : scopeTarget;
  }
  if (config.defaultTarget.mode) {
    return config.defaultTarget.mode === "auto" ? { mode: "auto" } : config.defaultTarget;
  }
  return { mode: "auto" };
}
