import { useQuery } from "@tanstack/react-query";
import {
	Activity,
	ChevronRight,
	CornerDownRight,
	Globe2,
	Workflow,
	Zap,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useOperationsNodes, useOperationsPools } from "@/features/operations/operations-queries";
import {
	getEgressRoutingStats,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressRoutingScope,
	type EgressRoutingTarget,
	type EgressTrafficClass,
} from "@/features/settings/settings-api";

import { ProxyRouteSimulator } from "./proxy-route-simulator";
import {
	fixedTargetCandidates,
	routingScopes,
	routingScopeLabelKeys,
	trafficClasses,
	trafficClassLabelKeys,
	useEgressOperations,
} from "./operations-shared";

export function ProxyRoutingView({
	onViewPools,
	onViewNodes,
}: {
	onViewPools?: () => void;
	onViewNodes?: () => void;
}) {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const nodesQuery = useOperationsNodes();
	const poolsQuery = useOperationsPools();

	const statsQuery = useQuery({
		queryKey: ["egress-routing-stats"],
		queryFn: () => getEgressRoutingStats(),
		refetchInterval: 10_000,
	});

	const nodes = useMemo(() => nodesQuery.data?.items ?? [], [nodesQuery.data?.items]);
	const pools = useMemo(() => poolsQuery.data ?? [], [poolsQuery.data]);

	// Eligible fixed target candidate nodes
	const eligibleNodes = useMemo(() => fixedTargetCandidates(nodes), [nodes]);

	// Maps for quick lookup
	const nodeMap = useMemo(() => new Map(nodes.map((n) => [n.id, n])), [nodes]);
	const poolMap = useMemo(() => new Map(pools.map((p) => [p.id, p])), [pools]);

	// Stats map: level -> { hit, fallback, lastSeen }
	const statsMap = useMemo(() => {
		const map = new Map<string, { hit: number; fallback: number; lastSeen?: string }>();
		for (const stat of statsQuery.data?.items ?? []) {
			map.set(stat.level, stat);
		}
		return map;
	}, [statsQuery.data]);



	return (
		<div className="flex flex-col gap-6">
			{/* Top Explanation Banner */}
			<div className="flex flex-wrap items-center justify-between gap-4 rounded-xl border border-border/80 bg-card/60 p-4 shadow-sm backdrop-blur-md">
				<div className="flex items-center gap-3">
					<div className="flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary">
						<Workflow className="size-5 text-primary" />
					</div>
					<div>
						<h2 className="text-base font-bold text-foreground">
							{t("network.viewRouting")}
						</h2>
						<p className="text-xs text-muted-foreground mt-0.5">
							{t("network.threeTierEngineDesc")}
						</p>
					</div>
				</div>

				<div className="flex items-center gap-2">
					<Badge variant="outline" className="text-xs font-medium">
						{t("network.poolsAvailableCount", { count: pools.length })}
					</Badge>
					<Badge variant="outline" className="text-xs font-medium">
						{t("network.pinnedExitsCount", { count: eligibleNodes.length })}
					</Badge>
				</div>
			</div>

			{/* Main Grid: 3-Tier Layered Editor (7 cols) + Route Simulator & Telemetry (5 cols) */}
			<div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
				{/* 3-Tier Layered Matrix Editor */}
				<div className="flex flex-col gap-5 lg:col-span-7">
					{/* Tier 1: Global Default Gateway */}
					<div className="rounded-xl border border-border/80 bg-card/75 p-5 shadow-sm">
						<div className="flex items-center justify-between border-b border-border/60 pb-3">
							<div className="flex items-center gap-2.5">
								<span className="flex size-6 items-center justify-center rounded-full bg-primary/10 text-xs font-bold text-primary">
									1
								</span>
								<div>
									<h3 className="text-sm font-bold text-foreground">
										{t("networkRouting.default")}
									</h3>
									<p className="text-[11px] text-muted-foreground">
										{t("networkRouting.defaultDescription")}
									</p>
								</div>
							</div>
							<Badge variant="secondary" className="text-[10px] font-semibold">
								{t("network.tier1Baseline")}
							</Badge>
						</div>

						<div className="mt-4 flex flex-wrap items-center justify-between gap-3">
							<div className="flex items-center gap-2">
								<RouteTargetPill target={operations.form.defaultTarget} poolMap={poolMap} nodeMap={nodeMap} />
							</div>

							<RouteTargetSelector
								target={operations.form.defaultTarget}
								isDefault
								pools={pools}
								eligibleNodes={eligibleNodes}
								onChange={(newTarget) => {
									operations.update((cur) => ({
										...cur,
										defaultTarget: newTarget || { mode: "auto" },
									}));
								}}
							/>
						</div>
					</div>

					{/* Tier 2: Scope Channels Overrides */}
					<div className="rounded-xl border border-border/80 bg-card/75 p-5 shadow-sm">
						<div className="flex items-center justify-between border-b border-border/60 pb-3">
							<div className="flex items-center gap-2.5">
								<span className="flex size-6 items-center justify-center rounded-full bg-blue-500/10 text-xs font-bold text-blue-500">
									2
								</span>
								<div>
									<h3 className="text-sm font-bold text-foreground">
										{t("networkRouting.provider")}
									</h3>
									<p className="text-[11px] text-muted-foreground">
										{t("network.channelRoutesDesc")}
									</p>
								</div>
							</div>
							<Badge variant="secondary" className="text-[10px] font-semibold">
								{t("network.tier2Scope")}
							</Badge>
						</div>

						<div className="mt-3 divide-y divide-border/60">
							{routingScopes.map((scope) => {
								const currentTarget = operations.form.scopeTargets[scope];
								const stat = statsMap.get(`scope:${scope}`);

								return (
									<div key={scope} className="flex flex-wrap items-center justify-between gap-3 py-3">
										<div className="min-w-0">
											<div className="flex items-center gap-2">
												<Globe2 className="size-4 text-blue-500 shrink-0" />
												<span className="text-xs font-bold text-foreground">
													{t(routingScopeLabelKeys[scope])}
												</span>
											</div>
											<div className="mt-1 flex items-center gap-2">
												<RouteTargetPill
													target={currentTarget}
													poolMap={poolMap}
													nodeMap={nodeMap}
													inheritTarget={operations.form.defaultTarget}
												/>
												{stat && (
													<span className="text-[10px] text-muted-foreground tabular-nums">
														· {t("network.hitsCountSuffix", { count: stat.hit.toLocaleString() })}
													</span>
												)}
											</div>
										</div>

										<RouteTargetSelector
											target={currentTarget}
											inheritLabel={t("networkRouting.inheritDefault")}
											pools={pools}
											eligibleNodes={eligibleNodes}
											onChange={(newTarget) => {
												operations.update((cur) => {
													const next = { ...cur.scopeTargets };
													if (!newTarget) {
														delete next[scope];
													} else {
														next[scope] = newTarget;
													}
													return { ...cur, scopeTargets: next };
												});
											}}
										/>
									</div>
								);
							})}
						</div>
					</div>

					{/* Tier 3: Traffic Class Fine-Grained Rules */}
					<div className="rounded-xl border border-border/80 bg-card/75 p-5 shadow-sm">
						<div className="flex items-center justify-between border-b border-border/60 pb-3">
							<div className="flex items-center gap-2.5">
								<span className="flex size-6 items-center justify-center rounded-full bg-purple-500/10 text-xs font-bold text-purple-500">
									3
								</span>
								<div>
									<h3 className="text-sm font-bold text-foreground">
										{t("networkRouting.semanticGroup")}
									</h3>
									<p className="text-[11px] text-muted-foreground">
										{t("network.semanticRoutesDesc")}
									</p>
								</div>
							</div>
							<Badge variant="secondary" className="text-[10px] font-semibold">
								{t("network.tier3Priority")}
							</Badge>
						</div>

						<div className="mt-3 divide-y divide-border/60">
							{trafficClasses.map((cls) => {
								const currentTarget = operations.form.classTargets[cls];
								const stat = statsMap.get(`class:${cls}`);

								return (
									<div key={cls} className="flex flex-wrap items-center justify-between gap-3 py-3">
										<div className="min-w-0">
											<div className="flex items-center gap-2">
												<Zap className="size-4 text-purple-500 shrink-0" />
												<span className="text-xs font-bold text-foreground">
													{t(trafficClassLabelKeys[cls])}
												</span>
											</div>
											<div className="mt-1 flex items-center gap-2">
												<RouteTargetPill
													target={currentTarget}
													poolMap={poolMap}
													nodeMap={nodeMap}
													inheritTarget={operations.form.defaultTarget}
												/>
												{stat && (
													<span className="text-[10px] text-muted-foreground tabular-nums">
														· {t("network.hitsCountSuffix", { count: stat.hit.toLocaleString() })}
													</span>
												)}
											</div>
										</div>

										<RouteTargetSelector
											target={currentTarget}
											inheritLabel={t("networkRouting.inheritScope")}
											pools={pools}
											eligibleNodes={eligibleNodes}
											onChange={(newTarget) => {
												operations.update((cur) => {
													const next = { ...cur.classTargets };
													if (!newTarget) {
														delete next[cls];
													} else {
														next[cls] = newTarget;
													}
													return { ...cur, classTargets: next };
												});
											}}
										/>
									</div>
								);
							})}
						</div>
					</div>
				</div>

				{/* Right Column: Interactive Route Simulator & Live Hit Telemetry (5 cols) */}
				<div className="flex flex-col gap-6 lg:col-span-5">
					{/* Interactive Route Simulator (Unified) */}
					<ProxyRouteSimulator
						nodes={nodes}
						pools={pools}
						onViewPools={onViewPools}
						onViewNodes={onViewNodes}
					/>

					{/* Live Traffic Hits & Fallbacks Summary */}
					<div className="rounded-xl border border-border/80 bg-card/75 p-5 shadow-sm">
						<div className="flex items-center justify-between border-b border-border/60 pb-3">
							<div className="flex items-center gap-2">
								<Activity className="size-4 text-primary" />
								<h3 className="text-sm font-bold text-foreground">
									{t("network.hitMatrix")}
								</h3>
							</div>
							<span className="text-[10px] text-muted-foreground">{t("network.telemetry")}</span>
						</div>

						<div className="mt-3 divide-y divide-border/60">
							{statsQuery.data?.items.map((stat, i) => (
								<div key={i} className="flex items-center justify-between py-2.5 text-xs">
									<div>
										<span className="font-semibold text-foreground block">
											{stat.level.startsWith("scope:")
												? t(routingScopeLabelKeys[stat.level.replace("scope:", "") as EgressRoutingScope] || stat.level)
												: stat.level.startsWith("class:")
													? t(trafficClassLabelKeys[stat.level.replace("class:", "") as EgressTrafficClass] || stat.level)
													: stat.level === "default"
														? t("networkRouting.default")
														: stat.level}
										</span>
										<span className="text-[10px] text-muted-foreground font-medium">
											{t("network.targetModeLabel")}: {stat.mode === "auto"
												? t("network.modeAuto")
												: stat.mode === "direct"
													? t("network.modeDirect")
													: stat.mode === "pool"
														? t("network.modePool")
														: t("network.modeNode")}
										</span>
									</div>
									<div className="text-right">
										<span className="font-bold tabular-nums text-foreground block">
											{t("network.hitsCountSuffix", { count: stat.hit.toLocaleString() })}
										</span>
										{stat.fallback > 0 && (
											<span className="text-[10px] text-amber-500 font-semibold block">
												{t("network.fallbacksCountSuffix", { count: stat.fallback })}
											</span>
										)}
									</div>
								</div>
							))}
						</div>
					</div>
				</div>
			</div>
		</div>
	);
}

/**
 * Route Target Pill Display
 */
function RouteTargetPill({
	target,
	poolMap,
	nodeMap,
	inheritTarget,
}: {
	target?: EgressRoutingTarget;
	poolMap: Map<string, EgressPoolDTO>;
	nodeMap: Map<string, EgressNodeDTO>;
	inheritTarget?: EgressRoutingTarget;
}) {
	const { t } = useTranslation();

	if (!target || !target.mode) {
		const inheritedModeLabel = inheritTarget?.mode
			? inheritTarget.mode === "auto"
				? t("network.modeAuto")
				: inheritTarget.mode === "direct"
					? t("network.modeDirect")
					: inheritTarget.mode === "pool"
						? t("network.modePool")
						: t("network.modeNode")
			: "";

		return (
			<span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
				<CornerDownRight className="size-3" />
				<span>{t("network.inherit")}</span>
				{inheritedModeLabel && (
					<span className="text-[10px] text-muted-foreground/80">
						({inheritedModeLabel})
					</span>
				)}
			</span>
		);
	}

	if (target.mode === "auto") {
		return (
			<Badge variant="outline" className="text-[11px] font-semibold text-emerald-600 dark:text-emerald-400 border-emerald-500/30 bg-emerald-500/5">
				{t("network.modeAuto")}
			</Badge>
		);
	}

	if (target.mode === "direct") {
		return (
			<Badge variant="outline" className="text-[11px] font-semibold text-blue-600 dark:text-blue-400 border-blue-500/30 bg-blue-500/5">
				{t("network.modeDirect")}
			</Badge>
		);
	}

	if (target.mode === "pool" && target.poolId) {
		const pool = poolMap.get(target.poolId);
		return (
			<Badge variant="outline" className="text-[11px] font-semibold text-purple-600 dark:text-purple-400 border-purple-500/30 bg-purple-500/5">
				{t("network.poolSelectedText", { name: pool?.name || target.poolId })}
			</Badge>
		);
	}

	if (target.mode === "node" && target.nodeId) {
		const node = nodeMap.get(target.nodeId);
		return (
			<Badge variant="outline" className="text-[11px] font-semibold text-amber-600 dark:text-amber-400 border-amber-500/30 bg-amber-500/5">
				{t("network.nodeSelectedText", { name: node?.name || target.nodeId, latency: node?.probeLatencyMs ?? 0 })}
			</Badge>
		);
	}

	return <Badge variant="destructive">{t("network.invalidTarget")}</Badge>;
}

/**
 * Route Target Selector (Popover)
 */
function RouteTargetSelector({
	target,
	isDefault = false,
	inheritLabel,
	pools,
	eligibleNodes,
	onChange,
}: {
	target?: EgressRoutingTarget;
	isDefault?: boolean;
	inheritLabel?: string;
	pools: EgressPoolDTO[];
	eligibleNodes: EgressNodeDTO[];
	onChange: (target?: EgressRoutingTarget) => void;
}) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);

	return (
		<Popover open={open} onOpenChange={setOpen}>
			<PopoverTrigger asChild>
				<Button size="sm" variant="outline" className="h-7 text-xs font-medium gap-1">
					<span>{t("network.configureTarget")}</span>
					<ChevronRight className="size-3 text-muted-foreground" />
				</Button>
			</PopoverTrigger>
			<PopoverContent align="end" className="w-80 p-3.5">
				<div className="space-y-3.5">
					<div>
						<Label className="text-xs font-semibold">{t("network.targetModeLabel")}</Label>
						<Select
							value={target?.mode || (isDefault ? "auto" : "inherit")}
							onValueChange={(newMode) => {
								if (newMode === "inherit") {
									onChange(undefined);
									setOpen(false);
								} else if (newMode === "auto") {
									onChange({ mode: "auto" });
									setOpen(false);
								} else if (newMode === "direct") {
									onChange({ mode: "direct" });
									setOpen(false);
								} else if (newMode === "pool") {
									onChange({ mode: "pool", poolId: pools[0]?.id });
								} else if (newMode === "node") {
									onChange({ mode: "node", nodeId: eligibleNodes[0]?.id });
								}
							}}
						>
							<SelectTrigger className="w-full mt-1.5 h-8 text-xs">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								{!isDefault && (
									<SelectItem value="inherit">
										{inheritLabel || t("network.inherit")}
									</SelectItem>
								)}
								<SelectItem value="auto">{t("network.modeAuto")}</SelectItem>
								<SelectItem value="direct">{t("network.modeDirect")}</SelectItem>
								<SelectItem value="pool">{t("network.modePool")}</SelectItem>
								<SelectItem value="node">{t("network.modeNode")}</SelectItem>
							</SelectContent>
						</Select>
					</div>

					{/* If Pool Mode Selected */}
					{target?.mode === "pool" && (
						<div>
							<Label className="text-xs font-semibold">{t("network.selectProxyPoolLabel")}</Label>
							<Select
								value={target.poolId || ""}
								onValueChange={(poolId) => {
									onChange({ mode: "pool", poolId });
									setOpen(false);
								}}
							>
								<SelectTrigger className="w-full mt-1.5 h-8 text-xs">
									<SelectValue placeholder={t("network.choosePoolPlaceholder")} />
								</SelectTrigger>
								<SelectContent>
									{pools.map((p) => (
										<SelectItem key={p.id} value={p.id}>
											{p.name} ({t("network.membersLabel", { count: p.memberIds.length })})
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</div>
					)}

					{/* If Node Mode Selected */}
					{target?.mode === "node" && (
						<div>
							<Label className="text-xs font-semibold">{t("network.selectFixedNodeLabel")}</Label>
							<Select
								value={target.nodeId || ""}
								onValueChange={(nodeId) => {
									onChange({ mode: "node", nodeId });
									setOpen(false);
								}}
							>
								<SelectTrigger className="w-full mt-1.5 h-8 text-xs">
									<SelectValue placeholder={t("network.chooseNodePlaceholder")} />
								</SelectTrigger>
								<SelectContent>
									{eligibleNodes.map((n) => (
										<SelectItem key={n.id} value={n.id}>
											{n.name} ({n.probeLatencyMs}ms)
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</div>
					)}
				</div>
			</PopoverContent>
		</Popover>
	);
}