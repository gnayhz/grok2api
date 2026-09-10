import {
	OperationsHelp,
	StatusPill,
} from "@/features/operations/operations-ui";
import { useQuery } from "@tanstack/react-query";
import React from "react";
import { ArrowRight, Search } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Input } from "@/components/ui/input";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { useEgressOperations } from "@/features/proxies/operations-shared";
import {
	fixedTargetCandidates,
	nodeCooling,
	routingScopes,
	routingScopeLabelKeys,
	trafficClassLabelKeys,
	trafficClasses,
	type EgressOperationsDraft,
} from "@/features/proxies/operations-shared";
import {
	getEgressRoutingStats,
	listAllEgressNodes,
	listEgressPools,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressRoutingTarget,
} from "@/features/settings/settings-api";
import { resolveEffectiveTarget } from "@/features/proxies/effective-target";
import { ErrorState, LoadingState } from "@/shared/components/data-state";

/** 可搜索的资源选择:下拉顶部过滤输入按名称/IP 实时筛,搜索框吸顶不随列表滚动。 */
function SearchableResourceSelect({
	placeholder,
	items,
	value,
	disabled,
	onChange,
	unavailableLabel,
}: {
	placeholder: string;
	items: { id: string; label: string }[];
	value?: string;
	disabled?: boolean;
	onChange: (id: string) => void;
	unavailableLabel?: string;
}) {
	const { t } = useTranslation();
	const [open, setOpen] = React.useState(false);
	const [filter, setFilter] = React.useState("");
	const needle = filter.trim().toLocaleLowerCase();
	const visible = needle
		? items.filter((item) => item.label.toLocaleLowerCase().includes(needle))
		: items;
	const selected = items.find((item) => item.id === value);
	return (
		<Select
			open={open}
			onOpenChange={(next) => {
				setOpen(next);
				if (!next) setFilter("");
			}}
			value={value ?? ""}
			disabled={disabled}
			onValueChange={onChange}
		>
			<SelectTrigger aria-label={placeholder} className="min-w-40 flex-1">
				<SelectValue placeholder={t("proxies.routing.targetUnavailable")} />
			</SelectTrigger>
			<SelectContent
				selectHeader={
					<div
						className="shrink-0 border-b px-1.5 py-1.5"
						onKeyDown={(event) => event.stopPropagation()}
					>
						<div className="relative">
							<Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
							<Input
								autoFocus
								className="h-8 border-0 bg-secondary/55 pl-8 text-xs shadow-none focus-visible:ring-0"
								value={filter}
								placeholder={placeholder}
								onChange={(event) => setFilter(event.target.value)}
							/>
						</div>
					</div>
				}
			>
				{!selected && value ? (
					<SelectItem value={value} disabled>
						{unavailableLabel ?? "—"}
					</SelectItem>
				) : null}
				{visible.length === 0 ? (
					<p className="px-2 py-3 text-center text-xs text-muted-foreground">
						{t("settings.egress.noMatches")}
					</p>
				) : null}
				{visible.slice(0, 200).map((item) => (
					<SelectItem
						key={item.id}
						value={item.id}
						className="max-w-96 truncate"
					>
						{item.label}
					</SelectItem>
				))}
			</SelectContent>
		</Select>
	);
}

/** 池目标选择:实体池列表(含已停用,标注状态),带搜索。 */
function PoolTargetSelect({
	pools,
	value,
	onChange,
}: {
	pools: EgressPoolDTO[];
	value?: string;
	onChange: (poolId: string) => void;
}) {
	const { t } = useTranslation();
	const [open, setOpen] = React.useState(false);
	const [filter, setFilter] = React.useState("");
	const items = pools.map((pool) => ({
		id: pool.id,
		label: pool.enabled
			? pool.name
			: pool.name + " · " + t("proxies.routing.poolDisabled"),
	}));
	const needle = filter.trim().toLocaleLowerCase();
	const visible = needle
		? items.filter((item) => item.label.toLocaleLowerCase().includes(needle))
		: items;
	const selected = items.find((item) => item.id === value);
	return (
		<Select
			open={open}
			onOpenChange={(next) => {
				setOpen(next);
				if (!next) setFilter("");
			}}
			value={value ?? ""}
			onValueChange={onChange}
		>
			<SelectTrigger
				aria-label={t("proxies.routing.targetPool")}
				className="min-w-40 flex-1"
			>
				<SelectValue placeholder={t("proxies.routing.targetUnavailable")} />
			</SelectTrigger>
			<SelectContent
				selectHeader={
					<div
						className="shrink-0 border-b px-1.5 py-1.5"
						onKeyDown={(event) => event.stopPropagation()}
					>
						<div className="relative">
							<Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
							<Input
								autoFocus
								className="h-8 border-0 bg-secondary/55 pl-8 text-xs shadow-none focus-visible:ring-0"
								value={filter}
								placeholder={t("settings.egress.search")}
								onChange={(event) => setFilter(event.target.value)}
							/>
						</div>
					</div>
				}
			>
				{!selected && value ? (
					<SelectItem value={value} disabled>
						{t("proxies.routing.targetUnavailable")}
					</SelectItem>
				) : null}
				{visible.map((item) => (
					<SelectItem
						key={item.id}
						value={item.id}
						className="max-w-96 truncate"
					>
						{item.label}
					</SelectItem>
				))}
				{visible.length === 0 ? (
					<p className="px-2 py-3 text-center text-xs text-muted-foreground">
						{t("settings.egress.noMatches")}
					</p>
				) : null}
			</SelectContent>
		</Select>
	);
}

type PickerProps = {
	value: EgressRoutingTarget;
	onChange: (target: EgressRoutingTarget) => void;
	nodes: EgressNodeDTO[];
	pools: EgressPoolDTO[];
};

/**
 * 双下拉:目标类型(未配置 / 直连 / 固定节点 / 代理池)+ 对应资源。
 * 资源下拉带吸顶搜索框(节点多时按名称/IP 过滤)。
 */
function RoutingTargetPicker({ value, onChange, nodes, pools }: PickerProps) {
	const { t } = useTranslation();
	const mode = value.mode;
	const selectedNode = nodes.some((node) => node.id === value.nodeId);
	const selectedPool = pools.some((pool) => pool.id === value.poolId);
	function setMode(next: string) {
		if (next === "auto") {
			onChange({ mode: "auto" });
			return;
		}
		if (next === "direct") {
			onChange({ mode: "direct" });
			return;
		}
		if (next === "node") {
			onChange({ mode: "node", nodeId: nodes[0]?.id });
			return;
		}
		onChange({ mode: "pool", poolId: pools[0]?.id });
	}
	return (
		<div className="flex min-w-0 flex-wrap items-center gap-2">
			<Select value={mode} onValueChange={setMode}>
				<SelectTrigger
					aria-label={t("proxies.routing.targetMode")}
					className="w-28"
				>
					<SelectValue placeholder={t("proxies.routing.targetUnavailable")} />
				</SelectTrigger>
				<SelectContent>
					<SelectItem value="auto">
						{t("proxies.routing.classFollowScope")}
					</SelectItem>
					<SelectItem value="direct">
						{t("proxies.routing.targetDirect")}
					</SelectItem>
					<SelectItem value="node" disabled={nodes.length === 0}>
						{t("proxies.routing.targetNode")}
					</SelectItem>
					<SelectItem value="pool" disabled={pools.length === 0}>
						{t("proxies.routing.targetPool")}
					</SelectItem>
				</SelectContent>
			</Select>
			{mode === "node" ? (
				<SearchableResourceSelect
					placeholder={t("settings.egress.search")}
					items={nodes.map((node) => ({
						id: node.id,
						label:
							node.name +
							(node.exitIp ? " · " + node.exitIp : "") +
							(nodeCooling(node)
								? " · " + t("proxies.routing.nodeCooling")
								: ""),
					}))}
					value={selectedNode ? value.nodeId : undefined}
					disabled={nodes.length === 0}
					onChange={(nodeId) => onChange({ mode: "node", nodeId })}
					unavailableLabel={t("proxies.routing.targetUnavailable")}
				/>
			) : null}
			{mode === "pool" ? (
				<PoolTargetSelect
					pools={pools}
					value={selectedPool ? value.poolId : undefined}
					onChange={(poolId) => onChange({ mode: "pool", poolId })}
				/>
			) : null}
		</div>
	);
}

/** 出口徽标:直观显示"这类流量最终从哪出去"。 */
function EffectiveTargetBadge({
	target,
	nodes,
	pools,
	nodeCount,
}: {
	target: EgressRoutingTarget;
	nodes: EgressNodeDTO[];
	pools: EgressPoolDTO[];
	nodeCount: number;
}) {
	const { t } = useTranslation();
	const label =
		target.mode === "direct"
			? t("proxies.routing.targetDirectShort")
			: target.mode === "node"
				? (nodes.find((n) => n.id === target.nodeId)?.name ??
					t("proxies.routing.targetUnavailable"))
				: target.mode === "pool"
					? (pools.find((p) => p.id === target.poolId)?.name ??
						t("proxies.routing.targetUnavailable"))
					: t("proxies.routing.autoScheduleCount", { count: nodeCount });
	return (
		<span className="inline-flex min-w-0 items-center gap-2 text-xs font-medium">
			<ArrowRight className="size-3 shrink-0 text-muted-foreground" />
			<span className="truncate" title={label}>
				{label}
			</span>
		</span>
	);
}

/** 命中统计小标签:该路由行实际的调度命中/回退(进程内计数)。 */
function HitStatBadge({ hit, fallback }: { hit: number; fallback: number }) {
	const { t } = useTranslation();
	if (!hit && !fallback) return null;
	return (
		<span
			className="mt-1 block text-[11px] text-muted-foreground"
			title={t("proxies.routing.statsHint")}
		>
			{t("proxies.routing.statsMini", { hit, fallback })}
		</span>
	);
}
function SectionHeader({ label, help }: { label: string; help?: string }) {
	return (
		<div className="ops-route-group-title">
			<span>{label}</span>
			{help && <OperationsHelp>{help}</OperationsHelp>}
		</div>
	);
}
function RouteRow({
	label,
	configured,
	badge,
	stats,
	picker,
}: {
	label: string;
	configured?: boolean;
	badge: React.ReactNode;
	stats?: { hit: number; fallback: number };
	picker: React.ReactNode;
}) {
	return (
		<div className="ops-route-row">
			<div className="min-w-0">
				<p className="font-medium">
					{label}
					{configured && (
						<span className="ml-2 inline-block size-1.5 rounded-full bg-current" />
					)}
				</p>
				{stats && <HitStatBadge hit={stats.hit} fallback={stats.fallback} />}
			</div>
			{badge}
			<div className="min-w-0">{picker}</div>
		</div>
	);
}

/**
 * 出口路由：语义（流量类别）→ 作用域 → 总出口 → 自动调度。节点和代理池只是
 * 资源；这一页决定每类流量从哪里出去。
 */
export function RoutingPanel() {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const nodesQuery = useQuery({
		queryKey: ["egress-nodes", "routing-options"],
		queryFn: () => listAllEgressNodes(),
	});
	const poolsQuery = useQuery({
		queryKey: ["egress-pools", "routing-options"],
		queryFn: () => listEgressPools(),
	});
	const statsQuery = useQuery({
		queryKey: ["egress-routing-stats"],
		queryFn: () => getEgressRoutingStats(),
		refetchInterval: 10_000,
	});
	const nodes = fixedTargetCandidates(nodesQuery.data?.items ?? []);
	// 目标下拉列出全部池:已停用的池标注状态——运行时严格失败(强绑定),
	// 但配置值不能在界面上静默消失。
	const pools = poolsQuery.data ?? [];
	const allNodes = nodesQuery.data?.items ?? [];
	const enabledCount = allNodes.filter((node) => node.enabled).length;
	const nodesError = nodesQuery.isError || poolsQuery.isError;

	if (operations.isError) {
		return (
			<ErrorState
				message={operations.errorMessage ?? t("errors.generic")}
				onRetry={operations.retry}
			/>
		);
	}
	if (operations.isPending) return <LoadingState />;
	if (nodesError) {
		return (
			<ErrorState
				message={t("errors.generic")}
				onRetry={() => {
					void nodesQuery.refetch();
					void poolsQuery.refetch();
				}}
			/>
		);
	}

	const defaultTarget = operations.form.defaultTarget;
	const stats = statsQuery.data?.items ?? [];
	// 行级命中统计:该 level(类别/作用域)所有模式的命中+回退合计。
	function statsFor(
		level: string,
	): { hit: number; fallback: number } | undefined {
		let hit = 0;
		let fallback = 0;
		let seen = false;
		for (const stat of stats) {
			if (stat.level !== level) continue;
			hit += stat.hit;
			fallback += stat.fallback;
			seen = true;
		}
		return seen ? { hit, fallback } : undefined;
	}

	function classEffectiveBadge(cls: (typeof trafficClasses)[number]) {
		return (
			<ClassEffectiveBadgeCell
				cls={cls}
				nodes={nodes}
				pools={pools}
				nodeCount={enabledCount}
				form={operations.form}
				defaultTarget={defaultTarget}
			/>
		);
	}

	function setScopeTarget(
		scope: (typeof routingScopes)[number],
		target: EgressRoutingTarget,
	) {
		operations.update((current) => {
			const next = { ...current.scopeTargets };
			if (target.mode === "auto") delete next[scope];
			else next[scope] = target;
			return { ...current, scopeTargets: next };
		});
	}

	function setClassTarget(
		cls: (typeof trafficClasses)[number],
		target: EgressRoutingTarget,
	) {
		operations.update((current) => {
			const next = { ...current.classTargets };
			if (target.mode === "auto") delete next[cls];
			else next[cls] = target;
			return { ...current, classTargets: next };
		});
	}

	// 生效解析:与后端 TargetFor 同规则 —— 类别 → 作用域 → 总出口 → 自动调度。
	// 显式 auto 是该层终态（自动调度），不是回落到下一层。
	function resolveEffective(
		cls?: (typeof trafficClasses)[number],
		scope?: (typeof routingScopes)[number],
	): EgressRoutingTarget {
		return resolveEffectiveTarget(
			{
				defaultTarget,
				scopeTargets: operations.form.scopeTargets,
				classTargets: operations.form.classTargets,
			},
			cls,
			scope,
		);
	}

	return (
		<div className="space-y-4">
			<div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
				<span>{t("ops.routePriorityShort")}</span>
				<StatusPill>
					{t(
						operations.isDirty ? "ops.routePreviewDraft" : "ops.settingsSaved",
					)}
				</StatusPill>
			</div>

			<div className="ops-setting-group">
				<div className="ops-route-row ops-route-heading">
					<span>{t("ops.routingRules")}</span>
					<span>{t("ops.routeApplied")}</span>
					<span>{t("ops.routeConfig")}</span>
				</div>
				{/* 总出口 */}
				<SectionHeader
					label={t("proxies.routing.defaultTarget")}
					help={t("proxies.routing.defaultTargetHelp")}
				/>
				<RouteRow
					label={t("proxies.routing.defaultTarget")}
					configured={defaultTarget.mode !== "auto"}
					badge={
						<EffectiveTargetBadge
							target={defaultTarget}
							nodes={nodes}
							pools={pools}
							nodeCount={enabledCount}
						/>
					}
					stats={statsFor("default")}
					picker={
						<RoutingTargetPicker
							value={defaultTarget}
							onChange={(target) =>
								operations.update((current) => ({
									...current,
									defaultTarget: target,
								}))
							}
							nodes={nodes}
							pools={pools}
						/>
					}
				/>

				{/* 作用域出口 */}
				<SectionHeader
					label={t("proxies.routing.scopeSection")}
					help={t("proxies.routing.scopeSectionHelp")}
				/>
				<div className="divide-y">
					{routingScopes.map((scope) => {
						const configured = operations.form.scopeTargets[scope];
						const isSet = Boolean(configured && configured.mode !== "auto");
						return (
							<RouteRow
								key={scope}
								label={t(routingScopeLabelKeys[scope])}
								configured={isSet}
								badge={
									<EffectiveTargetBadge
										target={resolveEffective(undefined, scope)}
										nodes={nodes}
										pools={pools}
										nodeCount={enabledCount}
									/>
								}
								stats={statsFor("scope:" + scope)}
								picker={
									<RoutingTargetPicker
										value={configured ?? { mode: "auto" }}
										onChange={(target) => setScopeTarget(scope, target)}
										nodes={nodes}
										pools={pools}
									/>
								}
							/>
						);
					})}
				</div>

				<details
					className="ops-disclosure"
					open={Object.keys(operations.form.classTargets).length > 0}
				>
					<summary className="px-4 py-3">
						{t("proxies.routing.classSection")} · {t("ops.advancedSettings")}
					</summary>{" "}
					{/* 语义路由:最具体,覆盖一切 */}
					<SectionHeader
						label={t("proxies.routing.classSection")}
						help={t("proxies.routing.classSectionHelp")}
					/>
					<div className="divide-y">
						{trafficClasses.map((cls) => {
							const configured = operations.form.classTargets[cls];
							const isSet = Boolean(configured && configured.mode !== "auto");
							return (
								<RouteRow
									key={cls}
									label={t(trafficClassLabelKeys[cls])}
									configured={isSet}
									badge={classEffectiveBadge(cls)}
									stats={statsFor("class:" + cls)}
									picker={
										<RoutingTargetPicker
											value={configured ?? { mode: "auto" }}
											onChange={(target) => setClassTarget(cls, target)}
											nodes={nodes}
											pools={pools}
										/>
									}
								/>
							);
						})}
					</div>
				</details>
				{/* 兜底:自动调度 */}
				<div className="flex items-center justify-between gap-3 bg-muted/20 px-3 py-2 text-xs text-muted-foreground">
					<span className="flex items-center gap-2">
						<ArrowRight className="size-3.5" />
						{t("proxies.routing.autoScheduleRoot")}
					</span>
					<EffectiveTargetBadge
						target={{ mode: "auto" }}
						nodes={nodes}
						pools={pools}
						nodeCount={enabledCount}
					/>
				</div>
			</div>
		</div>
	);
}

/**
 * 类别行生效徽标:同一类别可能来自多个作用域(推理/账单横跨 Build/Web/
 * Console)。类别已配置时直接展示;未配置而任一作用域有配置时,各作用域
 * 解析结果可能不同,展示"跟随作用域"而不是猜一个;作用域全空则解析到
 * 总出口/自动调度,那是唯一确定的。
 */
function ClassEffectiveBadgeCell({
	cls,
	nodes,
	pools,
	nodeCount,
	form,
	defaultTarget,
}: {
	cls: (typeof trafficClasses)[number];
	nodes: EgressNodeDTO[];
	pools: EgressPoolDTO[];
	nodeCount: number;
	form: EgressOperationsDraft;
	defaultTarget: EgressRoutingTarget;
}) {
	const { t } = useTranslation();
	const configured = form.classTargets[cls];
	if (configured && configured.mode !== "auto") {
		return (
			<EffectiveTargetBadge
				target={configured}
				nodes={nodes}
				pools={pools}
				nodeCount={nodeCount}
			/>
		);
	}
	const anyScopeSet = routingScopes.some(
		(scope) =>
			form.scopeTargets[scope] && form.scopeTargets[scope]!.mode !== "auto",
	);
	if (anyScopeSet) {
		return (
			<span
				className="text-xs text-muted-foreground"
				title={t("proxies.routing.followsScopeHelp")}
			>
				{t("proxies.routing.followsScope")}
			</span>
		);
	}
	return (
		<EffectiveTargetBadge
			target={defaultTarget.mode !== "auto" ? defaultTarget : { mode: "auto" }}
			nodes={nodes}
			pools={pools}
			nodeCount={nodeCount}
		/>
	);
}
