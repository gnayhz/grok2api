import { LayoutDashboard, Layers2, RefreshCw, Route, Server } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useLocation, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { NetworkButton as Button, NetworkNavigation } from "./network-ui";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent } from "@/components/ui/tabs";
import { NodesPanel } from "./nodes-panel";
import { EgressOperationsProvider } from "./operations-context";
import { useEgressOperations } from "./operations-shared";
import { PoolsPanel } from "./pools-panel";
import { RoutingPanel } from "./routing-panel";
import { NetworkOverview } from "./network-overview";
import { useOperationsNodes, useOperationsPools } from "@/features/operations/operations-queries";
import { OperationsError, StatusPill } from "@/features/operations/operations-ui";

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
	const queryClient = useQueryClient();
	const nodes = useOperationsNodes();
	const pools = useOperationsPools();
	const requested = location.hash.slice(1);
	const sourcesOpen = requested === "nodes/sources" || requested === "sources";
	const view = sourcesOpen
		? "nodes"
		: ["overview", "nodes", "pools", "routing"].includes(requested)
			? requested
			: "overview";
	const search = new URLSearchParams(location.search);
	const select = (next: string) => navigate({ pathname: "/proxies", hash: next });
	const refresh = () => {
		for (const key of [
			"egress-nodes",
			"egress-pools",
			"egress-sources",
			"egress-operations",
			"egress-runtime",
			"egress-routing-stats",
		]) {
			void queryClient.invalidateQueries({ queryKey: [key] });
		}
	};
	return (
		<div className="network-workspace">
			<header className="network-header">
				<h1>{t("ops.network")}</h1>
				<Button
					size="sm"
					variant="outline"
					onClick={refresh}
					disabled={nodes.isFetching || pools.isFetching}
				>
					<RefreshCw
						className={
							nodes.isFetching || pools.isFetching
								? "animate-spin motion-reduce:animate-none"
								: undefined
						}
					/>
					{t("network.refresh")}
				</Button>
			</header>
			{(nodes.isError || pools.isError || operations.isError) && (
				<OperationsError retry={refresh} />
			)}
			<Tabs className="network-tabs" activationMode="manual" value={view} onValueChange={select}>
				<div className="network-tabs-bar">
					<NetworkNavigation
						items={[
							{ value: "overview", icon: LayoutDashboard, label: t("network.overview") },
							{
								value: "nodes",
								icon: Server,
								label: t("network.resources"),
								count: nodes.data?.total,
							},
							{ value: "pools", icon: Layers2, label: t("ops.poolTab"), count: pools.data?.length },
							{ value: "routing", icon: Route, label: t("ops.routeTab") },
						]}
					/>
					<div className="network-tabs-status">
						<StatusPill tone={operations.isDirty ? "warn" : "good"}>
							{t(operations.isDirty ? "networkRouting.draftPreview" : "networkRouting.saved")}
						</StatusPill>
					</div>
				</div>
				<TabsContent value="overview">
					<NetworkOverview />
				</TabsContent>
				<TabsContent value="nodes">
					<NodesPanel
						sourcesOpen={sourcesOpen}
						onManageSources={() => navigate({ pathname: "/proxies", search: location.search, hash: "nodes/sources" })}
						onShowNodes={() => navigate({ pathname: "/proxies", search: location.search, hash: "nodes" })}
						initialCondition={search.get("condition") ?? "all"}
						focusKey={location.search}
					/>
				</TabsContent>
				<TabsContent value="pools">
					<PoolsPanel
						focusPoolId={search.get("pool") ?? undefined}
						onViewNodes={() => navigate("/proxies?condition=ready#nodes")}
					/>
				</TabsContent>
				<TabsContent value="routing">
					<RoutingPanel initialRule={search.get("rule") ?? undefined} />
				</TabsContent>
			</Tabs>
			{operations.isDirty && (
				<div className="network-savebar" role="status">
					<span>{t("ops.settingsDraft")}</span>
					<Button
						size="sm"
						variant="ghost"
						disabled={operations.savePending}
						onClick={operations.discard}
					>
						{t("proxies.discard")}
					</Button>
					<Button size="sm" disabled={operations.savePending} onClick={operations.save}>
						{operations.savePending && <Spinner />}
						{t("common.save")}
					</Button>
				</div>
			)}
		</div>
	);
}
