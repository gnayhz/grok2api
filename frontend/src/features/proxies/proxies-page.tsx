import { ArrowRight, Settings2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useLocation, useNavigate } from "react-router-dom";
import { OperationsButton as Button } from "@/features/operations/operations-ui";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent } from "@/components/ui/tabs";
import { NodesPanel } from "./nodes-panel";
import { EgressOperationsProvider } from "./operations-context";
import { useEgressOperations } from "./operations-shared";
import { resolveEffectiveTarget } from "./effective-target";
import { PoolsPanel } from "./pools-panel";
import { RoutingPanel } from "./routing-panel";
import {
	useOperationsNodes,
	useOperationsPools,
} from "@/features/operations/operations-queries";
import {
	MetricRail,
	OperationalMetric,
	OperationsHeader,
	OperationsTabs,
	StatusPill,
	OperationsError,
} from "@/features/operations/operations-ui";
import { networkSummary } from "@/features/operations/operations-data";
import { useNow } from "@/features/guard/quality-hooks";

export function ProxiesPage() {
	return (
		<EgressOperationsProvider>
			<NetworkWorkspace />
		</EgressOperationsProvider>
	);
}
function NetworkWorkspace() {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const location = useLocation();
	const navigate = useNavigate();
	const now = useNow(15000);
	const nodes = useOperationsNodes();
	const pools = useOperationsPools();
	const summary = networkSummary(nodes.data?.items ?? [], now);
	const [focus, setFocus] = useState({
		search: "",
		filter: "all",
		revision: 0,
	});
	const requested = location.hash.slice(1);
	const view = ["nodes", "pools", "routing"].includes(requested)
		? requested
		: "nodes";
	const select = (next: string) =>
		navigate({ pathname: "/proxies", hash: next });
	const locate = (search: string, filter = "all") => {
		setFocus((current) => ({ search, filter, revision: current.revision + 1 }));
		select("nodes");
	};
	const value = (n: number | string | null) =>
		nodes.isError || !nodes.data ? "—" : (n ?? "—");
	const destination = (scope: "grok_build" | "grok_web" | "grok_console") => {
		const target = resolveEffectiveTarget(operations.form, "inference", scope);
		if (target.mode === "direct") return t("ops.direct");
		if (target.mode === "auto") return t("ops.auto");
		if (target.mode === "node")
			return (
				nodes.data?.items.find((n) => n.id === target.nodeId)?.name ??
				t("ops.missingTarget")
			);
		return `${t("ops.poolTab")} · ${pools.data?.find((p) => p.id === target.poolId)?.name ?? t("ops.missingTarget")}`;
	};
	return (
		<div className="ops-workspace">
			<OperationsHeader
				title={t("ops.network")}
				description={t("ops.networkDescription")}
				status={
					<StatusPill
						tone={
							nodes.isError || !nodes.data
								? "neutral"
								: summary.attention
									? "warn"
									: "good"
						}
					>
						{nodes.isError || !nodes.data
							? t("ops.unknown")
							: summary.attention
								? `${summary.attention} · ${t("ops.attention")}`
								: t("ops.nodeLoaded", { count: summary.total })}
					</StatusPill>
				}
			/>

			{(nodes.isError || pools.isError || operations.isError) && (
				<div className="mb-4">
					<OperationsError
						retry={() => {
							void nodes.refetch();
							void pools.refetch();
							operations.retry();
						}}
					/>
				</div>
			)}

			<Tabs activationMode="manual" value={view} onValueChange={select}>
				<OperationsTabs
					items={[
						{
							value: "nodes",
							label: t("ops.nodeTab"),
							count: nodes.data?.total,
						},
						{
							value: "pools",
							label: t("ops.poolTab"),
							count: pools.data?.length,
						},
						{ value: "routing", label: t("ops.routeTab") },
					]}
					end={t("ops.freshness")}
				/>
				<TabsContent value="nodes" className="mt-0">
					<MetricRail>
						<OperationalMetric
							label={t("ops.availableNodes")}
							value={value(summary.ready)}
							detail={t("ops.availableHelp")}
							tone="good"
							onClick={() => locate("", "ready")}
						/>
						<OperationalMetric
							label={t("ops.attention")}
							value={value(summary.attention)}
							detail={t("ops.attentionHelp")}
							tone={summary.attention ? "warn" : undefined}
							onClick={() => locate("", "attention")}
						/>
						<OperationalMetric
							label={t("ops.unchecked")}
							value={value(summary.unknown)}
							detail={t("ops.uncheckedHelp")}
							onClick={() => locate("", "unknown")}
						/>
						<OperationalMetric
							label={t("ops.latency")}
							value={
								<>
									{value(summary.median)}
									{nodes.data && summary.median !== null && (
										<span className="ml-1 text-sm font-normal text-muted-foreground">
											ms
										</span>
									)}
								</>
							}
							detail={t("ops.latencyHelp")}
						/>
					</MetricRail>
					<div className="ops-route-strip">
						{(["grok_build", "grok_web", "grok_console"] as const).map(
							(scope) => (
								<div className="ops-route-path" key={scope}>
									<strong>{scope.replace("grok_", "").toUpperCase()}</strong>
									<ArrowRight className="size-3 text-muted-foreground" />
									<span className="truncate font-medium">
										{operations.isPending || operations.isError
											? t("ops.unknown")
											: destination(scope)}
									</span>
								</div>
							),
						)}
						<Button variant="ghost" size="sm" onClick={() => select("routing")}>
							<Settings2 className="size-3.5" />
							{t("ops.routeTab")}
						</Button>
					</div>

					<NodesPanel
						key={focus.revision}
						initialSearch={focus.search}
						initialCondition={focus.filter}
					/>
				</TabsContent>
				<TabsContent value="pools" className="mt-0">
					<PoolsPanel />
				</TabsContent>
				<TabsContent value="routing" className="mt-0">
					<RoutingPanel />
				</TabsContent>
			</Tabs>
			{operations.isDirty && (
				<div className="ops-save-dock">
					<span className="text-xs text-muted-foreground">
						{t("ops.settingsDraft")}
					</span>
					<Button
						size="sm"
						variant="ghost"
						disabled={operations.savePending}
						onClick={operations.discard}
					>
						{t("proxies.discard")}
					</Button>
					<Button
						size="sm"
						disabled={operations.savePending}
						onClick={operations.save}
					>
						{operations.savePending && <Spinner />}
						{t("common.save")}
					</Button>
				</div>
			)}
		</div>
	);
}
