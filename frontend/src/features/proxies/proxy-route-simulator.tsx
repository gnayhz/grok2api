import {
	Cpu,
	Globe2,
	Layers,
	Server,
} from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/shared/ui/badge";
import { useNow } from "@/shared/lib/use-now";
import { nodeCondition } from "@/entities/egress/node-condition";
import {
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressRoutingScope,
	type EgressTrafficClass,
} from "@/entities/egress/egress-api";
import { cn } from "@/shared/lib/cn";

import { resolveEffectiveTarget } from "./effective-target";
import {
	routingScopes,
	routingScopeLabelKeys,
	trafficClasses,
	trafficClassLabelKeys,
	useEgressOperations,
} from "./operations-shared";
import { maskIP } from "@/shared/lib/mask-ip";

export function ProxyRouteSimulator({
	nodes,
	pools,
	onViewPools,
	onViewNodes,
}: {
	nodes: EgressNodeDTO[];
	pools: EgressPoolDTO[];
	onViewPools?: () => void;
	onViewNodes?: () => void;
}) {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const now = useNow(10_000);

	const [simScope, setSimScope] = useState<EgressRoutingScope>("grok_build");
	const [simClass, setSimClass] = useState<EgressTrafficClass>("inference");

	// Active candidate nodes
	const isCandidate = useCallback(
		(node: EgressNodeDTO) => ["ready", "dynamic"].includes(nodeCondition(node, now)),
		[now]
	);

	const candidateIds = useMemo(
		() => new Set(nodes.filter(isCandidate).map((n) => n.id)),
		[nodes, isCandidate]
	);

	const poolCapacity = useCallback(
		(pool: EgressPoolDTO) => pool.memberIds.filter((id) => candidateIds.has(id)).length,
		[candidateIds]
	);

	const nodeMap = useMemo(() => new Map(nodes.map((n) => [n.id, n])), [nodes]);
	const poolMap = useMemo(() => new Map(pools.map((p) => [p.id, p])), [pools]);

	// Resolve target
	const effectiveTarget = useMemo(
		() =>
			resolveEffectiveTarget(
				{
					defaultTarget: operations.form.defaultTarget,
					scopeTargets: operations.form.scopeTargets,
					classTargets: operations.form.classTargets,
				},
				simClass,
				simScope
			),
		[operations.form, simClass, simScope]
	);

	// Determine rule origin
	const ruleOrigin = useMemo(() => {
		if (operations.form.classTargets[simClass]?.mode) {
			return t("networkRouting.fromClass");
		}
		if (operations.form.scopeTargets[simScope]?.mode) {
			return t("networkRouting.fromScope");
		}
		if (operations.form.defaultTarget?.mode) {
			return t("networkRouting.fromDefault");
		}
		return t("networkRouting.fromAuto");
	}, [operations.form, simClass, simScope, t]);

	// Resolved target presentation
	const targetPresentation = useMemo(() => {
		if (effectiveTarget.mode === "direct") {
			return {
				name: t("network.modeDirect"),
				detail: t("network.directDetail"),
				healthy: true,
				type: "direct" as const,
			};
		}
		if (effectiveTarget.mode === "node" && effectiveTarget.nodeId) {
			const node = nodeMap.get(effectiveTarget.nodeId);
			if (node) {
				return {
					name: `${t("network.modeNode")}: ${node.name}`,
					detail: node.exitIp
						? `${t("network.exitAddressLabel")}: ${maskIP(node.exitIp)} · ${node.probeLatencyMs}ms`
						: t("network.modeNode"),
					healthy: node.enabled && node.probeStatus !== "unhealthy",
					type: "node" as const,
					node,
				};
			}
			return { name: t("network.missing"), detail: "", healthy: false, type: "missing" as const };
		}
		if (effectiveTarget.mode === "pool" && effectiveTarget.poolId) {
			const pool = poolMap.get(effectiveTarget.poolId);
			if (pool) {
				const ready = poolCapacity(pool);
				return {
					name: `${t("network.modePool")}: ${pool.name}`,
					detail: t("network.availableMembersRatio", {
						ready,
						total: pool.memberIds.length,
						pct: Math.round((ready / (pool.memberIds.length || 1)) * 100),
					}),
					healthy: pool.enabled && ready > 0,
					type: "pool" as const,
					pool,
				};
			}
			return { name: t("network.missing"), detail: "", healthy: false, type: "missing" as const };
		}
		// Default auto
		const readyCount = nodes.filter((n) => n.enabled && isCandidate(n)).length;
		return {
			name: t("network.modeAuto"),
			detail: t("network.autoDetail") + ` (${t("network.available", { count: readyCount })})`,
			healthy: readyCount > 0,
			type: "auto" as const,
		};
	}, [effectiveTarget, nodeMap, poolMap, nodes, isCandidate, poolCapacity, t]);

	return (
		<div className="flex flex-col justify-between rounded-xl border border-border/80 bg-card/75 p-5 shadow-sm">
			<div>
				{/* Header */}
				<div className="flex items-center justify-between border-b border-border/60 pb-3">
					<div className="flex items-center gap-2">
						<Cpu className="size-4 text-primary" />
						<h3 className="text-sm font-bold text-foreground">
							{t("network.simulatorTitle")}
						</h3>
					</div>
					<Badge variant="outline" className="text-[10px] font-semibold text-primary border-primary/30 bg-primary/5">
						实时计算
					</Badge>
				</div>
				<p className="mt-2 text-xs text-muted-foreground leading-relaxed">
					{t("network.simulatorHelp")}
				</p>

				{/* Selectors */}
				<div className="mt-4 space-y-3.5">
					{/* Channel Selection */}
					<div>
						<span className="text-[11px] font-bold text-foreground block uppercase tracking-wider">
							{t("network.simulateScope")}
						</span>
						<div className="mt-1.5 grid grid-cols-3 gap-1.5">
							{routingScopes.map((scope) => (
								<button
									type="button"
									key={scope}
									className={cn(
										"flex items-center justify-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs font-medium transition-all",
										simScope === scope
											? "border-primary bg-primary text-primary-foreground shadow-sm font-semibold"
											: "border-border/80 bg-background text-foreground hover:bg-accent"
									)}
									onClick={() => setSimScope(scope)}
								>
									<Globe2 className="size-3" />
									<span className="truncate">{t(routingScopeLabelKeys[scope])}</span>
								</button>
							))}
						</div>
					</div>

					{/* Workload Selection */}
					<div>
						<span className="text-[11px] font-bold text-foreground block uppercase tracking-wider">
							{t("network.simulateClass")}
						</span>
						<div className="mt-1.5 grid grid-cols-3 gap-1.5">
							{trafficClasses.map((cls) => (
								<button
									type="button"
									key={cls}
									className={cn(
										"rounded-md border px-2 py-1.5 text-xs font-medium transition-all truncate text-center",
										simClass === cls
											? "border-primary bg-primary text-primary-foreground shadow-sm font-semibold"
											: "border-border/80 bg-background text-foreground hover:bg-accent"
									)}
									onClick={() => setSimClass(cls)}
								>
									{t(trafficClassLabelKeys[cls])}
								</button>
							))}
						</div>
					</div>
				</div>
			</div>

			{/* Visual Decision Tree & Output Box */}
			<div className="mt-5 rounded-lg border border-border/80 bg-muted/20 p-3.5">
				<div className="flex items-center justify-between">
					<span className="text-[10px] font-bold uppercase tracking-wider text-muted-foreground">
						{t("network.resolutionPath")}
					</span>
					<span className="text-[10px] font-semibold text-primary">
						{ruleOrigin}
					</span>
				</div>

				{/* Visual Linkage */}
				<div className="mt-3 space-y-2 text-xs">
					{/* Step 1: Workload */}
					<div className="flex items-center gap-2">
						<Badge variant="outline" className="text-[10px] font-semibold shrink-0">
							{t("network.flowWorkload")}
						</Badge>
						<span className="font-semibold text-foreground truncate">
							{t(trafficClassLabelKeys[simClass])}
						</span>
					</div>

					<div className="pl-4 text-muted-foreground/60 text-xs leading-none">↓</div>

					{/* Step 2: Channel */}
					<div className="flex items-center gap-2">
						<Badge variant="outline" className="text-[10px] font-semibold shrink-0">
							{t("network.flowChannel")}
						</Badge>
						<span className="font-semibold text-foreground truncate">
							{t(routingScopeLabelKeys[simScope])}
						</span>
					</div>

					<div className="pl-4 text-muted-foreground/60 text-xs leading-none">↓</div>

					{/* Step 3: Resolved Target Gateway */}
					<div className="rounded-lg border border-primary/30 bg-background/90 p-3 shadow-sm">
						<div className="flex items-center justify-between">
							<div className="flex items-center gap-2 min-w-0">
								<div
									className={cn(
										"size-2.5 rounded-full shrink-0",
										targetPresentation.healthy ? "bg-emerald-500 animate-pulse" : "bg-rose-500"
									)}
								/>
								<div className="min-w-0">
									<div className="flex items-center gap-1.5">
										<span className="text-xs font-bold text-foreground truncate">
											{targetPresentation.name}
										</span>
									</div>
									<p className="text-[11px] text-muted-foreground truncate mt-0.5">
										{targetPresentation.detail}
									</p>
								</div>
							</div>

							<Badge
								variant="outline"
								className={cn(
									"text-[10px] shrink-0 font-semibold",
									targetPresentation.healthy
										? "text-emerald-600 dark:text-emerald-400 border-emerald-500/30"
										: "text-rose-500 border-rose-500/30"
								)}
							>
								{targetPresentation.healthy ? t("network.healthyActive") : t("network.degradedFallback")}
							</Badge>
						</div>

						{/* Jump action link if pool or node */}
						{targetPresentation.type === "pool" && (
							<button
								type="button"
								onClick={onViewPools}
								className="mt-2 flex items-center gap-1 text-[11px] font-medium text-purple-600 dark:text-purple-400 hover:underline cursor-pointer"
							>
								<Layers className="size-3" />
								<span>查看所属代理池详情 ➔</span>
							</button>
						)}
						{targetPresentation.type === "node" && (
							<button
								type="button"
								onClick={onViewNodes}
								className="mt-2 flex items-center gap-1 text-[11px] font-medium text-emerald-600 dark:text-emerald-400 hover:underline cursor-pointer"
							>
								<Server className="size-3" />
								<span>查看目标出口节点详情 ➔</span>
							</button>
						)}
					</div>
				</div>
			</div>
		</div>
	);
}