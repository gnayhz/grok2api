import { useQueryClient } from "@tanstack/react-query";
import {
	Copy,
	Layers,
	LayoutGrid,
	MoreHorizontal,
	Pencil,
	Plus,
	Power,
	PowerOff,
	RefreshCw,
	Rss,
	Search,
	Server,
	ShieldAlert,
	Table as TableIcon,
	Trash2,
	Upload,
	X,
	Zap,
} from "lucide-react";
import { memo, useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { OperationsDialogContent, OperationsAlertDialogContent } from "@/features/operations/operations-ui";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import {
	Table,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { unbanQualityNode } from "@/features/guard/quality-api";
import { useNow } from "@/features/guard/quality-hooks";
import { nodeCondition } from "@/features/operations/operations-data";
import { useOperationsNodes, useOperationsPools } from "@/features/operations/operations-queries";
import {
	batchSetEgressRotation,
	createEgressNode,
	deleteEgressNode,
	deleteEgressNodes,
	getEgressNodeProxyURL,
	getEgressNodeRotationURL,
	importEgressText,
	rotateEgressNode,
	setEgressPoolMembers,
	testEgressNode,
	testEgressNodes,
	updateEgressNode,
	updateEgressNodesEnabled,
	type EgressNodeDTO,
} from "@/features/settings/settings-api";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { cn } from "@/shared/lib/cn";

import { formatTimeAgo, getLatencyTone, maskIP } from "./proxy-format";
import { SubscriptionsPanel } from "./subscriptions-panel";

type NodeFilterCondition =
	| "all"
	| "ready"
	| "dynamic"
	| "ipv6"
	| "cooling"
	| "unhealthy"
	| "restricted"
	| "disabled";

export function ProxyNodesView({
	initialCondition = "all",
	onOpenAddModal,
	onViewPools,
}: {
	initialCondition?: string;
	onOpenAddModal: (tab?: "manual" | "bulk" | "subscriptions") => void;
	onViewPools?: () => void;
}) {
	const { t, i18n } = useTranslation();
	const queryClient = useQueryClient();
	const nodesQuery = useOperationsNodes();
	const poolsQuery = useOperationsPools();
	const now = useNow(15_000);

	const [filterCondition, setFilterCondition] = useState<NodeFilterCondition>(
		(initialCondition as NodeFilterCondition) || "all"
	);
	const [search, setSearch] = useState("");
	const [viewMode, setViewMode] = useState<"cards" | "table">("cards");
	const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());

	// Edit & delete dialog states
	const [editNode, setEditNode] = useState<EgressNodeDTO | null>(null);
	const [deleteConfirmNode, setDeleteConfirmNode] = useState<EgressNodeDTO | null>(null);
	const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
	const [batchPoolOpen, setBatchPoolOpen] = useState(false);
	const [batchRotationOpen, setBatchRotationOpen] = useState(false);
	const [targetPoolId, setTargetPoolId] = useState<string>("");
	const [rotationTemplate, setRotationTemplate] = useState<string>("");

	// Single probe in-progress
	const [probingNodeId, setProbingNodeId] = useState<string | null>(null);

	const items = useMemo(() => nodesQuery.data?.items ?? [], [nodesQuery.data?.items]);
	const pools = useMemo(() => poolsQuery.data ?? [], [poolsQuery.data]);

	// Filter counts
	const counts = useMemo(() => {
		let ready = 0,
			dynamic = 0,
			ipv6 = 0,
			cooling = 0,
			unhealthy = 0,
			restricted = 0,
			disabled = 0;

		for (const node of items) {
			const c = nodeCondition(node, now);
			if (c === "ready") ready++;
			else if (c === "dynamic") dynamic++;
			else if (c === "cooling") cooling++;
			else if (c === "unhealthy") unhealthy++;
			else if (c === "held" || c === "banned") restricted++;
			else if (c === "disabled") disabled++;

			if (node.ipv6Probe && node.ipv6Probe.status === "healthy") {
				ipv6++;
			}
		}
		return { all: items.length, ready, dynamic, ipv6, cooling, unhealthy, restricted, disabled };
	}, [items, now]);

	// Filtered nodes
	const filteredNodes = useMemo(() => {
		let list = items;
		if (filterCondition !== "all") {
			list = list.filter((n) => {
				const c = nodeCondition(n, now);
				if (filterCondition === "ready") return c === "ready";
				if (filterCondition === "dynamic") return c === "dynamic";
				if (filterCondition === "ipv6") return n.ipv6Probe && n.ipv6Probe.status === "healthy";
				if (filterCondition === "cooling") return c === "cooling";
				if (filterCondition === "unhealthy") return c === "unhealthy";
				if (filterCondition === "restricted") return c === "held" || c === "banned";
				if (filterCondition === "disabled") return c === "disabled";
				return true;
			});
		}

		if (search.trim()) {
			const needle = search.trim().toLowerCase();
			list = list.filter(
				(n) =>
					n.name.toLowerCase().includes(needle) ||
					(n.exitIp && n.exitIp.toLowerCase().includes(needle)) ||
					(n.ipv4Probe?.exitIp && n.ipv4Probe.exitIp.toLowerCase().includes(needle)) ||
					(n.ipv6Probe?.exitIp && n.ipv6Probe.exitIp.toLowerCase().includes(needle)) ||
					(n.proxyDisplay && n.proxyDisplay.toLowerCase().includes(needle)) ||
					(n.sourceName && n.sourceName.toLowerCase().includes(needle)) ||
					(n.pools && n.pools.some((p) => p.name.toLowerCase().includes(needle)))
			);
		}
		return list;
	}, [items, filterCondition, search, now]);

	const refreshNodes = useCallback(() => {
		void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
		void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
	}, [queryClient]);

	// Selection
	const handleToggleSelect = useCallback((id: string) => {
		setSelectedIds((prev) => {
			const next = new Set(prev);
			if (next.has(id)) next.delete(id);
			else next.add(id);
			return next;
		});
	}, []);

	const handleSelectAll = useCallback(() => {
		if (selectedIds.size === filteredNodes.length) {
			setSelectedIds(new Set());
		} else {
			setSelectedIds(new Set(filteredNodes.map((n) => n.id)));
		}
	}, [selectedIds.size, filteredNodes]);

	// Actions
	const handleToggleSingleEnabled = useCallback(
		async (nodeId: string, enabled: boolean) => {
			try {
				await updateEgressNodesEnabled([nodeId], enabled);
				refreshNodes();
				toast.success(t(enabled ? "common.enabled" : "common.disabled"));
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.updateError"));
			}
		},
		[refreshNodes, t]
	);

	const handleTestSingle = useCallback(
		async (nodeId: string) => {
			setProbingNodeId(nodeId);
			try {
				const res = await testEgressNode(nodeId);
				refreshNodes();
				if (res.status === "healthy") {
					toast.success(
						t("network.probeSuccessDetail", {
							latency: res.latencyMs,
							ip: res.exitIp || "OK",
						})
					);
				} else {
					toast.error(t("network.probeFailedDetail", { error: res.error || "Failed" }));
				}
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.probeTestError"));
			} finally {
				setProbingNodeId(null);
			}
		},
		[refreshNodes, t]
	);

	const handleRotateSingle = useCallback(
		async (nodeId: string) => {
			try {
				await rotateEgressNode(nodeId);
				refreshNodes();
				toast.success(t("network.rotateSuccess"));
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.rotateError"));
			}
		},
		[refreshNodes, t]
	);

	const handleRevealProxyURL = useCallback(
		async (nodeId: string) => {
			try {
				const { proxyURL } = await getEgressNodeProxyURL(nodeId);
				await navigator.clipboard.writeText(proxyURL);
				toast.success(t("network.copied") + `: ${proxyURL}`);
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.revealError"));
			}
		},
		[t]
	);

	const handleRevealRotationURL = useCallback(
		async (nodeId: string) => {
			try {
				const { rotationURL } = await getEgressNodeRotationURL(nodeId);
				await navigator.clipboard.writeText(rotationURL);
				toast.success(t("network.copied") + `: ${rotationURL}`);
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.revealError"));
			}
		},
		[t]
	);

	const handleUnbanNode = useCallback(
		async (node: EgressNodeDTO) => {
			try {
				await unbanQualityNode(Number(node.id));
				refreshNodes();
				toast.success(t("network.unbanSuccess"));
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.unbanError"));
			}
		},
		[refreshNodes, t]
	);

	// Batch actions
	const handleBatchProbe = async () => {
		const ids = Array.from(selectedIds);
		if (!ids.length) return;
		try {
			const res = await testEgressNodes(ids);
			refreshNodes();
			toast.success(
				t("network.testAllDone", { healthy: res.healthy, unhealthy: res.unhealthy })
			);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.batchTestError"));
		}
	};

	const handleBatchToggleEnabled = async (enabled: boolean) => {
		const ids = Array.from(selectedIds);
		if (!ids.length) return;
		try {
			const res = await updateEgressNodesEnabled(ids, enabled);
			refreshNodes();
			toast.success(t("network.batchUpdatedCount", { count: res.updated }));
			setSelectedIds(new Set());
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.updateError"));
		}
	};

	const handleBatchDelete = async () => {
		const ids = Array.from(selectedIds);
		if (!ids.length) return;
		try {
			const res = await deleteEgressNodes(ids);
			refreshNodes();
			toast.success(t("network.batchDeletedCount", { count: res.deleted }));
			setSelectedIds(new Set());
			setBatchDeleteOpen(false);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.deleteError"));
		}
	};

	const handleBatchAssignPool = async () => {
		if (!targetPoolId) return;
		const pool = pools.find((p) => p.id === targetPoolId);
		if (!pool) return;
		const combined = Array.from(new Set([...pool.memberIds, ...Array.from(selectedIds)]));
		try {
			await setEgressPoolMembers(targetPoolId, combined);
			refreshNodes();
			toast.success(t("network.batchAddedToPool", { name: pool.name }));
			setBatchPoolOpen(false);
			setSelectedIds(new Set());
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.assignPoolError"));
		}
	};

	const handleBatchSetRotation = async () => {
		const ids = Array.from(selectedIds);
		if (!ids.length) return;
		try {
			const res = await batchSetEgressRotation(ids, rotationTemplate);
			refreshNodes();
			toast.success(t("network.batchUpdatedCount", { count: res.updated }));
			setBatchRotationOpen(false);
			setSelectedIds(new Set());
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.rotationTemplateError"));
		}
	};

	return (
		<div className="flex flex-col gap-4">
			{/* Filter Pills, Search & View Controls */}
			<div className="flex flex-col gap-3 xl:flex-row xl:items-center xl:justify-between">
				{/* Status and Dual-Stack Filter Pills (Non-wrapping with subtle grouping) */}
				<div className="flex items-center gap-1.5 overflow-x-auto whitespace-nowrap scrollbar-none py-0.5">
					{(
						[
							{ id: "all", label: t("network.filterNodeAll"), title: t("networkResources.filter.all"), count: counts.all, dot: "" },
							{ id: "ready", label: t("network.filterNodeReady"), title: t("networkResources.filter.ready"), count: counts.ready, dot: "bg-emerald-500" },
							{ id: "dynamic", label: t("network.filterNodeDynamic"), title: t("network.dynamicTunnel"), count: counts.dynamic, dot: "bg-blue-500" },
							{ id: "ipv6", label: t("network.filterNodeIPv6"), title: t("network.ipv6Supported"), count: counts.ipv6, dot: "bg-cyan-500" },
						] as const
					).map((item) => (
						<button
							type="button"
							key={item.id}
							title={item.title}
							onClick={() => setFilterCondition(item.id)}
							className={cn(
								"flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-all cursor-pointer",
								filterCondition === item.id
									? "bg-primary text-primary-foreground shadow-sm"
									: "bg-card/70 border border-border/80 text-muted-foreground hover:border-border hover:text-foreground"
							)}
						>
							{item.dot && (
								<span
									className={cn(
										"size-1.5 rounded-full",
										item.dot,
										filterCondition === item.id && "ring-2 ring-primary-foreground/30"
									)}
								/>
							)}
							<span>{item.label}</span>
							<span
								className={cn(
									"rounded px-1 text-[10px] tabular-nums font-mono",
									filterCondition === item.id
										? "bg-primary-foreground/20 text-primary-foreground"
										: "bg-muted text-muted-foreground"
								)}
							>
								{item.count}
							</span>
						</button>
					))}

					<div className="h-4 w-px bg-border/70 mx-0.5 shrink-0" />

					{(
						[
							{ id: "cooling", label: t("network.filterNodeCooling"), title: t("network.cooling"), count: counts.cooling, dot: "bg-amber-500" },
							{ id: "unhealthy", label: t("network.filterNodeUnhealthy"), title: t("network.unhealthy"), count: counts.unhealthy, dot: "bg-rose-500" },
							{ id: "restricted", label: t("network.filterNodeRestricted"), title: t("networkResources.restricted"), count: counts.restricted, dot: "bg-purple-500" },
							{ id: "disabled", label: t("network.filterNodeDisabled"), title: t("networkResources.filter.disabled"), count: counts.disabled, dot: "bg-zinc-500" },
						] as const
					).map((item) => (
						<button
							type="button"
							key={item.id}
							title={item.title}
							onClick={() => setFilterCondition(item.id)}
							className={cn(
								"flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-all cursor-pointer",
								filterCondition === item.id
									? "bg-primary text-primary-foreground shadow-sm"
									: "bg-card/70 border border-border/80 text-muted-foreground hover:border-border hover:text-foreground"
							)}
						>
							{item.dot && (
								<span
									className={cn(
										"size-1.5 rounded-full",
										item.dot,
										filterCondition === item.id && "ring-2 ring-primary-foreground/30"
									)}
								/>
							)}
							<span>{item.label}</span>
							<span
								className={cn(
									"rounded px-1 text-[10px] tabular-nums font-mono",
									filterCondition === item.id
										? "bg-primary-foreground/20 text-primary-foreground"
										: "bg-muted text-muted-foreground"
								)}
							>
								{item.count}
							</span>
						</button>
					))}
				</div>

				{/* Search, View Mode, and Subscriptions Trigger */}				<div className="flex items-center gap-2 shrink-0">
					<div className="relative min-w-[220px]">
						<Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
						<Input
							placeholder={t("networkResources.searchNodes")}
							className="h-8 pl-8 pr-7 text-xs"
							value={search}
							onChange={(e) => setSearch(e.target.value)}
						/>
						{search && (
							<button
								type="button"
								onClick={() => setSearch("")}
								className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
							>
								<X className="size-3" />
							</button>
						)}
					</div>

					<div className="flex items-center rounded-lg border border-border/80 bg-card/60 p-0.5">
						<Tooltip>
							<TooltipTrigger asChild>
								<Button
									variant={viewMode === "cards" ? "secondary" : "ghost"}
									size="icon"
									className="size-7"
									onClick={() => setViewMode("cards")}
								>
									<LayoutGrid className="size-3.5" />
								</Button>
							</TooltipTrigger>
							<TooltipContent>{t("network.viewCard")}</TooltipContent>
						</Tooltip>

						<Tooltip>
							<TooltipTrigger asChild>
								<Button
									variant={viewMode === "table" ? "secondary" : "ghost"}
									size="icon"
									className="size-7"
									onClick={() => setViewMode("table")}
								>
									<TableIcon className="size-3.5" />
								</Button>
							</TooltipTrigger>
							<TooltipContent>{t("network.viewTable")}</TooltipContent>
						</Tooltip>
					</div>

					<Button
						size="sm"
						variant="outline"
						className="h-8 gap-1 text-xs"
						onClick={() => onOpenAddModal("subscriptions")}
					>
						<Rss className="size-3.5 text-orange-500" />
						<span>{t("network.addSubscription")}</span>
					</Button>
				</div>
			</div>

			{/* Batch Operations Bar */}
			{selectedIds.size > 0 && (
				<div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-primary/30 bg-primary/5 px-4 py-2.5 shadow-sm">
					<div className="flex items-center gap-2">
						<Checkbox
							checked={selectedIds.size === filteredNodes.length && filteredNodes.length > 0}
							onCheckedChange={handleSelectAll}
						/>
						<span className="text-xs font-semibold text-primary">
							{t("network.batchActions", { count: selectedIds.size })}
						</span>
					</div>

					<div className="flex flex-wrap items-center gap-1.5">
						<Button size="sm" variant="outline" className="h-7 text-xs gap-1" onClick={handleBatchProbe}>
							<Zap className="size-3 text-amber-500" />
							{t("network.batchProbe")}
						</Button>

						<Button size="sm" variant="outline" className="h-7 text-xs gap-1" onClick={() => handleBatchToggleEnabled(true)}>
							<Power className="size-3 text-emerald-500" />
							{t("network.batchEnable")}
						</Button>

						<Button size="sm" variant="outline" className="h-7 text-xs gap-1" onClick={() => handleBatchToggleEnabled(false)}>
							<PowerOff className="size-3 text-zinc-500" />
							{t("network.batchDisable")}
						</Button>

						<Button size="sm" variant="outline" className="h-7 text-xs gap-1" onClick={() => setBatchPoolOpen(true)}>
							<Layers className="size-3 text-blue-500" />
							{t("network.batchSetPool")}
						</Button>

						<Button size="sm" variant="outline" className="h-7 text-xs gap-1" onClick={() => setBatchRotationOpen(true)}>
							<RefreshCw className="size-3 text-purple-500" />
							{t("network.batchRotation")}
						</Button>

						<Button size="sm" variant="destructive" className="h-7 text-xs gap-1" onClick={() => setBatchDeleteOpen(true)}>
							<Trash2 className="size-3" />
							{t("network.batchDelete")}
						</Button>

						<Button size="sm" variant="ghost" className="h-7 text-xs text-muted-foreground" onClick={() => setSelectedIds(new Set())}>
							<X className="size-3 mr-1" />
							{t("common.cancel")}
						</Button>
					</div>
				</div>
			)}

			{/* Nodes Presentation: Card Matrix vs Virtualized Table */}
			{filteredNodes.length === 0 ? (
				<div className="flex h-48 flex-col items-center justify-center rounded-xl border border-dashed border-border/80 bg-card/40 p-6 text-center">
					<Server className="size-8 text-muted-foreground/40 mb-2" />
					<p className="text-sm font-semibold text-foreground">
						{search ? t("networkRouting.noResources") : t("network.emptyTitle")}
					</p>
					<p className="text-xs text-muted-foreground mt-1 max-w-sm">
						{search ? t("networkResources.searchNodes") : t("network.emptyDescription")}
					</p>
					{!search && (
						<Button
							size="sm"
							className="mt-3 gap-1.5"
							onClick={() => onOpenAddModal("manual")}
						>
							<Plus className="size-3.5" />
							{t("network.addNodeOrImport")}
						</Button>
					)}
				</div>
			) : viewMode === "cards" ? (
				/* Card Matrix View */
				<div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
					{filteredNodes.map((node) => (
						<ProxyNodeCardItem
							key={node.id}
							node={node}
							cond={nodeCondition(node, now)}
							isSelected={selectedIds.has(node.id)}
							isProbing={probingNodeId === node.id}
							locale={i18n.language}
							onToggleSelect={handleToggleSelect}
							onToggleEnabled={handleToggleSingleEnabled}
							onTestProbe={handleTestSingle}
							onRotate={handleRotateSingle}
							onRevealProxyURL={handleRevealProxyURL}
							onRevealRotationURL={handleRevealRotationURL}
							onEdit={setEditNode}
							onDelete={setDeleteConfirmNode}
							onUnban={handleUnbanNode}
							onViewPools={onViewPools}
						/>
					))}
				</div>
			) : (
				/* High-Performance Virtualized Table */
				<div className="overflow-hidden rounded-xl border border-border/80 bg-card/60 shadow-sm" data-slot="table-scroll-container">
					<Table>
						<TableHeader>
							<TableRow className="hover:bg-transparent">
								<TableHead className="w-10">
									<Checkbox
										checked={selectedIds.size === filteredNodes.length && filteredNodes.length > 0}
										onCheckedChange={handleSelectAll}
									/>
								</TableHead>
								<TableHead>{t("networkResources.exitSource")}</TableHead>
								<TableHead>{t("network.ipv4Egress")}</TableHead>
								<TableHead>{t("network.ipv6Egress")}</TableHead>
								<TableHead>{t("networkResources.poolMembership")}</TableHead>
								<TableHead className="text-right">{t("common.actions")}</TableHead>
							</TableRow>
						</TableHeader>
						<VirtualTableBody
							items={filteredNodes}
							colSpan={6}
							rowHeight={56}
							renderRow={(node) => {
								const cond = nodeCondition(node, now);
								const isSelected = selectedIds.has(node.id);
								const isProbing = probingNodeId === node.id;
								const ipv4 = node.ipv4Probe;
								const ipv6 = node.ipv6Probe;

								return (
									<TableRow
										key={node.id}
										className={cn(
											"h-[56px] transition-colors",
											isSelected && "bg-primary/5 hover:bg-primary/10"
										)}
									>
										<TableCell>
											<Checkbox
												checked={isSelected}
												onCheckedChange={() => handleToggleSelect(node.id)}
											/>
										</TableCell>
										<TableCell>
											<div className="flex items-center gap-2">
												<span
													className={cn(
														"size-2 shrink-0 rounded-full",
														cond === "ready"
															? "bg-emerald-500 animate-pulse"
															: cond === "dynamic"
																? "bg-blue-500"
																: cond === "cooling"
																	? "bg-amber-500"
																	: cond === "unhealthy"
																		? "bg-rose-500"
																		: cond === "held" || cond === "banned"
																			? "bg-purple-500"
																			: "bg-zinc-400"
													)}
												/>
												<div className="min-w-0">
													<div className="flex items-center gap-1.5">
														<span className="truncate text-xs font-semibold text-foreground">
															{node.name}
														</span>
														{node.rotatingEndpoint ? (
															<Badge variant="outline" className="text-[9px] px-1 py-0 border-blue-500/40 text-blue-500 shrink-0">
																{t("network.dynamicTunnel")}
															</Badge>
														) : node.rotationConfigured && node.rotationEnabled ? (
															<Badge variant="outline" className="text-[9px] px-1 py-0 border-purple-500/40 text-purple-600 dark:text-purple-400 shrink-0">
																{t("network.rotateWebhookLabel")}
															</Badge>
														) : (
															<Badge variant="outline" className="text-[9px] px-1 py-0 border-zinc-500/40 text-zinc-600 dark:text-zinc-400 shrink-0">
																{t("network.fixedExitBadge")}
															</Badge>
														)}
														{node.accountBoundProxy && (
															<Badge variant="outline" className="text-[9px] px-1 py-0 border-purple-500/40 text-purple-500">
																{t("network.accountBound")}
															</Badge>
														)}
													</div>
													{node.sourceName && (
														<span className="text-[10px] text-muted-foreground truncate block">
															{t("network.subSource", { name: node.sourceName })}
														</span>
													)}
												</div>
											</div>
										</TableCell>

										{/* IPv4 Column */}
										<TableCell>
											<div className="flex items-center gap-1.5 font-mono text-xs">
												<Badge variant="outline" className="text-[9px] px-1 py-0 border-emerald-500/30 text-emerald-600 dark:text-emerald-400">
													v4
												</Badge>
												<span className="truncate max-w-[120px]">
													{ipv4?.exitIp ? maskIP(ipv4.exitIp) : node.exitIp ? maskIP(node.exitIp) : "--"}
												</span>
												{isProbing ? (
													<Spinner className="size-3" />
												) : ipv4?.latencyMs ? (
													<span className="text-[10px] text-muted-foreground tabular-nums">
														({ipv4.latencyMs}ms)
													</span>
												) : null}
											</div>
										</TableCell>

										{/* IPv6 Column */}
										<TableCell>
											<div className="flex items-center gap-1.5 font-mono text-xs">
												<Badge
													variant="outline"
													className={cn(
														"text-[9px] px-1 py-0",
														ipv6?.status === "healthy"
															? "border-cyan-500/30 text-cyan-600 dark:text-cyan-400"
															: "border-muted text-muted-foreground"
													)}
												>
													v6
												</Badge>
												<span className="truncate max-w-[140px] text-muted-foreground">
													{ipv6?.exitIp ? maskIP(ipv6.exitIp) : t("network.ipv6None")}
												</span>
												{ipv6?.latencyMs ? (
													<span className="text-[10px] text-muted-foreground tabular-nums">
														({ipv6.latencyMs}ms)
													</span>
												) : null}
											</div>
										</TableCell>

										{/* Pool Membership */}
										<TableCell>
											<div className="flex flex-wrap gap-1 max-w-[180px]">
												{node.pools && node.pools.length > 0 ? (
													node.pools.map((p) => (
														<Badge
															key={p.id}
															variant="secondary"
															className="text-[10px] px-1.5 py-0 cursor-pointer hover:bg-accent"
															onClick={onViewPools}
														>
															{p.name}
														</Badge>
													))
												) : (
													<span className="text-xs text-muted-foreground/60">
														{t("networkResources.noPool")}
													</span>
												)}
											</div>
										</TableCell>

										{/* Actions */}
										<TableCell className="text-right">
											<DropdownMenu>
												<DropdownMenuTrigger asChild>
													<Button size="icon" variant="ghost" className="size-7">
														<MoreHorizontal className="size-3.5" />
													</Button>
												</DropdownMenuTrigger>
												<DropdownMenuContent align="end" className="w-44">
													<DropdownMenuItem onClick={() => handleTestSingle(node.id)}>
														<Zap className="mr-2 size-3.5 text-amber-500" />
														<span>{t("network.testLatency")}</span>
													</DropdownMenuItem>
													<DropdownMenuItem onClick={() => handleToggleSingleEnabled(node.id, !node.enabled)}>
														{node.enabled ? (
															<PowerOff className="mr-2 size-3.5 text-zinc-500" />
														) : (
															<Power className="mr-2 size-3.5 text-emerald-500" />
														)}
														<span>{node.enabled ? t("common.disable") : t("common.enable")}</span>
													</DropdownMenuItem>
													{node.rotationConfigured && node.rotationEnabled && (
														<DropdownMenuItem onClick={() => handleRotateSingle(node.id)}>
															<RefreshCw className="mr-2 size-3.5 text-blue-500" />
															<span>{t("network.rotateAction")}</span>
														</DropdownMenuItem>
													)}
													{node.proxyConfigured && (
														<DropdownMenuItem onClick={() => handleRevealProxyURL(node.id)}>
															<Copy className="mr-2 size-3.5" />
															<span>{t("network.copyProxyUrl")}</span>
														</DropdownMenuItem>
													)}
													{node.rotationConfigured && node.rotationEnabled && (
														<DropdownMenuItem onClick={() => handleRevealRotationURL(node.id)}>
															<Copy className="mr-2 size-3.5 text-purple-500" />
															<span>{t("network.copyRotationUrl")}</span>
														</DropdownMenuItem>
													)}
													{node.quality?.state && (
														<DropdownMenuItem onClick={() => handleUnbanNode(node)}>
															<ShieldAlert className="mr-2 size-3.5 text-purple-500" />
															<span>{t("network.releaseUnban")}</span>
														</DropdownMenuItem>
													)}
													<DropdownMenuSeparator />
													<DropdownMenuItem onClick={() => setEditNode(node)}>
														<Pencil className="mr-2 size-3.5" />
														<span>{t("common.edit")}</span>
													</DropdownMenuItem>
													<DropdownMenuItem
														onClick={() => setDeleteConfirmNode(node)}
														className="text-destructive focus:text-destructive"
													>
														<Trash2 className="mr-2 size-3.5" />
														<span>{t("common.delete")}</span>
													</DropdownMenuItem>
												</DropdownMenuContent>
											</DropdownMenu>
										</TableCell>
									</TableRow>
								);
							}}
						/>
					</Table>
				</div>
			)}

			{/* Edit Node Modal */}
			{editNode && (
				<NodeEditDialog
					key={editNode.id}
					node={editNode}
					open={Boolean(editNode)}
					onOpenChange={(open) => !open && setEditNode(null)}
					onSuccess={refreshNodes}
				/>
			)}

			{/* Single Delete Confirmation Dialog */}
			<AlertDialog
				open={Boolean(deleteConfirmNode)}
				onOpenChange={(open) => !open && setDeleteConfirmNode(null)}
			>
				<OperationsAlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>{t("common.delete")}</AlertDialogTitle>
						<AlertDialogDescription>
							{t("network.batchDeleteConfirm", { count: 1 })}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
							onClick={async () => {
								if (deleteConfirmNode) {
									await deleteEgressNode(deleteConfirmNode.id);
									refreshNodes();
									setDeleteConfirmNode(null);
									toast.success(t("network.nodeDeleted"));
								}
							}}
						>
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</OperationsAlertDialogContent>
			</AlertDialog>

			{/* Batch Delete Confirmation Dialog */}
			<AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
				<OperationsAlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>{t("network.batchDelete")}</AlertDialogTitle>
						<AlertDialogDescription>
							{t("network.batchDeleteConfirm", { count: selectedIds.size })}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
							onClick={handleBatchDelete}
						>
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</OperationsAlertDialogContent>
			</AlertDialog>

			{/* Batch Assign to Pool Dialog */}
			<Dialog open={batchPoolOpen} onOpenChange={setBatchPoolOpen}>
				<OperationsDialogContent className="max-w-md">
					<DialogHeader>
						<DialogTitle className="flex items-center gap-2">
							<Layers className="size-5 text-primary" />
							{t("network.batchSetPool")}
						</DialogTitle>
					</DialogHeader>
					<div className="space-y-3 py-2">
						<Label className="text-xs">{t("ops.poolTab")}</Label>
						<Select value={targetPoolId} onValueChange={setTargetPoolId}>
							<SelectTrigger className="w-full">
								<SelectValue placeholder={t("network.selectTargetPool")} />
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
					<DialogFooter>
						<Button variant="outline" size="sm" onClick={() => setBatchPoolOpen(false)}>
							{t("common.cancel")}
						</Button>
						<Button size="sm" disabled={!targetPoolId} onClick={handleBatchAssignPool}>
							{t("common.save")}
						</Button>
					</DialogFooter>
				</OperationsDialogContent>
			</Dialog>

			{/* Batch Rotation Template Dialog */}
			<Dialog open={batchRotationOpen} onOpenChange={setBatchRotationOpen}>
				<OperationsDialogContent className="max-w-md">
					<DialogHeader>
						<DialogTitle className="flex items-center gap-2">
							<RefreshCw className="size-5 text-primary" />
							{t("network.batchRotation")}
						</DialogTitle>
					</DialogHeader>
					<div className="space-y-3 py-2">
						<Label className="text-xs">{t("network.rotationTemplateLabel")}</Label>
						<Input
							placeholder="https://api.myproxy.com/rotate?key=xxx&node={name}"
							value={rotationTemplate}
							onChange={(e) => setRotationTemplate(e.target.value)}
						/>
						<p className="text-[11px] text-muted-foreground">
							{t("network.rotationVariablesHint")}
						</p>
					</div>
					<DialogFooter>
						<Button variant="outline" size="sm" onClick={() => setBatchRotationOpen(false)}>
							{t("common.cancel")}
						</Button>
						<Button size="sm" disabled={!rotationTemplate} onClick={handleBatchSetRotation}>
							{t("common.save")}
						</Button>
					</DialogFooter>
				</OperationsDialogContent>
			</Dialog>
		</div>
	);
}

/**
 * Proxy Node Card with Dual-Stack IPv4 / IPv6 Display
 */
const ProxyNodeCardItem = memo(function ProxyNodeCardItem({
	node,
	cond,
	isSelected,
	isProbing,
	locale,
	onToggleSelect,
	onToggleEnabled,
	onTestProbe,
	onRotate,
	onRevealProxyURL,
	onRevealRotationURL,
	onEdit,
	onDelete,
	onUnban,
	onViewPools,
}: {
	node: EgressNodeDTO;
	cond: string;
	isSelected: boolean;
	isProbing: boolean;
	locale: string;
	onToggleSelect: (id: string) => void;
	onToggleEnabled: (id: string, enabled: boolean) => void;
	onTestProbe: (id: string) => void;
	onRotate: (id: string) => void;
	onRevealProxyURL: (id: string) => void;
	onRevealRotationURL: (id: string) => void;
	onEdit: (node: EgressNodeDTO) => void;
	onDelete: (node: EgressNodeDTO) => void;
	onUnban: (node: EgressNodeDTO) => void;
	onViewPools?: () => void;
}) {
	const { t } = useTranslation();

	const beaconTone =
		cond === "ready"
			? "bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.5)]"
			: cond === "dynamic"
				? "bg-blue-500 shadow-[0_0_8px_rgba(59,130,246,0.5)]"
				: cond === "cooling"
					? "bg-amber-500"
					: cond === "unhealthy"
						? "bg-rose-500"
						: cond === "held" || cond === "banned"
							? "bg-purple-500"
							: "bg-zinc-400";

	const ipv4 = node.ipv4Probe;
	const ipv6 = node.ipv6Probe;
	const latencyTone = getLatencyTone(node.probeLatencyMs);

	return (
		<div
			className={cn(
				"group relative flex flex-col justify-between rounded-xl border bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md h-full",
				isSelected ? "border-primary ring-1 ring-primary/30" : "border-border/80"
			)}
		>
			<div>
				{/* Top bar: Select checkbox, Name, Switch, and Menu */}
				<div className="flex items-start justify-between gap-2">
					<div className="flex items-center gap-2 min-w-0">
						<Checkbox checked={isSelected} onCheckedChange={() => onToggleSelect(node.id)} />
						<span className={cn("size-2 shrink-0 rounded-full", beaconTone)} />
						<span className="truncate text-sm font-bold text-foreground" title={node.name}>
							{node.name}
						</span>
					</div>

					<div className="flex items-center gap-1 shrink-0">
						<Switch
							checked={node.enabled}
							onCheckedChange={(val) => onToggleEnabled(node.id, val)}
							className="scale-75 origin-right"
							title={node.enabled ? t("common.disable") : t("common.enable")}
						/>
						<DropdownMenu>
							<DropdownMenuTrigger asChild>
								<Button size="icon" variant="ghost" className="size-7 opacity-70 group-hover:opacity-100">
									<MoreHorizontal className="size-3.5" />
								</Button>
							</DropdownMenuTrigger>
							<DropdownMenuContent align="end" className="w-44">
								<DropdownMenuItem onClick={() => onTestProbe(node.id)}>
									<Zap className="mr-2 size-3.5 text-amber-500" />
									<span>{t("network.testLatency")}</span>
								</DropdownMenuItem>
								<DropdownMenuItem onClick={() => onToggleEnabled(node.id, !node.enabled)}>
									{node.enabled ? (
										<PowerOff className="mr-2 size-3.5 text-zinc-500" />
									) : (
										<Power className="mr-2 size-3.5 text-emerald-500" />
									)}
									<span>{node.enabled ? t("common.disable") : t("common.enable")}</span>
								</DropdownMenuItem>
							{node.rotationConfigured && node.rotationEnabled && (
								<DropdownMenuItem onClick={() => onRotate(node.id)}>
									<RefreshCw className="mr-2 size-3.5 text-blue-500" />
									<span>{t("network.rotateAction")}</span>
								</DropdownMenuItem>
							)}
							{node.proxyConfigured && (
								<DropdownMenuItem onClick={() => onRevealProxyURL(node.id)}>
									<Copy className="mr-2 size-3.5" />
									<span>{t("network.copyProxyUrl")}</span>
								</DropdownMenuItem>
							)}
							{node.rotationConfigured && node.rotationEnabled && (
								<DropdownMenuItem onClick={() => onRevealRotationURL(node.id)}>
									<Copy className="mr-2 size-3.5 text-purple-500" />
									<span>{t("network.copyRotationUrl")}</span>
								</DropdownMenuItem>
							)}
							{node.quality?.state && (
								<DropdownMenuItem onClick={() => onUnban(node)}>
									<ShieldAlert className="mr-2 size-3.5 text-purple-500" />
									<span>{t("network.releaseUnban")}</span>
								</DropdownMenuItem>
							)}
							<DropdownMenuSeparator />
							<DropdownMenuItem onClick={() => onEdit(node)}>
								<Pencil className="mr-2 size-3.5" />
								<span>{t("common.edit")}</span>
							</DropdownMenuItem>
							<DropdownMenuItem onClick={() => onDelete(node)} className="text-destructive focus:text-destructive">
								<Trash2 className="mr-2 size-3.5" />
								<span>{t("common.delete")}</span>
							</DropdownMenuItem>
						</DropdownMenuContent>
					</DropdownMenu>
					</div>
				</div>

				{/* Feature Badges */}
				<div className="mt-2 flex flex-wrap items-center gap-1.5 min-h-[22px]">
					{node.rotatingEndpoint ? (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-blue-500/30 text-blue-600 dark:text-blue-400 bg-blue-500/5 shrink-0">
							{t("network.dynamicTunnel")}
						</Badge>
					) : node.rotationConfigured && node.rotationEnabled ? (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-purple-500/30 text-purple-600 dark:text-purple-400 bg-purple-500/5 shrink-0">
							{t("network.rotateWebhookLabel")}
						</Badge>
					) : (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-zinc-500/30 text-zinc-600 dark:text-zinc-400 bg-zinc-500/5 shrink-0">
							{t("network.fixedExitBadge")}
						</Badge>
					)}
					{node.accountBoundProxy && (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-purple-500/30 text-purple-600 dark:text-purple-400 bg-purple-500/5">
							{t("network.accountBound")}
						</Badge>
					)}
					{node.sourceName && (
						<Badge variant="secondary" className="text-[10px] px-1.5 py-0 truncate max-w-[130px]">
							{node.sourceName}
						</Badge>
					)}
					{node.quality?.state && (
						<Badge variant="destructive" className="text-[10px] px-1.5 py-0 animate-pulse">
							{t("network.courtStatus", { state: node.quality.state })}
						</Badge>
					)}
				</div>

				{/* Dual-Stack Telemetry Box (IPv4 & IPv6 explicitly shown!) */}
				<div className="mt-3 flex flex-col gap-1.5 rounded-lg border border-border/60 bg-muted/20 p-2.5">
					{/* IPv4 Status */}
					<div className="flex items-center justify-between font-mono text-xs">
						<div className="flex items-center gap-1.5 min-w-0">
							<span className="rounded bg-emerald-500/10 px-1 py-0.2 text-[9px] font-bold text-emerald-600 dark:text-emerald-400">
								IPv4
							</span>
							<span className="truncate text-[11px] text-foreground font-semibold">
								{ipv4?.exitIp ? maskIP(ipv4.exitIp) : node.exitIp ? maskIP(node.exitIp) : t("network.notConfigured")}
							</span>
						</div>
						<span className="text-[10px] text-muted-foreground tabular-nums shrink-0">
							{ipv4?.latencyMs ? `${ipv4.latencyMs}ms` : "--"}
						</span>
					</div>

					{/* IPv6 Status */}
					<div className="flex items-center justify-between font-mono text-xs border-t border-border/40 pt-1.5">
						<div className="flex items-center gap-1.5 min-w-0">
							<span className={cn(
								"rounded px-1 py-0.2 text-[9px] font-bold",
								ipv6?.status === "healthy"
									? "bg-cyan-500/10 text-cyan-600 dark:text-cyan-400"
									: "bg-muted text-muted-foreground"
							)}>
								IPv6
							</span>
							<span className="truncate text-[11px] text-muted-foreground">
								{ipv6?.exitIp ? maskIP(ipv6.exitIp) : t("network.ipv6None")}
							</span>
						</div>
						<span className="text-[10px] text-muted-foreground tabular-nums shrink-0">
							{ipv6?.latencyMs ? `${ipv6.latencyMs}ms` : "--"}
						</span>
					</div>
				</div>

				{/* Pool Membership Chips */}
				<div className="mt-2.5 flex items-center gap-1.5 text-xs text-muted-foreground min-h-[22px]">
					<Layers className="size-3.5 shrink-0" />
					<div className="flex flex-wrap gap-1 min-w-0">
						{node.pools && node.pools.length > 0 ? (
							node.pools.map((p) => (
								<Badge
									key={p.id}
									variant="outline"
									className="text-[9px] px-1 py-0 cursor-pointer hover:bg-accent"
									onClick={onViewPools}
								>
									{p.name}
								</Badge>
							))
						) : (
							<span className="text-[11px] text-muted-foreground/60">{t("networkResources.noPool")}</span>
						)}
					</div>
				</div>
			</div>

			{/* Card Footer: Latency Summary & Inline Actions */}
			<div className="mt-3 flex items-center justify-between border-t border-border/60 pt-2.5">
				<div className="flex items-center gap-1.5">
					{isProbing ? (
						<Spinner className="size-3.5" />
					) : node.probeStatus === "healthy" ? (
						<span className={cn("text-xs font-mono tabular-nums", latencyTone.textClass)}>
							{node.probeLatencyMs}ms
						</span>
					) : node.probeStatus === "unhealthy" ? (
						<span className="text-xs font-semibold text-rose-500 font-mono">
							{t("network.unhealthy")}
						</span>
					) : (
						<span className="text-xs text-muted-foreground font-mono">
							{t("network.neverProbed")}
						</span>
					)}
					<span className="text-[10px] text-muted-foreground">
						· {node.lastProbedAt ? formatTimeAgo(node.lastProbedAt, locale) : t("network.neverProbed")}
					</span>
				</div>

				<div className="flex items-center gap-1">
					<Button
						size="icon"
						variant="ghost"
						className="size-6 text-muted-foreground hover:text-amber-500"
						disabled={isProbing}
						onClick={() => onTestProbe(node.id)}
						title={t("network.testLatency")}
					>
						<Zap className="size-3" />
					</Button>

					{node.rotationConfigured && node.rotationEnabled && (
						<Button
							size="icon"
							variant="ghost"
							className="size-6 text-muted-foreground hover:text-blue-500"
							onClick={() => onRotate(node.id)}
							title={t("network.rotateAction")}
						>
							<RefreshCw className="size-3" />
						</Button>
					)}

					{node.proxyConfigured && (
						<Button
							size="icon"
							variant="ghost"
							className="size-6 text-muted-foreground hover:text-foreground"
							onClick={() => onRevealProxyURL(node.id)}
							title={t("network.copyProxyUrl")}
						>
							<Copy className="size-3" />
						</Button>
					)}
				</div>
			</div>
		</div>
	);
});

/**
 * Add or Import Dialog: 3 Tabs (Manual with Quick Test, Bulk Text, Subscriptions)
 */
export function NodeAddOrImportDialog({
	open,
	activeTab,
	onOpenChange,
	existingPools,
	onSuccess,
}: {
	open: boolean;
	activeTab: "manual" | "bulk" | "subscriptions";
	onOpenChange: (open: boolean) => void;
	existingPools: { id: string; name: string; memberIds: string[] }[];
	onSuccess: () => void;
}) {
	const { t } = useTranslation();
	const [tab, setTab] = useState<"manual" | "bulk" | "subscriptions">(activeTab);

	// Manual Add form states
	const [name, setName] = useState("");
	const [enabled, setEnabled] = useState(true);
	const [proxyPool, setProxyPool] = useState(false);
	const [proxyURL, setProxyURL] = useState("");
	const [selectedPoolId, setSelectedPoolId] = useState<string>("");
	const [rotationURL, setRotationURL] = useState("");
	const [rotationEnabled, setRotationEnabled] = useState(false);
	const [submitting, setSubmitting] = useState(false);

	// Bulk text states
	const [bulkContent, setBulkContent] = useState("");
	const [bulkNamePrefix, setBulkNamePrefix] = useState(t("network.defaultPrefixValue"));
	const [bulkLoading, setBulkLoading] = useState(false);

	const handleManualCreate = async () => {
		if (!name.trim()) return;
		setSubmitting(true);
		try {
			const created = await createEgressNode({
				name: name.trim(),
				enabled,
				proxyPool,
				proxyURL: proxyURL.trim() || undefined,
				rotationURL: rotationEnabled ? (rotationURL.trim() || undefined) : undefined,
				rotationEnabled,
			});

			// If pool was chosen, assign node to pool
			if (selectedPoolId) {
				const pool = existingPools.find((p) => p.id === selectedPoolId);
				if (pool) {
					await setEgressPoolMembers(selectedPoolId, [...pool.memberIds, created.id]);
				}
			}

			toast.success(t("network.nodeCreated"));
			setName("");
			setProxyURL("");
			setRotationURL("");
			setSelectedPoolId("");
			onSuccess();
			onOpenChange(false);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.createError"));
		} finally {
			setSubmitting(false);
		}
	};

	const handleBulkImport = async () => {
		if (!bulkContent.trim()) return;
		setBulkLoading(true);
		try {
			const res = await importEgressText({
				name: bulkNamePrefix.trim() || t("network.importedPrefix"),
				content: bulkContent,
			});
			toast.success(t("network.importSuccess", { imported: res.imported, skipped: res.skipped }));
			setBulkContent("");
			onSuccess();
			onOpenChange(false);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.importError"));
		} finally {
			setBulkLoading(false);
		}
	};

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<OperationsDialogContent className="max-w-2xl">
				<DialogHeader>
					<DialogTitle className="flex items-center gap-2">
						<Plus className="size-5 text-primary" />
						<span>{t("network.addNodeOrImport")}</span>
					</DialogTitle>
				</DialogHeader>

				{/* 3 Navigation Tabs */}
				<div className="flex border-b border-border/70 pb-2 gap-2">
					<Button
						variant={tab === "manual" ? "default" : "ghost"}
						size="sm"
						className="h-8 text-xs font-medium"
						onClick={() => setTab("manual")}
					>
						<Server className="size-3.5 mr-1.5" />
						<span>{t("network.addManual")}</span>
					</Button>
					<Button
						variant={tab === "bulk" ? "default" : "ghost"}
						size="sm"
						className="h-8 text-xs font-medium"
						onClick={() => setTab("bulk")}
					>
						<Upload className="size-3.5 mr-1.5" />
						<span>{t("network.addBulkText")}</span>
					</Button>
					<Button
						variant={tab === "subscriptions" ? "default" : "ghost"}
						size="sm"
						className="h-8 text-xs font-medium"
						onClick={() => setTab("subscriptions")}
					>
						<Rss className="size-3.5 mr-1.5 text-orange-500" />
						<span>{t("network.addSubscription")}</span>
					</Button>
				</div>

				{/* Tab 1: Single Node Manual Add */}
				{tab === "manual" ? (
					<div className="space-y-4 py-2">
						<div className="grid grid-cols-2 gap-3">
							<div className="space-y-1.5">
								<Label className="text-xs font-semibold">{t("network.nodeNameLabel")}</Label>
								<Input
									placeholder={t("network.nodeNamePlaceholder")}
									value={name}
									onChange={(e) => setName(e.target.value)}
								/>
							</div>

							<div className="flex items-center justify-between rounded-lg border border-border/80 p-3 self-end h-[42px]">
								<Label className="text-xs font-semibold">{t("networkResources.enabledForRouting")}</Label>
								<Switch checked={enabled} onCheckedChange={setEnabled} />
							</div>
						</div>

						{/* Proxy URL input */}
						<div className="space-y-1.5">
							<Label className="text-xs font-semibold">{t("network.exitAddressLabel")}</Label>
							<Input
								placeholder={t("network.proxyUrlExamplePlaceholder")}
								value={proxyURL}
								onChange={(e) => setProxyURL(e.target.value)}
							/>
							<p className="text-[11px] text-muted-foreground">
								{t("network.proxyUrlProtocolsHint")}
							</p>
						</div>

						{/* Assign to pool directly */}
						<div className="space-y-1.5">
							<Label className="text-xs font-semibold">{t("network.directAssignPoolLabel")}</Label>
							<Select value={selectedPoolId} onValueChange={setSelectedPoolId}>
								<SelectTrigger className="w-full">
									<SelectValue placeholder={t("network.doNotAssignPoolPlaceholder")} />
								</SelectTrigger>
								<SelectContent>
									<SelectItem value="">{t("network.doNotAssignPoolOption")}</SelectItem>
									{existingPools.map((p) => (
										<SelectItem key={p.id} value={p.id}>
											{p.name}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</div>

						{/* Advanced Toggles */}
						<div className="grid grid-cols-2 gap-3 pt-1">
							<div className="flex items-center justify-between rounded-lg border border-border/80 p-3">
								<div>
									<Label className="text-xs font-semibold">{t("network.dynamicEndpointToggle")}</Label>
									<p className="text-[10px] text-muted-foreground">{t("network.dynamicEndpointDesc")}</p>
								</div>
								<Switch
									checked={proxyPool}
									onCheckedChange={(checked) => {
										setProxyPool(checked);
										if (checked) setRotationEnabled(false);
									}}
								/>
							</div>

							<div className="flex items-center justify-between rounded-lg border border-border/80 p-3">
								<div>
									<Label className="text-xs font-semibold">{t("network.enableRotationWebhook")}</Label>
									<p className="text-[10px] text-muted-foreground">{t("network.triggerOnBan")}</p>
								</div>
								<Switch
									checked={rotationEnabled}
									disabled={proxyPool}
									onCheckedChange={(checked) => {
										setRotationEnabled(checked);
										if (checked) setProxyPool(false);
									}}
								/>
							</div>
						</div>

						{rotationEnabled && (
							<div className="space-y-1.5">
								<Label className="text-xs font-semibold">{t("network.rotationTemplateLabel")}</Label>
								<Input
									placeholder="https://api.myproxy.com/rotate?key=xxx"
									value={rotationURL}
									onChange={(e) => setRotationURL(e.target.value)}
								/>
							</div>
						)}

						<DialogFooter>
							<Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
								{t("common.cancel")}
							</Button>
							<Button size="sm" disabled={!name.trim() || submitting} onClick={handleManualCreate}>
								{submitting && <Spinner className="size-3.5 mr-1.5" />}
								<span>{t("common.save")}</span>
							</Button>
						</DialogFooter>
					</div>
				) : tab === "bulk" ? (
					/* Tab 2: Bulk Text Import */
					<div className="space-y-4 py-2">
						<div className="space-y-1.5">
							<Label className="text-xs font-semibold">{t("network.namePrefix")}</Label>
							<Input
								placeholder={t("network.bulkPrefixPlaceholder")}
								value={bulkNamePrefix}
								onChange={(e) => setBulkNamePrefix(e.target.value)}
							/>
						</div>

						<div className="space-y-1.5">
							<Label className="text-xs font-semibold">{t("network.proxyListPlaceholder")}</Label>
							<Textarea
								rows={8}
								placeholder={t("network.importPlaceholder")}
								value={bulkContent}
								onChange={(e) => setBulkContent(e.target.value)}
								className="font-mono text-xs"
							/>
						</div>

						<DialogFooter>
							<Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
								{t("common.cancel")}
							</Button>
							<Button
								size="sm"
								disabled={!bulkContent.trim() || bulkLoading}
								onClick={handleBulkImport}
							>
								{bulkLoading && <Spinner className="size-3.5 mr-1.5" />}
								<span>{t("network.importAction")}</span>
							</Button>
						</DialogFooter>
					</div>
				) : (
					/* Tab 3: Subscriptions Hub */
					<div className="py-2">
						<SubscriptionsPanel showHeader={false} />
					</div>
				)}
			</OperationsDialogContent>
		</Dialog>
	);
}

/**
 * Node Edit Dialog
 */
function NodeEditDialog({
	node,
	open,
	onOpenChange,
	onSuccess,
}: {
	node: EgressNodeDTO;
	open: boolean;
	onOpenChange: (open: boolean) => void;
	onSuccess: () => void;
}) {
	const { t } = useTranslation();
	const [name, setName] = useState(node.name);
	const [enabled, setEnabled] = useState(node.enabled);
	const [proxyPool, setProxyPool] = useState(node.proxyPool);
	const [proxyURL, setProxyURL] = useState("");
	const [clearProxyURL, setClearProxyURL] = useState(false);
	const [rotationURL, setRotationURL] = useState("");
	const [clearRotationURL, setClearRotationURL] = useState(false);
	const [rotationEnabled, setRotationEnabled] = useState(node.rotationEnabled);
	const [loading, setLoading] = useState(false);

	const handleSave = async () => {
		setLoading(true);
		try {
			await updateEgressNode(node.id, {
				name: name.trim() || node.name,
				enabled,
				proxyPool,
				proxyURL: proxyURL.trim() || undefined,
				clearProxyURL,
				rotationURL: rotationEnabled ? (rotationURL.trim() || undefined) : undefined,
				clearRotationURL: !rotationEnabled ? true : clearRotationURL,
				rotationEnabled,
			});
			toast.success(t("network.nodeUpdated"));
			onSuccess();
			onOpenChange(false);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.updateError"));
		} finally {
			setLoading(false);
		}
	};

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<OperationsDialogContent className="max-w-md">
				<DialogHeader>
					<DialogTitle className="flex items-center gap-2">
						<Pencil className="size-5 text-primary" />
						<span>{t("common.edit")}: {node.name}</span>
					</DialogTitle>
				</DialogHeader>

				<div className="space-y-3.5 py-2">
					<div className="space-y-1.5">
						<Label className="text-xs font-semibold">{t("network.nodeNameLabel")}</Label>
						<Input value={name} onChange={(e) => setName(e.target.value)} />
					</div>

					<div className="flex items-center justify-between rounded-lg border border-border/80 p-3">
						<Label className="text-xs font-semibold">{t("networkResources.enabledForRouting")}</Label>
						<Switch checked={enabled} onCheckedChange={setEnabled} />
					</div>

					<div className="space-y-1.5">
						<div className="flex items-center justify-between">
							<Label className="text-xs font-semibold">{t("network.exitAddressLabel")}</Label>
							<div className="flex items-center gap-1.5">
								<Label htmlFor="clear-proxy-checkbox" className="text-[11px] text-destructive cursor-pointer">
									{t("network.clearUrl")}
								</Label>
								<Checkbox
									id="clear-proxy-checkbox"
									checked={clearProxyURL}
									onCheckedChange={(c) => setClearProxyURL(Boolean(c))}
								/>
							</div>
						</div>
						<Input
							placeholder={t("network.keepExisting")}
							disabled={clearProxyURL}
							value={proxyURL}
							onChange={(e) => setProxyURL(e.target.value)}
						/>
					</div>

					<div className="grid grid-cols-2 gap-2">
						<div className="flex items-center justify-between rounded-lg border border-border/80 p-2.5">
							<Label className="text-xs font-semibold">{t("network.dynamicPoolLabel")}</Label>
							<Switch
								checked={proxyPool}
								onCheckedChange={(checked) => {
									setProxyPool(checked);
									if (checked) setRotationEnabled(false);
								}}
							/>
						</div>

						<div className="flex items-center justify-between rounded-lg border border-border/80 p-2.5">
							<Label className="text-xs font-semibold">{t("network.rotateWebhookLabel")}</Label>
							<Switch
								checked={rotationEnabled}
								disabled={proxyPool}
								onCheckedChange={(checked) => {
									setRotationEnabled(checked);
									if (checked) setProxyPool(false);
								}}
							/>
						</div>
					</div>

					{rotationEnabled && (
						<div className="space-y-1.5">
							<div className="flex items-center justify-between">
								<Label className="text-xs font-semibold">{t("network.rotationTemplateLabel")}</Label>
								<div className="flex items-center gap-1.5">
									<Label htmlFor="clear-rot-checkbox" className="text-[11px] text-destructive cursor-pointer">
										{t("network.clearUrl")}
									</Label>
									<Checkbox
										id="clear-rot-checkbox"
										checked={clearRotationURL}
										onCheckedChange={(c) => setClearRotationURL(Boolean(c))}
									/>
								</div>
							</div>
							<Input
								placeholder={t("network.keepExisting")}
								disabled={clearRotationURL}
								value={rotationURL}
								onChange={(e) => setRotationURL(e.target.value)}
							/>
						</div>
					)}
				</div>

				<DialogFooter>
					<Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
						{t("common.cancel")}
					</Button>
					<Button size="sm" disabled={loading} onClick={handleSave}>
						{loading && <Spinner className="size-3.5 mr-1.5" />}
						<span>{t("common.save")}</span>
					</Button>
				</DialogFooter>
			</OperationsDialogContent>
		</Dialog>
	);
}