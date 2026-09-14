import { useQueryClient } from "@tanstack/react-query";
import {
	Layers,
	Radio,
	Server,
	Workflow,
} from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useLocation, useNavigate } from "react-router-dom";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useOperationsNodes, useOperationsPools } from "@/features/operations/operations-queries";
import { OperationsError } from "@/features/operations/operations-ui";

import { EgressOperationsProvider } from "./operations-context";
import { useEgressOperations } from "./operations-shared";
import { ProxyCommandBar, ProxyFloatingSaveDock } from "./proxy-command-bar";
import { NodeAddOrImportDialog, ProxyNodesView } from "./proxy-nodes-view";
import { ProxyPoolsView } from "./proxy-pools-view";
import { ProxyRadarView } from "./proxy-radar-view";
import { ProxyRoutingView } from "./proxy-routing-view";

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
	const nodesQuery = useOperationsNodes();
	const poolsQuery = useOperationsPools();

	// Page-level state for Add/Import modal to ensure 100% working clicks from any view
	const [addModalOpen, setAddModalOpen] = useState(false);
	const [addModalTab, setAddModalTab] = useState<"manual" | "bulk" | "subscriptions">("manual");

	const requested = location.hash.slice(1);
	const sourcesOpen = requested === "nodes/sources" || requested === "sources";

	// Map requested hash to current view
	const view = sourcesOpen
		? "nodes"
		: requested === "overview" || requested === "radar"
			? "radar"
			: ["nodes", "pools", "routing"].includes(requested)
				? requested
				: "radar";

	const search = new URLSearchParams(location.search);

	// Keep visited tabs in DOM for instant 0ms switching without remounting lag
	const [visitedTabs, setVisitedTabs] = useState<Record<string, boolean>>(() => ({ [view]: true }));
	if (!visitedTabs[view]) {
		setVisitedTabs((prev) => ({ ...prev, [view]: true }));
	}

	const selectTab = (next: string) => {
		navigate({ pathname: "/proxies", search: location.search, hash: next });
	};

	const navigateWithQuery = (tab: "radar" | "nodes" | "pools" | "routing", queryStr?: string) => {
		navigate({
			pathname: "/proxies",
			search: queryStr ? `?${queryStr}` : "",
			hash: tab,
		});
	};

	const handleOpenAddModal = (tab: "manual" | "bulk" | "subscriptions" = "manual") => {
		setAddModalTab(tab);
		setAddModalOpen(true);
	};

	return (
		<div className="flex flex-col gap-5 min-w-0 pb-2">
			{/* Top Telemetry & Command Bar */}
			<ProxyCommandBar onOpenAddModal={handleOpenAddModal} />

			{/* Global Query Error Banner */}
			{(nodesQuery.isError || poolsQuery.isError || operations.isError) && (
				<OperationsError
					retry={() => {
						void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
						void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
						void queryClient.invalidateQueries({ queryKey: ["egress-operations"] });
					}}
				/>
			)}

			{/* Main Workspace Navigation Tabs */}
			<Tabs
				className="flex flex-col gap-4"
				activationMode="manual"
				value={view}
				onValueChange={selectTab}
			>
				{/* Modern Tab Bar */}
				<div className="flex items-center justify-between border-b border-border/80 pb-0">
					<TabsList className="h-11 bg-transparent p-0 gap-1 rounded-none border-b-0">
						{/* Tab 1: Radar & Topology */}
						<TabsTrigger
							value="radar"
							className="data-[state=active]:border-primary data-[state=active]:bg-primary/5 data-[state=active]:text-primary border-b-2 border-transparent rounded-none px-4 py-2.5 text-xs font-semibold gap-2 transition-all"
						>
							<Radio className="size-4" />
							<span>{t("network.viewRadar")}</span>
						</TabsTrigger>

						{/* Tab 2: Node Fleet Matrix */}
						<TabsTrigger
							value="nodes"
							className="data-[state=active]:border-primary data-[state=active]:bg-primary/5 data-[state=active]:text-primary border-b-2 border-transparent rounded-none px-4 py-2.5 text-xs font-semibold gap-2 transition-all"
						>
							<Server className="size-4" />
							<span>{t("network.viewNodes")}</span>
							{nodesQuery.data?.total !== undefined && (
								<span className="rounded-full bg-muted px-1.5 py-0.2 text-[10px] font-bold tabular-nums text-muted-foreground">
									{nodesQuery.data.total}
								</span>
							)}
						</TabsTrigger>

						{/* Tab 3: Proxy Pools Deck */}
						<TabsTrigger
							value="pools"
							className="data-[state=active]:border-primary data-[state=active]:bg-primary/5 data-[state=active]:text-primary border-b-2 border-transparent rounded-none px-4 py-2.5 text-xs font-semibold gap-2 transition-all"
						>
							<Layers className="size-4" />
							<span>{t("network.viewPools")}</span>
							{poolsQuery.data?.length !== undefined && (
								<span className="rounded-full bg-muted px-1.5 py-0.2 text-[10px] font-bold tabular-nums text-muted-foreground">
									{poolsQuery.data.length}
								</span>
							)}
						</TabsTrigger>

						{/* Tab 4: Smart Routing Matrix */}
						<TabsTrigger
							value="routing"
							className="data-[state=active]:border-primary data-[state=active]:bg-primary/5 data-[state=active]:text-primary border-b-2 border-transparent rounded-none px-4 py-2.5 text-xs font-semibold gap-2 transition-all"
						>
							<Workflow className="size-4" />
							<span>{t("network.viewRouting")}</span>
						</TabsTrigger>
					</TabsList>

					{/* Save State Indicator */}
					<div className="flex items-center gap-2 pr-1">
						{operations.isDirty ? (
							<span className="inline-flex items-center gap-1.5 rounded-full bg-amber-500/10 px-2.5 py-1 text-xs font-semibold text-amber-600 dark:text-amber-400 border border-amber-500/20 animate-pulse">
								<span className="size-1.5 rounded-full bg-amber-500" />
								<span>{t("networkRouting.draftPreview")}</span>
							</span>
						) : (
							<span className="inline-flex items-center gap-1.5 rounded-full bg-muted/60 px-2.5 py-1 text-xs font-medium text-muted-foreground">
								<span className="size-1.5 rounded-full bg-emerald-500" />
								<span>{t("networkRouting.saved")}</span>
							</span>
						)}
					</div>
				</div>

				{/* Tab 1: Traffic Radar & Topology */}
				{visitedTabs.radar && (
					<TabsContent forceMount value="radar" className="data-[state=inactive]:hidden focus-visible:outline-none mt-2">
						<ProxyRadarView onNavigateTab={navigateWithQuery} />
					</TabsContent>
				)}

				{/* Tab 2: Node Fleet Matrix */}
				{visitedTabs.nodes && (
					<TabsContent forceMount value="nodes" className="data-[state=inactive]:hidden focus-visible:outline-none mt-2">
						<ProxyNodesView
							initialCondition={search.get("condition") ?? "all"}
							onOpenAddModal={handleOpenAddModal}
							onViewPools={() => selectTab("pools")}
						/>
					</TabsContent>
				)}

				{/* Tab 3: Proxy Pools Deck */}
				{visitedTabs.pools && (
					<TabsContent forceMount value="pools" className="data-[state=inactive]:hidden focus-visible:outline-none mt-2">
						<ProxyPoolsView
							focusPoolId={search.get("pool") ?? undefined}
							onViewNodes={() => navigateWithQuery("nodes", "condition=ready")}
						/>
					</TabsContent>
				)}

				{/* Tab 4: Smart Routing Matrix */}
				{visitedTabs.routing && (
					<TabsContent forceMount value="routing" className="data-[state=inactive]:hidden focus-visible:outline-none mt-2">
						<ProxyRoutingView
							onViewPools={() => selectTab("pools")}
							onViewNodes={() => selectTab("nodes")}
						/>
					</TabsContent>
				)}
			</Tabs>

			{/* Global Add / Import Node Dialog (Accessible from any tab with instant response) */}
			<NodeAddOrImportDialog
				open={addModalOpen}
				activeTab={addModalTab}
				onOpenChange={setAddModalOpen}
				existingPools={poolsQuery.data ?? []}
				onSuccess={() => {
					void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
					void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
				}}
			/>

			{/* Floating Dock for Unsaved Changes */}
			<ProxyFloatingSaveDock />
		</div>
	);
}