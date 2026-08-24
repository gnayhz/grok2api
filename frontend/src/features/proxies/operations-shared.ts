import { createContext, useContext } from "react";
import { toast } from "sonner";

import {
	type EgressNodeDTO,
	type EgressOperationsConfigDTO,
	type EgressRoutingScope,
	type EgressTrafficClass,
} from "@/features/settings/settings-api";

/**
 * 出口运营配置的非组件共享面：路由枚举、i18n 键表、表单构造与纯谓词。
 * 从 operations-context.tsx 拆出——组件文件混出非组件会让
 * react-refresh/only-export-components 全线告警（HMR 边界失效），
 * 且这些常量/谓词被 routing/nodes/probe 面板直接消费，与 Provider
 * 生命周期无关。
 */

export type EgressOperationsDraft = Omit<EgressOperationsConfigDTO, "updatedAt">;

export const routingScopes: EgressRoutingScope[] = ["grok_build", "grok_web", "grok_console"];
export const trafficClasses: EgressTrafficClass[] = ["inference", "credential", "billing", "model_sync", "video"];

export const routingScopeLabelKeys: Record<EgressRoutingScope, string> = {
	grok_build: "proxies.routing.scopeBuild",
	grok_web: "proxies.routing.scopeWeb",
	grok_console: "proxies.routing.scopeConsole",
};

export const trafficClassLabelKeys: Record<EgressTrafficClass, string> = {
	inference: "proxies.routing.classInference",
	credential: "proxies.routing.classCredential",
	billing: "proxies.routing.classBilling",
	model_sync: "proxies.routing.classModelSync",
	video: "proxies.routing.classVideo",
};

const defaultOperationsForm: EgressOperationsDraft = {
	probeProvider: "cloudflare", probeIntervalSeconds: 900,
	// 总出口默认直连:新装系统最保守的出口是本机网络,由管理员显式改道。
	defaultTarget: { mode: "direct" }, scopeTargets: {}, classTargets: {},
};

export function operationsFormFrom(value?: EgressOperationsConfigDTO): EgressOperationsDraft {
	if (!value) return structuredClone(defaultOperationsForm);
	return {
		probeProvider: value.probeProvider,
		probeIntervalSeconds: value.probeIntervalSeconds,
		defaultTarget: value.defaultTarget ?? { mode: "auto" },
		scopeTargets: { ...value.scopeTargets },
		classTargets: { ...value.classTargets },
	};
}

// 节点是纯资源: 可作固定目标的条件是启用 + 已配置代理。旋转出口(代理池
// 模式)可选——固定的是隧道而非瞬时出口 IP;账号绑定代理({account} 模板)
// 仍排除——它按账号渲染不同子会话,应进池用 affinity 策略。冷却是瞬态
// 运行态(与后端 CanNodeServeFixedTarget 口径一致):冷却中的节点仍持有并
// 承接固定路由, 过滤掉会把活路由误显示为"目标已不可用"。
export function fixedTargetCandidates(nodes: EgressNodeDTO[]): EgressNodeDTO[] {
	return nodes.filter((node) => node.enabled && node.proxyConfigured && !node.accountBoundProxy);
}

export function nodeCooling(node: EgressNodeDTO): boolean {
	return node.cooldownUntil !== undefined && Date.parse(node.cooldownUntil) > Date.now();
}

export function showError(error: unknown) {
	toast.error(error instanceof Error ? error.message : "Operation failed");
}

/**
 * 统一路由草稿的 Context/hook 面（Provider 组件本体在
 * operations-context.tsx——组件文件只留组件，保住 react-refresh 边界，
 * 与 shared/auth/auth-state.ts 的切分约定一致）。
 */
export type EgressOperationsValue = {
	form: EgressOperationsDraft;
	isPending: boolean;
	isError: boolean;
	errorMessage?: string;
	isDirty: boolean;
	update: (updater: (current: EgressOperationsDraft) => EgressOperationsDraft) => void;
	save: () => Promise<boolean>;
	savePending: boolean;
	discard: () => void;
	retry: () => void;
};

export const EgressOperationsContext = createContext<EgressOperationsValue | null>(null);

export function useEgressOperations(): EgressOperationsValue {
	const value = useContext(EgressOperationsContext);
	if (!value) throw new Error("useEgressOperations must be used inside EgressOperationsProvider");
	return value;
}
