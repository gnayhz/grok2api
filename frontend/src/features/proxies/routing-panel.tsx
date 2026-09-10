import { useQuery } from "@tanstack/react-query";
import { ArrowRight, GitBranch, Globe2, Layers3, Search, Server } from "lucide-react";
import { useId, useState } from "react";
import { useTranslation } from "react-i18next";

import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useNow } from "@/features/guard/quality-hooks";
import { nodeCondition } from "@/features/operations/operations-data";
import { StatusPill, type StatusTone } from "@/features/operations/operations-ui";
import { resolveEffectiveTarget } from "@/features/proxies/effective-target";
import {
	fixedTargetCandidates,
	routingScopes,
	routingScopeLabelKeys,
	trafficClasses,
	trafficClassLabelKeys,
	useEgressOperations,
} from "@/features/proxies/operations-shared";
import {
	getEgressRoutingStats,
	listAllEgressNodes,
	listEgressPools,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressRoutingScope,
	type EgressRoutingTarget,
	type EgressTrafficClass,
} from "@/features/settings/settings-api";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import { NetworkButton, NetworkField, NetworkSelect, NetworkText } from "./network-ui";
import "./routing-panel.css";

type RoutingPanelProps = { initialRule?: string };
type RouteRule = {
	id: string;
	label: string;
	target?: EgressRoutingTarget;
	inheritLabel?: string;
	onChange: (target?: EgressRoutingTarget) => void;
};
type TargetSummary = { name: string; detail: string; status: string; tone: StatusTone };

function ResourceSelect({
	id,
	label,
	items,
	value,
	onChange,
}: {
	id: string;
	label: string;
	items: { id: string; label: string }[];
	value?: string;
	onChange: (id: string) => void;
}) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const [filter, setFilter] = useState("");
	const needle = filter.trim().toLocaleLowerCase();
	const visible = items.filter((item) => item.label.toLocaleLowerCase().includes(needle));
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
			<SelectTrigger id={id} aria-label={label}>
				<SelectValue placeholder={t("networkRouting.chooseResource")}>
					{selected?.label ?? (value ? t("networkRouting.unavailableResource", { id: value }) : undefined)}
				</SelectValue>
			</SelectTrigger>
			<SelectContent
				className="nroute-resource-menu"
				selectHeader={
					<div className="nroute-resource-search" onKeyDown={(event) => {
						if (event.key !== "Escape") event.stopPropagation();
					}}>
						<Search aria-hidden="true" />
						<Input
							autoFocus
							value={filter}
							aria-label={t("networkRouting.searchResources")}
							placeholder={t("networkRouting.searchResources")}
							onChange={(event) => setFilter(event.target.value)}
						/>
					</div>
				}
			>
				{value && !selected && (
					<SelectItem value={value} disabled>{t("networkRouting.unavailableResource", { id: value })}</SelectItem>
				)}
				{visible.length === 0 && <p className="nroute-menu-note">{t("networkRouting.noResources")}</p>}
				{visible.length > 200 && <p className="nroute-menu-note">{t("networkRouting.refineSearch")}</p>}
				{visible.slice(0, 200).map((item) => (
					<SelectItem key={item.id} value={item.id} className="nroute-resource-option">{item.label}</SelectItem>
				))}
			</SelectContent>
		</Select>
	);
}

function TargetEditor({ rule, nodes, pools, now }: { rule: RouteRule; nodes: EgressNodeDTO[]; pools: EgressPoolDTO[]; now: number }) {
	const { t } = useTranslation();
	const id = useId();
	const mode = rule.target?.mode || (rule.inheritLabel ? "inherit" : "auto");
	// Inheritance removes the rule. A stored explicit auto is a terminal route,
	// so it keeps its own visible value until the operator chooses a replacement.
	const explicitAuto = Boolean(rule.inheritLabel && mode === "auto");
	const options = [
		{ value: rule.inheritLabel ? "inherit" : "auto", label: rule.inheritLabel ?? t("networkRouting.auto") },
		...(explicitAuto ? [{ value: "auto", label: t("networkRouting.explicitAuto"), disabled: true }] : []),
		{ value: "pool", label: t("networkRouting.pool"), disabled: pools.length === 0 },
		{ value: "node", label: t("networkRouting.node"), disabled: nodes.length === 0 },
		{ value: "direct", label: t("networkRouting.direct") },
	];
	return (
		<div className="nroute-editor">
			<div className="nroute-fields">
				<NetworkField controlId={`${id}-mode`} label={t("networkRouting.targetMode")}>
					<NetworkSelect
						id={`${id}-mode`}
						label={t("networkRouting.targetMode")}
						value={mode}
						options={options}
						onChange={(next) => {
							if (next === "inherit") rule.onChange(undefined);
							else if (next === "node") rule.onChange({ mode: "node", nodeId: nodes[0]?.id });
							else if (next === "pool") rule.onChange({ mode: "pool", poolId: pools[0]?.id });
							else if (next === "auto" || next === "direct") rule.onChange({ mode: next });
						}}
					/>
				</NetworkField>
				{(mode === "node" || mode === "pool") && (
					<NetworkField controlId={`${id}-resource`} label={t("networkRouting.targetResource")}>
						<ResourceSelect
							id={`${id}-resource`}
							label={t("networkRouting.targetResource")}
							value={mode === "node" ? rule.target?.nodeId : rule.target?.poolId}
							items={mode === "node" ? nodes.map((node) => ({
								id: node.id,
								label: [node.name, node.exitIp, t(`ops.condition.${nodeCondition(node, now)}`)].filter(Boolean).join(" · "),
							})) : pools.map((pool) => ({
								id: pool.id,
								label: pool.name + (pool.enabled ? "" : ` · ${t("networkRouting.disabled")}`),
							}))}
							onChange={(resource) => rule.onChange(mode === "node" ? { mode: "node", nodeId: resource } : { mode: "pool", poolId: resource })}
						/>
					</NetworkField>
				)}
			</div>
			{explicitAuto && <p className="nroute-note">{t("networkRouting.explicitAutoHelp")}</p>}
		</div>
	);
}

function TargetIcon({ target }: { target: EgressRoutingTarget }) {
	const Icon = target.mode === "pool" ? Layers3 : target.mode === "node" ? Server : target.mode === "direct" ? Globe2 : GitBranch;
	return <Icon aria-hidden="true" />;
}

/** Navigation may reset the selected rule without resetting the shared draft. */
export function RoutingPanel({ initialRule = "default" }: RoutingPanelProps) {
	return <RoutingWorkspace key={initialRule} initialRule={initialRule} />;
}

function RoutingWorkspace({ initialRule }: { initialRule: string }) {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const now = useNow(15_000);
	const [selected, setSelected] = useState(() => {
		if (initialRule === "classes" || initialRule.startsWith("class:")) return "classes";
		return routingScopes.some((scope) => initialRule === `scope:${scope}`) ? initialRule : "default";
	});
	const [selectedClass, setSelectedClass] = useState<EgressTrafficClass>(
		trafficClasses.find((cls) => initialRule === `class:${cls}`) ?? "inference",
	);
	const id = useId();
	const nodesQuery = useQuery({ queryKey: ["egress-nodes", "routing-options"], queryFn: ({ signal }) => listAllEgressNodes({}, signal) });
	const poolsQuery = useQuery({ queryKey: ["egress-pools", "routing-options"], queryFn: () => listEgressPools() });
	const statsQuery = useQuery({ queryKey: ["egress-routing-stats"], queryFn: () => getEgressRoutingStats(), refetchInterval: 10_000 });

	if (operations.isError) return <ErrorState message={operations.errorMessage ?? t("errors.generic")} onRetry={operations.retry} />;
	if (operations.isPending) return <LoadingState />;
	if (nodesQuery.isError || poolsQuery.isError) {
		return <ErrorState message={t("errors.generic")} onRetry={() => { void nodesQuery.refetch(); void poolsQuery.refetch(); }} />;
	}
	if (nodesQuery.isPending || poolsQuery.isPending) return <LoadingState />;

	const allNodes = nodesQuery.data?.items ?? [];
	const nodes = fixedTargetCandidates(allNodes);
	const pools = poolsQuery.data ?? [];
	const nodesById = new Map(allNodes.map((node) => [node.id, node]));
	const poolsById = new Map(pools.map((pool) => [pool.id, pool]));
	const eligibleNodeIds = new Set(nodes.map((node) => node.id));
	const candidateCount = allNodes.filter((node) => ["ready", "dynamic"].includes(nodeCondition(node, now))).length;
	const classOverrideCount = trafficClasses.filter((cls) => operations.form.classTargets[cls]?.mode).length;

	function summary(target: EgressRoutingTarget): TargetSummary {
		if (target.mode === "direct") return {
			name: t("networkRouting.direct"), detail: t("networkRouting.directDetail"),
			status: t("networkRouting.directStatus"), tone: "good",
		};
		if (target.mode === "node") {
			const node = nodesById.get(target.nodeId ?? "");
			if (!node || !eligibleNodeIds.has(node.id)) return {
				name: node?.name ?? t("networkRouting.unavailableResource", { id: target.nodeId ?? "—" }),
				detail: t("networkRouting.nodeUnavailable"), status: t("networkRouting.unavailable"), tone: "bad",
			};
			const condition = nodeCondition(node, now);
			return {
				name: node.name, status: t(`ops.condition.${condition}`),
				detail: t(node.rotatingEndpoint ? "networkRouting.dynamicNode" : "networkRouting.fixedNode"),
				tone: condition === "ready" || condition === "dynamic" ? "good" : condition === "unknown" ? "neutral" : "warn",
			};
		}
		if (target.mode === "pool") {
			const pool = poolsById.get(target.poolId ?? "");
			if (!pool) return {
				name: t("networkRouting.unavailableResource", { id: target.poolId ?? "—" }),
				detail: t("networkRouting.poolUnavailable"), status: t("networkRouting.unavailable"), tone: "bad",
			};
			const ready = pool.memberIds.filter((memberId) => {
				const node = nodesById.get(memberId);
				return node && ["ready", "dynamic"].includes(nodeCondition(node, now));
			}).length;
			return {
				name: pool.name,
				detail: pool.enabled ? t("networkRouting.poolCandidates", { count: ready, total: pool.memberCount }) : t("networkRouting.poolDisabledDetail"),
				status: t(pool.enabled ? "networkRouting.poolEnabled" : "networkRouting.disabled"),
				tone: pool.enabled ? (ready > 0 ? "good" : "warn") : "bad",
			};
		}
		return {
			name: t("networkRouting.auto"), detail: t("networkRouting.autoCandidates", { count: candidateCount }),
			status: t("networkRouting.snapshot"), tone: "neutral",
		};
	}

	// 回退说明仅保留代理池场景（回退链影响实际选路）；直连/节点/自动调度不再提示。
	function fallbackHint(target: EgressRoutingTarget): string | undefined {
		if (target.mode !== "pool") return undefined;
		const pool = poolsById.get(target.poolId ?? "");
		if (!pool?.enabled) return t("networkRouting.disabledPoolFallback");
		if (pool.fallbackMode === "direct") return t("networkRouting.directFallback");
		if (pool.fallbackMode === "pool") {
			const fallbackPool = poolsById.get(pool.fallbackPoolId ?? "");
			const name = fallbackPool ? fallbackPool.name + (fallbackPool.enabled ? "" : ` · ${t("networkRouting.disabled")}`) : t("networkRouting.unavailableResource", { id: pool.fallbackPoolId ?? "—" });
			return t("networkRouting.poolFallback", { name });
		}
		return undefined;
	}

	function effective(cls?: EgressTrafficClass, scope?: EgressRoutingScope) {
		return resolveEffectiveTarget(operations.form, cls, scope);
	}
	function decidingLabel(cls?: EgressTrafficClass, scope?: EgressRoutingScope) {
		if (cls && operations.form.classTargets[cls]?.mode) return t("networkRouting.fromClass");
		if (scope && operations.form.scopeTargets[scope]?.mode) return t("networkRouting.fromScope");
		return t(operations.form.defaultTarget.mode ? "networkRouting.fromDefault" : "networkRouting.fromAuto");
	}

	const rules: RouteRule[] = [
		{
			id: "default", label: t("networkRouting.default"), target: operations.form.defaultTarget,
			onChange: (target) => operations.update((current) => ({ ...current, defaultTarget: target ?? { mode: "auto" } })),
		},
		...routingScopes.map((scope): RouteRule => ({
			id: `scope:${scope}`, label: t(routingScopeLabelKeys[scope]), target: operations.form.scopeTargets[scope],
			inheritLabel: t("networkRouting.inheritDefault"),
			onChange: (target) => operations.update((current) => {
				const next = { ...current.scopeTargets };
				if (target) next[scope] = target;
				else delete next[scope];
				return { ...current, scopeTargets: next };
			}),
		})),
	];
	const classRules = trafficClasses.map((cls): RouteRule & { cls: EgressTrafficClass } => ({
		cls,
		id: `class:${cls}`, label: t(trafficClassLabelKeys[cls]), target: operations.form.classTargets[cls],
		inheritLabel: t("networkRouting.inheritScope"),
		onChange: (nextTarget) => operations.update((current) => {
			const next = { ...current.classTargets };
			if (nextTarget) next[cls] = nextTarget;
			else delete next[cls];
			return { ...current, classTargets: next };
		}),
	}));
	const classesMode = selected === "classes";
	const activeClassRule = classRules.find((rule) => rule.cls === selectedClass)!;
	const activeRule = classesMode
		? activeClassRule
		: (rules.find((rule) => rule.id === selected) ?? rules[0]!);
	const activeScope = routingScopes.find((item) => activeRule.id === `scope:${item}`);
	const target = effective(undefined, activeScope);
	const targetSummary = summary(target);
	const classConfigured = Boolean(activeClassRule.target?.mode);
	const activeFallback = fallbackHint(target);
	const classFallback =
		classConfigured && activeClassRule.target ? fallbackHint(activeClassRule.target) : undefined;

	function counters(rule: RouteRule) {
		const records = statsQuery.data?.items.filter((stat) => stat.level === rule.id) ?? [];
		const known = !statsQuery.isPending && !statsQuery.isError && Boolean(statsQuery.data);
		const hit = records.reduce((total, stat) => total + stat.hit, 0);
		const fallback = records.reduce((total, stat) => total + stat.fallback, 0);
		return (
			<section className="nroute-counters" aria-label={t("networkRouting.ruleCounters", { name: rule.label })}>
				<div className="nroute-counter"><span>{t("networkRouting.targetSelected")}</span><strong>{known ? hit.toLocaleString() : "—"}</strong></div>
				<div className="nroute-counter"><span>{t("networkRouting.fallbackRefusal")}</span><strong>{known ? fallback.toLocaleString() : "—"}</strong></div>
				<div className="nroute-counter-help">
					{rule.target?.mode === "auto" && <p>{t("networkRouting.autoCounters")}</p>}
					{statsQuery.isError && <NetworkButton size="sm" variant="ghost" onClick={() => void statsQuery.refetch()}>{t("networkRouting.retryCounters")}</NetworkButton>}
				</div>
			</section>
		);
	}

	return (
		<section className="nroute-page">
			<div className="nroute-board">
				<nav className="nroute-scope-picker" aria-label={t("networkRouting.chooseRule")}>
					{rules.map((rule) => {
						const ruleScope = routingScopes.find((item) => rule.id === `scope:${item}`);
						const routeSummary = summary(effective(undefined, ruleScope));
						return (
							<button key={rule.id} type="button" aria-pressed={selected === rule.id} aria-controls={`${id}-canvas`} onClick={() => setSelected(rule.id)}>
								<strong>{rule.label}</strong>
								<span>{rule.id === "default" ? t("networkRouting.defaultDescription") : !rule.target?.mode ? t("networkRouting.inheritDefault") : routeSummary.name}</span>
							</button>
						);
					})}
					<button type="button" aria-pressed={classesMode} aria-controls={`${id}-canvas`} onClick={() => setSelected("classes")}>
						<strong>{t("networkRouting.semanticGroup")}</strong>
						<span>{t("networkRouting.classOverrides", { count: classOverrideCount })}</span>
					</button>
				</nav>
				<article className="nroute-canvas" id={`${id}-canvas`} aria-label={classesMode ? t("networkRouting.semanticGroup") : activeRule.label}>
					{classesMode ? (
						<>
							<nav className="nroute-class-picker" aria-label={t("networkRouting.chooseClass")}>
								{trafficClasses.map((cls) => (
									<button
										type="button"
										key={cls}
										aria-pressed={selectedClass === cls}
										aria-controls={`${id}-class-editor`}
										onClick={() => setSelectedClass(cls)}
									>
										{operations.form.classTargets[cls]?.mode && (
											<span className="nroute-configured-dot" aria-label={t("networkRouting.configured")} />
										)}
										{t(trafficClassLabelKeys[cls])}
									</button>
								))}
							</nav>
							<div id={`${id}-class-editor`} className="nroute-class-editor">
								<TargetEditor key={activeClassRule.id} rule={activeClassRule} nodes={nodes} pools={pools} now={now} />
								<div className="nroute-class-results">
									{routingScopes.map((provider) => {
										const resolved = effective(selectedClass, provider);
										const result = summary(resolved);
										return <article key={provider} className="nroute-class-result">
											<div className="nroute-result-source">{t(routingScopeLabelKeys[provider])}<ArrowRight aria-hidden="true" /></div>
											<strong><NetworkText>{result.name}</NetworkText></strong>
											<StatusPill tone={result.tone}>{result.status}</StatusPill>
											<p>{decidingLabel(selectedClass, provider)}</p>
										</article>;
									})}
								</div>
								{classFallback && <p className="nroute-note">{classFallback}</p>}
								{counters(activeClassRule)}
							</div>
						</>
					) : (
						<>
							<div className="nroute-flow">
								<div className="nroute-flow-node"><GitBranch aria-hidden="true" /><strong>{activeRule.label}</strong><span>{t(activeRule.id === "default" ? "networkRouting.defaultTraffic" : "networkRouting.providerTraffic")}</span></div>
								<div className="nroute-flow-link" aria-hidden="true"><span /><ArrowRight /></div>
								<div className="nroute-flow-node nroute-flow-target" data-tone={targetSummary.tone}><TargetIcon target={target} /><strong><NetworkText>{targetSummary.name}</NetworkText></strong><span>{targetSummary.detail}</span></div>
							</div>
							{activeFallback && <p className="nroute-fallback">{activeFallback}</p>}
							<TargetEditor key={activeRule.id} rule={activeRule} nodes={nodes} pools={pools} now={now} />
							{counters(activeRule)}
						</>
					)}
				</article>
			</div>
		</section>
	);
}
