import { useQuery } from "@tanstack/react-query";
import { useNow } from "@/features/guard/quality-hooks";
import {
	ArrowRight,
	Check,
	CircleAlert,
	Clock3,
	GitBranch,
	Layers2,
	Network,
	Radio,
	Route,
	Server,
	ShieldAlert,
	type LucideIcon,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { useOperationsNodes, useOperationsPools } from "@/features/operations/operations-queries";
import { networkSummary, nodeCondition } from "@/features/operations/operations-data";
import { OperationsHelp } from "@/features/operations/operations-ui";
import {
	getEgressOperationsConfig,
	getEgressRoutingStats,
	listEgressSources,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressRoutingTarget,
	type EgressTrafficClass,
} from "@/features/settings/settings-api";
import { resolveEffectiveTarget } from "./effective-target";
import { getNetworkRuntime } from "./network-api";
import { NetworkButton as Button } from "./network-ui";
import {
	routingScopes,
	routingScopeLabelKeys,
	trafficClasses,
	trafficClassLabelKeys,
	useEgressOperations,
} from "./operations-shared";

const nodeLink = (condition = "all") => `/proxies?condition=${condition}#nodes`;
const isCandidate = (node: EgressNodeDTO, now: number) =>
	["ready", "dynamic"].includes(nodeCondition(node, now));
type TargetPresentation = {
	name: string;
	detail: string;
	icon: LucideIcon;
	warning: boolean;
};

export function NetworkOverview() {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const nodes = useOperationsNodes();
	const pools = useOperationsPools();
	const sources = useQuery({
		queryKey: ["egress-sources"],
		queryFn: ({ signal }) => listEgressSources(undefined, signal),
		staleTime: 15000,
		refetchInterval: 30000,
	});
	const config = useQuery({
		queryKey: ["egress-operations"],
		queryFn: ({ signal }) => getEgressOperationsConfig(signal),
	});
	const stats = useQuery({
		queryKey: ["egress-routing-stats"],
		queryFn: () => getEgressRoutingStats(),
		refetchInterval: 10_000,
	});
	const now = useNow(15000);
	const items = nodes.data?.items ?? [];
	const summary = nodes.data ? networkSummary(items, now) : undefined;
	const candidateIds = new Set(
		items.filter((node) => isCandidate(node, now)).map((node) => node.id),
	);
	const poolCapacity = (pool: EgressPoolDTO) =>
		pool.memberIds.filter((id) => candidateIds.has(id)).length;
	const count = (condition: string) =>
		items.filter((node) => nodeCondition(node, now) === condition).length;
	const held = count("held"),
		banned = count("banned"),
		unhealthy = count("unhealthy"),
		cooling = count("cooling"),
		unconfigured = count("unconfigured");
	const sourceErrors =
		sources.data?.items.filter((source) => source.enabled && source.lastSyncError).length ?? 0;
	const emptyPools =
		nodes.data && pools.data
			? pools.data.filter((pool) => pool.enabled && poolCapacity(pool) === 0)
			: [];
	const queue = [
		{
			visible: held + banned > 0,
			icon: ShieldAlert,
			title: "held",
			detail: t("network.heldDetail", { held, banned }),
			to: nodeLink("restricted"),
		},
		{
			visible: unhealthy > 0,
			icon: Radio,
			title: "unhealthy",
			detail: t("network.unhealthyDetail", { count: unhealthy }),
			to: nodeLink("unhealthy"),
		},
		{
			visible: cooling > 0,
			icon: Clock3,
			title: "cooling",
			detail: t("network.coolingDetail", { count: cooling }),
			to: nodeLink("cooling"),
		},
		{
			visible: unconfigured > 0,
			icon: Server,
			title: "unconfigured",
			detail: t("network.unconfiguredDetail", { count: unconfigured }),
			to: nodeLink("unconfigured"),
		},
		{
			visible: sourceErrors > 0,
			icon: CircleAlert,
			title: "sourceError",
			detail: t("network.sourceErrorDetail", { count: sourceErrors }),
			to: "/proxies#nodes/sources",
		},
		{
			visible: emptyPools.length > 0,
			icon: Layers2,
			title: "emptyPool",
			detail: t("network.emptyPoolDetail", { count: emptyPools.length }),
			to:
				emptyPools.length === 1
					? `/proxies?pool=${encodeURIComponent(emptyPools[0]!.id)}#pools`
					: "/proxies#pools",
		},
	].filter((item) => item.visible);
	const incomplete =
		!nodes.data ||
		!sources.data ||
		!pools.data ||
		nodes.isError ||
		sources.isError ||
		pools.isError;
	const segments = summary
		? [
				{ key: "ready", count: summary.ready },
				{ key: "attention", count: summary.attention },
				{ key: "unknown", count: summary.unknown },
				{ key: "disabled", count: summary.disabled },
			]
		: [];
	const cfg = config.data;
	// 类别卡片固定展示六类业务：显式目标直接呈现；未配置时按与后端
	// resolveTarget 相同的类别→渠道→默认链路解析——三个渠道去向一致就
	// 显示该目标，不一致则汇总为“去向随渠道”并列出去重后的目标名。
	const classCards = cfg
		? trafficClasses.map((cls): { cls: EgressTrafficClass; explicit: boolean; presentation: TargetPresentation } => {
				const explicit = cfg.classTargets[cls]?.mode ? cfg.classTargets[cls] : undefined;
				if (explicit) return { cls, explicit: true, presentation: targetPresentation(explicit) };
				const resolved = routingScopes.map((scope) => resolveEffectiveTarget(cfg, cls, scope));
				const presentations = resolved.map((target) => targetPresentation(target));
				const identities = new Set(
					resolved.map((target) => `${target.mode}:${target.nodeId ?? target.poolId ?? ""}`),
				);
				if (identities.size === 1) return { cls, explicit: false, presentation: presentations[0]! };
				return {
					cls,
					explicit: false,
					presentation: {
						name: t("network.classVaries"),
						detail: [...new Set(presentations.map((item) => item.name))].join(" · "),
						icon: GitBranch,
						warning: presentations.some((item) => item.warning),
					},
				};
			})
		: [];
	// 规则计数与路由面板同源同口径：继承的流量计入实际决定出口的规则，
	// 因此只有显式规则的卡片展示自己的计数。
	const countersFor = (level: string) => {
		const records = stats.data?.items.filter((stat) => stat.level === level) ?? [];
		return {
			known: Boolean(stats.data) && !stats.isError,
			hit: records.reduce((total, stat) => total + stat.hit, 0),
			fallback: records.reduce((total, stat) => total + stat.fallback, 0),
		};
	};
	function targetPresentation(target: EgressRoutingTarget): TargetPresentation {
		if (target.mode === "direct")
			return {
				name: t("network.direct"),
				detail: t("network.directDetail"),
				icon: Network,
				warning: false,
			};
		if (target.mode === "auto")
			return {
				name: t("network.automatic"),
				detail: t("network.autoDetail"),
				icon: Route,
				warning: false,
			};
		if (target.mode === "node") {
			const node = items.find((item) => item.id === target.nodeId);
			return {
				name: node?.name ?? t(nodes.data ? "network.missing" : "network.dataUnavailable"),
				// 详情行显示目标类型（固定节点/动态隧道），与代理池、自动调度口径一致；
				// 健康异常通过警示色名称表达，不再占用详情行。
				detail: node
					? node.accountBoundProxy
						? t("network.accountBoundTarget")
						: t(node.rotatingEndpoint ? "network.dynamicTarget" : "network.node")
					: t("network.node"),
				icon: Server,
				warning: Boolean(nodes.data && (!node || node.accountBoundProxy || !isCandidate(node, now))),
			};
		}
		const pool = pools.data?.find((item) => item.id === target.poolId);
		return {
			name: pool?.name ?? t(pools.data ? "network.missing" : "network.dataUnavailable"),
			detail: pool
				? !pool.enabled
					? t("network.poolDisabled")
					: nodes.data
						? t("network.memberCapacity", {
								ready: poolCapacity(pool),
								total: pool.memberIds.length,
							})
						: t("network.dataUnavailable")
				: t("network.pool"),
			icon: Layers2,
			warning: Boolean(
				pools.data && (!pool || !pool.enabled || (nodes.data && poolCapacity(pool) === 0)),
			),
		};
	}
	return (
		<div className="network-overview">
			<div className="network-overview-top">
				<section className="network-surface network-capacity">
					<div className="network-section-title">
						<h2>{t("network.capacity")}</h2>
						<OperationsHelp label={t("network.capacity")}>
							{t("network.capacityHelp")}
						</OperationsHelp>
					</div>
					{summary ? (
						summary.total > 0 ? (
							<>
								<div className="network-capacity-value">
									<strong>{summary.ready}</strong>
									<span>/ {summary.total}</span>
									<div>
										{t("network.available")}
										<small>{t("network.dynamic", { count: summary.dynamic })}</small>
									</div>
								</div>
								<div className="network-capacity-bar" aria-hidden="true">
									{segments
										.filter((segment) => segment.count > 0)
										.map((segment) => (
											<span
												key={segment.key}
												data-condition={segment.key}
												style={{ flex: segment.count }}
											/>
										))}
								</div>
								<div className="network-capacity-legend">
									{segments.map((segment) => (
										<Link key={segment.key} to={nodeLink(segment.key)}>
											<i data-condition={segment.key} />
											<span>{t(`network.${segment.key}`)}</span>
											<b>{segment.count}</b>
										</Link>
									))}
								</div>
								<div className="network-capacity-footer">
									<span>
										{t("network.median")}
										<OperationsHelp label={t("network.median")}>
											{t("network.medianHelp")}
										</OperationsHelp>
									</span>
									<b>{summary.median === null ? "—" : `${summary.median} ms`}</b>
								</div>
							</>
						) : (
							<div className="network-empty">
								<Server />
								<h3>{t("network.emptyTitle")}</h3>
								<p>{t("network.emptyDescription")}</p>
								<Button asChild size="sm">
									<Link to={nodeLink()}>
										{t("network.manageResources")}
										<ArrowRight />
									</Link>
								</Button>
							</div>
						)
					) : (
						<div className="network-capacity-loading" aria-label={t("network.loading")}>
							<div className="h-14 w-40 animate-pulse rounded-md bg-muted" />
							<div className="h-3 w-full animate-pulse rounded-md bg-muted" />
							<div className="h-5 w-3/4 animate-pulse rounded-md bg-muted" />
						</div>
					)}
				</section>
				<section className="network-surface network-attention">
					<div className="network-section-title">
						<h2>
							{t("network.queue")}
							<OperationsHelp label={t("network.queue")}>{t("network.queueHelp")}</OperationsHelp>
						</h2>
						<span>{t("network.queueCount", { count: queue.length })}</span>
					</div>
					{queue.length > 0 ? (
						<ul className="network-queue">
							{queue.map(({ title, detail, to, icon: Icon }) => (
								<li key={title}>
									<span className="network-queue-icon">
										<Icon />
									</span>
									<div>
										<h3>{t(`network.${title}`)}</h3>
										<p>{detail}</p>
									</div>
									<Link to={to} aria-label={`${t("network.handle")} · ${t(`network.${title}`)}`}>
										{t("network.handle")}
										<ArrowRight />
									</Link>
								</li>
							))}
						</ul>
					) : !incomplete ? (
						<div className="network-empty network-empty-quiet">
							<Check />
							<h3>{t("network.noIssues")}</h3>
							<p>{t("network.noIssuesDetail")}</p>
						</div>
					) : null}
					{incomplete && (
						<div className="network-partial">
							<p>{t("network.partialQueue")}</p>
							{(sources.isError || nodes.isError || pools.isError) && (
								<Button
									size="sm"
									variant="ghost"
									onClick={() => {
										void sources.refetch();
										void nodes.refetch();
										void pools.refetch();
									}}
								>
									{t("common.retry")}
								</Button>
							)}
						</div>
					)}
				</section>
			</div>
			<section className="network-surface network-destinations">
				<div className="network-section-title">
					<h2>{t("network.routes")}</h2>
					<Button asChild size="sm" variant="ghost">
						<Link to="/proxies#routing">
							{t("network.editRoutes")}
							<ArrowRight />
						</Link>
					</Button>
				</div>
				{operations.isDirty && <p className="network-draft-notice">{t("network.savedOnly")}</p>}
				{cfg ? (
					<>
						<div className="network-destination-grid">
							{routingScopes.map((scope) => {
								const explicit = cfg.scopeTargets[scope];
								const target = targetPresentation(
									explicit ?? cfg.defaultTarget ?? { mode: "auto" },
								);
								const Icon = target.icon;
								return (
									<Link
										key={scope}
										className="network-destination"
										to={`/proxies?rule=scope:${scope}#routing`}
									>
										<div className="network-destination-scope">
											<span>{t(routingScopeLabelKeys[scope])}</span>
											<small>{t(explicit ? "network.explicit" : "network.inherit")}</small>
										</div>
										<div className="network-destination-target" data-warning={target.warning}>
											<Icon />
											<ArrowRight />
											<strong>{target.name}</strong>
											{explicit && <DestinationCounters {...countersFor(`scope:${scope}`)} />}
										</div>
										<p>{target.detail}</p>
									</Link>
								);
							})}
						</div>
						<div className="network-classes-block">
							<div className="network-classes-heading">
								<h3>{t("network.classDestinations")}</h3>
								<span>
									{t("network.classOverrideCount", {
										count: classCards.filter((card) => card.explicit).length,
										total: trafficClasses.length,
									})}
								</span>
							</div>
							<div className="network-destination-grid">
								{classCards.map(({ cls, explicit, presentation }) => {
									const Icon = presentation.icon;
									return (
										<Link
											key={cls}
											className="network-destination network-destination-class"
											to={`/proxies?rule=class:${cls}#routing`}
										>
											<div className="network-destination-scope">
												<span>{t(trafficClassLabelKeys[cls])}</span>
												{!explicit && <small>{t("network.classInherited")}</small>}
											</div>
											<div className="network-destination-target" data-warning={presentation.warning}>
												<Icon />
												<ArrowRight />
												<strong>{presentation.name}</strong>
												{explicit && <DestinationCounters {...countersFor(`class:${cls}`)} />}
											</div>
											<p>{presentation.detail}</p>
										</Link>
									);
								})}
							</div>
						</div>
					</>
				) : (
					<p className="network-partial">
						{t(config.isError ? "network.dataUnavailable" : "network.loading")}
					</p>
				)}
			</section>
			<RuntimeCapacity />
		</div>
	);
}

function DestinationCounters({ hit, fallback, known }: { hit: number; fallback: number; known: boolean }) {
	const { t, i18n } = useTranslation();
	const format = (value: number) => value.toLocaleString(i18n.language);
	const label = known
		? `${t("networkRouting.targetSelected")} ${format(hit)} · ${t("networkRouting.fallbackRefusal")} ${format(fallback)}`
		: t("network.dataUnavailable");
	return (
		<span className="network-destination-counters" title={label} aria-label={label}>
			<span className="network-destination-counter-values" aria-hidden="true">
				<b>{known ? format(hit) : "—"}</b>
				{known && fallback > 0 && (
					<>
						<i>/</i>
						<b data-kind="fallback">{format(fallback)}</b>
					</>
				)}
			</span>
		</span>
	);
}

function RuntimeCapacity() {
	const { t, i18n } = useTranslation();
	const runtime = useQuery({
		queryKey: ["egress-runtime"],
		queryFn: ({ signal }) => getNetworkRuntime(signal),
		staleTime: 5000,
		refetchInterval: 15000,
	});
	const network = runtime.data?.network;
	const metrics = network
		? [
				{ label: "requests", value: network.requests, limit: network.limits.Requests },
				{ label: "waiters", value: network.waiters, limit: network.limits.Waiters },
				{ label: "connections", value: network.connections, limit: network.limits.Connections },
				{ label: "dialing", value: network.dialing, limit: network.limits.Dialing },
			]
		: [];
	return (
		<section className="network-runtime network-surface">
			<div className="network-section-title">
				<h2>
					{t("network.runtime")}
					<OperationsHelp label={t("network.runtime")}>{t("network.runtimeHelp")}</OperationsHelp>
				</h2>
				<span>
					{runtime.dataUpdatedAt > 0 &&
						t("network.refreshed", {
							time: new Date(runtime.dataUpdatedAt).toLocaleTimeString(i18n.language, {
								hour12: false,
							}),
						})}
				</span>
			</div>
			{network && !runtime.isError ? (
				<>
					<dl className="network-runtime-grid">
						{metrics.map((metric) => (
							<div key={metric.label}>
								<dt>{t(`network.${metric.label}`)}</dt>
								<dd>
									{metric.limit > 0 ? metric.value.toLocaleString(i18n.language) : "—"}
									<small>
										{metric.limit > 0
											? t("network.limit", { count: metric.limit })
											: t("network.dataUnavailable")}
									</small>
								</dd>
								<div className="network-runtime-bar" aria-hidden="true">
									<i
										style={{
											width: `${metric.limit > 0 ? Math.min(100, (100 * metric.value) / metric.limit) : 0}%`,
										}}
									/>
								</div>
							</div>
						))}
					</dl>
					{network.draining && (
						<p role="status" className="network-draft-notice">
							{t("network.draining")}
						</p>
					)}
				</>
			) : (
				<div className="network-partial">
					<p>{t(runtime.isError ? "network.runtimeUnavailable" : "network.loading")}</p>
					{runtime.isError && (
						<Button size="sm" variant="ghost" onClick={() => void runtime.refetch()}>
							{t("common.retry")}
						</Button>
					)}
				</div>
			)}
		</section>
	);
}
