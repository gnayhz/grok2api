import { useQuery } from "@tanstack/react-query";
import {
	Activity,
	AlertOctagon,
	AlertTriangle,
	CheckCircle2,
	ChevronRight,
	Clock,
	Layers2,
	Radio,
	Server,
	ShieldAlert,
} from "lucide-react";
import { useCallback, useMemo } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { useNow } from "@/features/guard/quality-hooks";
import { nodeCondition } from "@/features/operations/operations-data";
import { useOperationsNodes, useOperationsPools } from "@/features/operations/operations-queries";
import {
	getEgressRoutingStats,
	listEgressSources,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressRoutingScope,
	type EgressTrafficClass,
} from "@/features/settings/settings-api";
import { cn } from "@/shared/lib/cn";

import { getNetworkRuntime } from "./network-api";
import {
	routingScopeLabelKeys,
	trafficClassLabelKeys,
} from "./operations-shared";
import { formatTimeAgo } from "./proxy-format";
import { ProxyRouteSimulator } from "./proxy-route-simulator";

export function ProxyRadarView({
	onNavigateTab,
}: {
	onNavigateTab: (tab: "radar" | "nodes" | "pools" | "routing", search?: string) => void;
}) {
	const { t, i18n } = useTranslation();
	const nodesQuery = useOperationsNodes();
	const poolsQuery = useOperationsPools();
	const now = useNow(10_000);

	const sourcesQuery = useQuery({
		queryKey: ["egress-sources"],
		queryFn: ({ signal }) => listEgressSources(undefined, signal),
		staleTime: 15_000,
		refetchInterval: 30_000,
	});

	const statsQuery = useQuery({
		queryKey: ["egress-routing-stats"],
		queryFn: () => getEgressRoutingStats(),
		refetchInterval: 10_000,
	});

	const runtimeQuery = useQuery({
		queryKey: ["egress-runtime"],
		queryFn: ({ signal }) => getNetworkRuntime(signal),
		refetchInterval: 10_000,
	});

	const items = useMemo(() => nodesQuery.data?.items ?? [], [nodesQuery.data?.items]);
	const pools = useMemo(() => poolsQuery.data ?? [], [poolsQuery.data]);
	const runtime = runtimeQuery.data?.network;

	// Active candidate nodes
	const isCandidate = useCallback(
		(node: EgressNodeDTO) => ["ready", "dynamic"].includes(nodeCondition(node, now)),
		[now]
	);

	const candidateIds = useMemo(
		() => new Set(items.filter(isCandidate).map((n) => n.id)),
		[items, isCandidate]
	);

	const poolCapacity = useCallback(
		(pool: EgressPoolDTO) => pool.memberIds.filter((id) => candidateIds.has(id)).length,
		[candidateIds]
	);

	// Issue inspection counts
	const count = (cond: string) => items.filter((n) => nodeCondition(n, now) === cond).length;
	const held = count("held");
	const banned = count("banned");
	const unhealthy = count("unhealthy");
	const cooling = count("cooling");
	const unconfigured = count("unconfigured");
	const sourceErrors =
		sourcesQuery.data?.items.filter((s) => s.enabled && s.lastSyncError).length ?? 0;
	const emptyPools = pools.filter((p) => p.enabled && poolCapacity(p) === 0);

	return (
		<div className="flex flex-col gap-6">
			{/* 1. Issue Inspection Radar / All Nominal Banner */}
			{held + banned + unhealthy + cooling + unconfigured + sourceErrors + emptyPools.length > 0 ? (
				<div className="rounded-xl border border-amber-500/30 bg-amber-500/5 p-4 backdrop-blur-md">
					<div className="flex flex-wrap items-center justify-between gap-3">
						<div className="flex items-center gap-2.5">
							<div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-amber-500/10 text-amber-500">
								<AlertTriangle className="size-4" />
							</div>
							<div>
								<h3 className="text-sm font-bold text-foreground">
									{t("network.issueCenter")}
								</h3>
								<p className="text-xs text-muted-foreground mt-0.5">
									{t("network.queueCount", {
										count:
											(held + banned > 0 ? 1 : 0) +
											(unhealthy > 0 ? 1 : 0) +
											(cooling > 0 ? 1 : 0) +
											(unconfigured > 0 ? 1 : 0) +
											(sourceErrors > 0 ? 1 : 0) +
											(emptyPools.length > 0 ? 1 : 0),
									})}
								</p>
							</div>
						</div>
					</div>

					{/* Issue Action Chips */}
					<div className="mt-3.5 grid grid-cols-1 gap-2.5 sm:grid-cols-2 lg:grid-cols-3">
						{held + banned > 0 && (
							<div
								onClick={() => onNavigateTab("nodes", "condition=restricted")}
								className="group flex cursor-pointer items-center justify-between rounded-lg border border-border/80 bg-card/80 p-2.5 transition-all hover:border-amber-500/40 hover:bg-card"
							>
								<div className="flex items-center gap-2">
									<ShieldAlert className="size-4 text-amber-500" />
									<span className="text-xs font-medium text-foreground">
										{t("network.heldDetail", { held, banned })}
									</span>
								</div>
								<ChevronRight className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
							</div>
						)}

						{unhealthy > 0 && (
							<div
								onClick={() => onNavigateTab("nodes", "condition=unhealthy")}
								className="group flex cursor-pointer items-center justify-between rounded-lg border border-border/80 bg-card/80 p-2.5 transition-all hover:border-rose-500/40 hover:bg-card"
							>
								<div className="flex items-center gap-2">
									<Radio className="size-4 text-rose-500" />
									<span className="text-xs font-medium text-foreground">
										{t("network.unhealthyDetail", { count: unhealthy })}
									</span>
								</div>
								<ChevronRight className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
							</div>
						)}

						{cooling > 0 && (
							<div
								onClick={() => onNavigateTab("nodes", "condition=cooling")}
								className="group flex cursor-pointer items-center justify-between rounded-lg border border-border/80 bg-card/80 p-2.5 transition-all hover:border-blue-500/40 hover:bg-card"
							>
								<div className="flex items-center gap-2">
									<Clock className="size-4 text-blue-500" />
									<span className="text-xs font-medium text-foreground">
										{t("network.coolingDetail", { count: cooling })}
									</span>
								</div>
								<ChevronRight className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
							</div>
						)}

						{unconfigured > 0 && (
							<div
								onClick={() => onNavigateTab("nodes", "condition=unconfigured")}
								className="group flex cursor-pointer items-center justify-between rounded-lg border border-border/80 bg-card/80 p-2.5 transition-all hover:border-muted-foreground hover:bg-card"
							>
								<div className="flex items-center gap-2">
									<Server className="size-4 text-muted-foreground" />
									<span className="text-xs font-medium text-foreground">
										{t("network.unconfiguredDetail", { count: unconfigured })}
									</span>
								</div>
								<ChevronRight className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
							</div>
						)}

						{emptyPools.length > 0 && (
							<div
								onClick={() => onNavigateTab("pools")}
								className="group flex cursor-pointer items-center justify-between rounded-lg border border-border/80 bg-card/80 p-2.5 transition-all hover:border-amber-500/40 hover:bg-card"
							>
								<div className="flex items-center gap-2">
									<Layers2 className="size-4 text-amber-500" />
									<span className="text-xs font-medium text-foreground">
										{t("network.emptyPoolDetail", { count: emptyPools.length })}
									</span>
								</div>
								<ChevronRight className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
							</div>
						)}

						{sourceErrors > 0 && (
							<div
								onClick={() => onNavigateTab("nodes", "sources=open")}
								className="group flex cursor-pointer items-center justify-between rounded-lg border border-border/80 bg-card/80 p-2.5 transition-all hover:border-rose-500/40 hover:bg-card"
							>
								<div className="flex items-center gap-2">
									<AlertOctagon className="size-4 text-rose-500" />
									<span className="text-xs font-medium text-foreground">
										{t("network.sourceErrorDetail", { count: sourceErrors })}
									</span>
								</div>
								<ChevronRight className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
							</div>
						)}
					</div>
				</div>
			) : (
				<div className="flex items-center justify-between rounded-xl border border-emerald-500/20 bg-emerald-500/5 p-4 backdrop-blur-md">
					<div className="flex items-center gap-3">
						<div className="flex size-8 items-center justify-center rounded-lg bg-emerald-500/10 text-emerald-500">
							<CheckCircle2 className="size-4" />
						</div>
						<div>
							<h3 className="text-sm font-bold text-foreground">
								{t("network.allNominal")}
							</h3>
							<p className="text-xs text-muted-foreground mt-0.5">
								{t("network.allNominalSub")}
							</p>
						</div>
					</div>
					<Badge variant="outline" className="border-emerald-500/30 text-emerald-600 dark:text-emerald-400 font-semibold text-xs">
						{t("network.normal")}
					</Badge>
				</div>
			)}

			{/* 2. Main Grid: Left Hit Matrix & Runtime Telemetry (7 cols) + Right Unified Route Simulator (5 cols) */}
			<div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
				{/* Left Side: Hit Matrix & Runtime Capacity (7 cols) */}
				<div className="flex flex-col gap-6 lg:col-span-7">
					{/* Hit Matrix */}
					<div className="flex flex-col gap-3 rounded-xl border border-border/80 bg-card/70 p-5 shadow-sm">
						<div className="flex items-center justify-between border-b border-border/60 pb-3">
							<div className="flex items-center gap-2">
								<Activity className="size-4 text-primary" />
								<h3 className="text-sm font-bold text-foreground">
									{t("network.hitMatrix")}
								</h3>
							</div>
							<span className="text-[11px] text-muted-foreground font-medium">
								{t("network.rulesActiveCount", { count: statsQuery.data?.items.length ?? 0 })}
							</span>
						</div>

						{statsQuery.data && statsQuery.data.items.length > 0 ? (
							<div className="mt-2 divide-y divide-border/60 rounded-lg border border-border/70 overflow-hidden">
								{statsQuery.data.items.map((stat, idx) => (
									<div
										key={idx}
										className="flex items-center justify-between p-3 transition-colors hover:bg-muted/30"
									>
										<div className="flex items-center gap-2.5 min-w-0">
											<Badge variant="outline" className="text-xs font-semibold shrink-0">
												{stat.mode === "auto"
													? t("network.modeAuto")
													: stat.mode === "direct"
														? t("network.modeDirect")
														: stat.mode === "pool"
															? t("network.modePool")
															: t("network.modeNode")}
											</Badge>
											<div className="min-w-0">
												<p className="text-xs font-semibold text-foreground truncate">
													{stat.level.startsWith("scope:")
														? t(routingScopeLabelKeys[stat.level.replace("scope:", "") as EgressRoutingScope] || stat.level)
														: stat.level.startsWith("class:")
															? t(trafficClassLabelKeys[stat.level.replace("class:", "") as EgressTrafficClass] || stat.level)
															: stat.level === "default"
																? t("networkRouting.default")
																: stat.level}
												</p>
												<p className="text-[10px] text-muted-foreground">
													{stat.lastSeen ? formatTimeAgo(stat.lastSeen, i18n.language) : t("network.neverSeenTraffic")}
												</p>
											</div>
										</div>

										<div className="flex items-center gap-4 text-right shrink-0">
											<div>
												<span className="block text-xs font-bold tabular-nums text-foreground">
													{t("network.hitsCountSuffix", { count: stat.hit.toLocaleString() })}
												</span>
												<span className="text-[10px] text-muted-foreground">
													{t("network.hitCount")}
												</span>
											</div>

											<div>
												<span
													className={cn(
														"block text-xs font-bold tabular-nums",
														stat.fallback > 0 ? "text-amber-500 font-semibold" : "text-muted-foreground"
													)}
												>
													{t("network.fallbacksCountSuffix", { count: stat.fallback.toLocaleString() })}
												</span>
												<span className="text-[10px] text-muted-foreground">
													{t("network.fallbackCount")}
												</span>
											</div>
										</div>
									</div>
								))}
							</div>
						) : (
							<div className="flex h-32 flex-col items-center justify-center rounded-lg border border-dashed border-border/80 text-muted-foreground">
								<Activity className="size-6 opacity-40 mb-1.5" />
								<span className="text-xs">{t("network.neverSeenTraffic")}</span>
							</div>
						)}
					</div>

					{/* Physical Connection Runtime Gauge */}
					<div className="flex flex-col gap-3 rounded-xl border border-border/80 bg-card/70 p-5 shadow-sm">
						<div className="flex items-center justify-between border-b border-border/60 pb-3">
							<div className="flex items-center gap-2">
								<Radio className="size-4 text-primary" />
								<h3 className="text-sm font-bold text-foreground">
									{t("network.runtime")}
								</h3>
							</div>
							{runtime?.draining && (
								<Badge variant="destructive" className="text-[10px]">
									{t("network.draining")}
								</Badge>
							)}
						</div>
						<p className="text-xs text-muted-foreground leading-normal">
							{t("network.runtimeHelp")}
						</p>

						<div className="mt-2 space-y-3">
							<div>
								<div className="flex justify-between text-xs font-semibold">
									<span>{t("network.connections")}</span>
									<span className="tabular-nums font-mono">
										{runtime?.activeConnections ?? 0} / {runtime?.limits?.Connections ?? "∞"}
									</span>
								</div>
								<div className="mt-1.5 h-2 w-full overflow-hidden rounded-full bg-muted">
									<div
										className="h-full bg-blue-500 transition-all duration-300"
										style={{
											width: `${Math.min(
												100,
												runtime?.limits?.Connections
													? Math.round(
															((runtime.activeConnections ?? 0) / runtime.limits.Connections) *
																100
														)
													: 10
											)}%`,
										}}
									/>
								</div>
							</div>

							<div className="grid grid-cols-2 gap-2 pt-1.5">
								<div className="rounded-lg border border-border/60 bg-muted/20 p-2.5">
									<span className="text-[11px] text-muted-foreground block">
										{t("network.dialing")}
									</span>
									<p className="text-lg font-bold tabular-nums text-foreground mt-0.5">
										{runtime?.dialing ?? 0}
									</p>
									<span className="text-[10px] text-muted-foreground font-mono">
										{t("network.connsMax", { count: runtime?.limits?.Dialing ?? 0 })}
									</span>
								</div>

								<div className="rounded-lg border border-border/60 bg-muted/20 p-2.5">
									<span className="text-[11px] text-muted-foreground block">
										{t("network.waiters")}
									</span>
									<p
										className={cn(
											"text-lg font-bold tabular-nums mt-0.5",
											(runtime?.waiters ?? 0) > 0 ? "text-amber-500" : "text-foreground"
										)}
									>
										{runtime?.waiters ?? 0}
									</p>
									<span className="text-[10px] text-muted-foreground font-mono">
										{t("network.connsMax", { count: runtime?.limits?.Waiters ?? 0 })}
									</span>
								</div>
							</div>

							<div className="flex items-center justify-between rounded-lg border border-border/60 bg-muted/10 px-3 py-2 text-xs">
								<span className="text-muted-foreground">{t("network.idleConns")}</span>
								<span className="font-semibold tabular-nums text-foreground font-mono">
									{runtime?.idleConnections ?? 0}
								</span>
							</div>
						</div>
					</div>
				</div>

				{/* Right Side: Unified Route Simulator (5 cols) */}
				<div className="lg:col-span-5 flex flex-col">
					<ProxyRouteSimulator
						nodes={items}
						pools={pools}
						onViewPools={() => onNavigateTab("pools")}
						onViewNodes={() => onNavigateTab("nodes")}
					/>
				</div>
			</div>
		</div>
	);
}
