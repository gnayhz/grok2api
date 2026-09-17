import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
	ArrowDown,
	ArrowRight,
	ArrowUp,
	ArrowUpDown,
	BarChart3,
	Layers,
	MoreHorizontal,
	Pencil,
	Plus,
	RotateCcw,
	Search,
	Trash2,
	Users,
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
} from "@/shared/ui/alert-dialog";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Checkbox } from "@/shared/ui/checkbox";
import { Dialog, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { OperationsDialogContent, OperationsAlertDialogContent } from "@/shared/ui/operations";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/shared/ui/dropdown-menu";
import { Input } from "@/shared/ui/input";
import { Label } from "@/shared/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/shared/ui/select";
import { Spinner } from "@/shared/ui/spinner";
import { Switch } from "@/shared/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { useNow } from "@/shared/lib/use-now";
import { nodeCondition } from "@/entities/egress/node-condition";

import {
	createEgressPool,
	deleteEgressPool,
	getEgressPoolStats,
	resetEgressPoolStats,
	setEgressPoolMembers,
	updateEgressPool,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressPoolFallbackMode,
	type EgressPoolStrategy,
} from "@/entities/egress/egress-api";
import { cn } from "@/shared/lib/cn";

import { maskIP } from "@/shared/lib/mask-ip";
import { formatTimeAgo, getLatencyTone } from "./proxy-format";
import { useEgressNodes, useEgressPools } from "@/entities/egress/egress-queries";

export function ProxyPoolsView({
	focusPoolId,
	onViewNodes,
}: {
	focusPoolId?: string;
	onViewNodes?: () => void;
}) {
	const { t, i18n } = useTranslation();
	const queryClient = useQueryClient();
	const poolsQuery = useEgressPools();
	const nodesQuery = useEgressNodes();
	const now = useNow(15_000);

	const [search, setSearch] = useState("");
	const [createModalOpen, setCreateModalOpen] = useState(false);
	const [editPool, setEditPool] = useState<EgressPoolDTO | null>(null);
	const [membersModalPool, setMembersModalPool] = useState<EgressPoolDTO | null>(null);
	const [statsModalPool, setStatsModalPool] = useState<EgressPoolDTO | null>(null);
	const [deleteConfirmPool, setDeleteConfirmPool] = useState<EgressPoolDTO | null>(null);

	const pools = useMemo(() => poolsQuery.data ?? [], [poolsQuery.data]);
	const nodes = useMemo(() => nodesQuery.data?.items ?? [], [nodesQuery.data?.items]);

	// Node map
	const nodeMap = useMemo(() => new Map(nodes.map((n) => [n.id, n])), [nodes]);

	// Filtered pools
	const filteredPools = useMemo(() => {
		if (!search.trim()) return pools;
		const needle = search.trim().toLowerCase();
		return pools.filter(
			(p) =>
				p.name.toLowerCase().includes(needle) ||
				p.strategy.toLowerCase().includes(needle) ||
				(p.fallbackPoolName && p.fallbackPoolName.toLowerCase().includes(needle))
		);
	}, [pools, search]);

	const refreshPools = useCallback(() => {
		void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
		void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
	}, [queryClient]);

	const handleEditPool = useCallback((pool: EgressPoolDTO) => setEditPool(pool), []);
	const handleManageMembers = useCallback((pool: EgressPoolDTO) => setMembersModalPool(pool), []);
	const handleViewStats = useCallback((pool: EgressPoolDTO) => setStatsModalPool(pool), []);
	const handleDeletePool = useCallback((pool: EgressPoolDTO) => setDeleteConfirmPool(pool), []);
	const handleTogglePoolEnabled = useCallback(
		async (pool: EgressPoolDTO, enabled: boolean) => {
			try {
				await updateEgressPool(pool.id, {
					name: pool.name,
					enabled,
					strategy: pool.strategy,
					fallbackMode: pool.fallbackMode,
					fallbackPoolId: pool.fallbackPoolId,
				});
				refreshPools();
				toast.success(`${pool.name} ${enabled ? t("common.enabled") : t("common.disabled")}`);
			} catch (err) {
				toast.error(err instanceof Error ? err.message : t("network.updateError"));
			}
		},
		[refreshPools, t]
	);

	return (
		<div className="flex flex-col gap-5">
			{/* Top Bar: Search & Create */}
			<div className="flex flex-wrap items-center justify-between gap-3">
				<div className="flex items-center gap-3">
					<div className="relative min-w-[240px]">
						<Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
						<Input
							placeholder={t("network.searchPoolsPlaceholder")}
							className="h-8 pl-8 text-xs"
							value={search}
							onChange={(e) => setSearch(e.target.value)}
						/>
					</div>
					<span className="text-xs text-muted-foreground font-medium">
						{t("network.poolsCountConfigured", { count: pools.length })}
					</span>
				</div>

				<div className="flex items-center gap-2">
					<Button
						size="sm"
						className="gap-1.5 shadow-sm bg-primary text-primary-foreground hover:bg-primary/90 font-medium"
						onClick={() => setCreateModalOpen(true)}
					>
						<Plus className="size-4" />
						<span>{t("network.createPool")}</span>
					</Button>
				</div>
			</div>

			{/* Pools Grid Deck */}
			{filteredPools.length === 0 ? (
				<div className="flex h-56 flex-col items-center justify-center rounded-xl border border-dashed border-border/80 bg-card/40 p-6 text-center">
					<Layers className="size-8 text-muted-foreground/40 mb-2" />
					<p className="text-sm font-semibold text-foreground">
						{search ? t("network.noMatchingPools") : t("networkPools.emptyTitle")}
					</p>
					<p className="text-xs text-muted-foreground mt-1 max-w-sm cursor-pointer" onClick={onViewNodes}>
						{t("networkPools.emptyDescription")}
					</p>
					{!search && (
						<Button
							size="sm"
							className="mt-3 gap-1.5"
							onClick={() => setCreateModalOpen(true)}
						>
							<Plus className="size-3.5" />
							<span>{t("network.createPool")}</span>
						</Button>
					)}
				</div>
			) : (
				<div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
					{filteredPools.map((pool) => (
						<ProxyPoolCardItem
							key={pool.id}
							pool={pool}
							nodeMap={nodeMap}
							now={now}
							locale={i18n.language}
							isFocused={focusPoolId === pool.id}
							onEdit={handleEditPool}
							onManageMembers={handleManageMembers}
							onViewStats={handleViewStats}
							onDelete={handleDeletePool}
							onToggleEnabled={handleTogglePoolEnabled}
						/>
					))}
				</div>
			)}

			{/* Create Pool Dialog */}
			<PoolConfigModal
				open={createModalOpen}
				existingPools={pools}
				onOpenChange={setCreateModalOpen}
				onSuccess={refreshPools}
			/>

			{/* Edit Pool Dialog */}
			{editPool && (
				<PoolConfigModal
					pool={editPool}
					open={Boolean(editPool)}
					existingPools={pools}
					onOpenChange={(open) => !open && setEditPool(null)}
					onSuccess={refreshPools}
				/>
			)}

			{/* Pool Members Organizer Modal */}
			{membersModalPool && (
				<PoolMembersOrganizerModal
					pool={membersModalPool}
					allNodes={nodes}
					open={Boolean(membersModalPool)}
					onOpenChange={(open) => !open && setMembersModalPool(null)}
					onSuccess={refreshPools}
				/>
			)}

			{/* Pool Stats & Analytics Modal (No overlap with X close icon!) */}
			{statsModalPool && (
				<PoolStatsModal
					pool={statsModalPool}
					nodeMap={nodeMap}
					open={Boolean(statsModalPool)}
					onOpenChange={(open) => !open && setStatsModalPool(null)}
					onReset={refreshPools}
				/>
			)}

			{/* Delete Pool Dialog */}
			<AlertDialog
				open={Boolean(deleteConfirmPool)}
				onOpenChange={(open) => !open && setDeleteConfirmPool(null)}
			>
				<OperationsAlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>{t("common.delete")}</AlertDialogTitle>
						<AlertDialogDescription>
							{t("network.deletePoolConfirm", { name: deleteConfirmPool?.name ?? "" })}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
							onClick={async () => {
								if (deleteConfirmPool) {
									await deleteEgressPool(deleteConfirmPool.id);
									refreshPools();
									setDeleteConfirmPool(null);
									toast.success(t("network.poolDeleted"));
								}
							}}
						>
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</OperationsAlertDialogContent>
			</AlertDialog>
		</div>
	);
}

/**
 * Strategy Labels & Badge Styling
 */
function getStrategyConfig(t: (key: string) => string): Record<
	EgressPoolStrategy,
	{ label: string; tone: string; desc: string }
> {
	return {
		affinity: {
			label: t("network.stratAffinity"),
			tone: "border-purple-500/30 text-purple-600 dark:text-purple-400 bg-purple-500/5",
			desc: t("network.stratAffinityDesc"),
		},
		"least-used": {
			label: t("network.stratLeastUsed"),
			tone: "border-emerald-500/30 text-emerald-600 dark:text-emerald-400 bg-emerald-500/5",
			desc: t("network.stratLeastUsedDesc"),
		},
		sticky: {
			label: t("network.stratSticky"),
			tone: "border-blue-500/30 text-blue-600 dark:text-blue-400 bg-blue-500/5",
			desc: t("network.stratStickyDesc"),
		},
		rotation: {
			label: t("network.stratRotation"),
			tone: "border-amber-500/30 text-amber-600 dark:text-amber-400 bg-amber-500/5",
			desc: t("network.stratRotationDesc"),
		},
		random: {
			label: t("network.stratRandom"),
			tone: "border-cyan-500/30 text-cyan-600 dark:text-cyan-400 bg-cyan-500/5",
			desc: t("network.stratRandomDesc"),
		},
	};
}

/**
 * Individual Pool Card
 */
const ProxyPoolCardItem = memo(function ProxyPoolCardItem({
	pool,
	nodeMap,
	now,
	locale,
	isFocused,
	onEdit,
	onManageMembers,
	onViewStats,
	onDelete,
	onToggleEnabled,
}: {
	pool: EgressPoolDTO;
	nodeMap: Map<string, EgressNodeDTO>;
	now: number;
	locale: string;
	isFocused: boolean;
	onEdit: (pool: EgressPoolDTO) => void;
	onManageMembers: (pool: EgressPoolDTO) => void;
	onViewStats: (pool: EgressPoolDTO) => void;
	onDelete: (pool: EgressPoolDTO) => void;
	onToggleEnabled: (pool: EgressPoolDTO, enabled: boolean) => void;
}) {
	const { t } = useTranslation();

	const members = pool.memberIds.map((id) => nodeMap.get(id)).filter(Boolean) as EgressNodeDTO[];
	const readyMembers = members.filter((m) => {
		const c = nodeCondition(m, now);
		return c === "ready" || c === "dynamic";
	});
	const unhealthyMembers = members.filter((m) => nodeCondition(m, now) === "unhealthy");
	const quarantinedMembers = members.filter((m) => {
		const c = nodeCondition(m, now);
		return c === "held" || c === "banned";
	});

	const total = pool.memberIds.length;
	const readyCount = readyMembers.length;
	const healthRatio = total > 0 ? Math.round((readyCount / total) * 100) : 0;

	const stratConfig = getStrategyConfig(t);
	const strat = stratConfig[pool.strategy] ?? stratConfig.random;

	return (
		<div
			className={cn(
				"group relative flex flex-col justify-between rounded-xl border bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md",
				isFocused ? "border-primary ring-1 ring-primary/40" : "border-border/80"
			)}
		>
			<div>
				{/* Header */}
				<div className="flex items-start justify-between gap-2">
					<div className="min-w-0">
						<div className="flex items-center gap-2">
							<span
								className={cn(
									"size-2.5 rounded-full shrink-0",
									!pool.enabled
										? "bg-zinc-400"
										: readyCount > 0
											? "bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.5)]"
											: "bg-rose-500 animate-pulse"
								)}
							/>
							<h3 className="truncate text-base font-bold text-foreground" title={pool.name}>
								{pool.name}
							</h3>
						</div>
						<p className="mt-0.5 text-[11px] text-muted-foreground">{strat.desc}</p>
					</div>

					<div className="flex items-center gap-1.5">
						<Switch
							checked={pool.enabled}
							onCheckedChange={(val) => onToggleEnabled(pool, val)}
							className="scale-90"
							title={pool.enabled ? t("common.disable") : t("common.enable")}
						/>

						<DropdownMenu>
							<DropdownMenuTrigger asChild>
								<Button size="icon" variant="ghost" className="size-7 opacity-70 group-hover:opacity-100">
									<MoreHorizontal className="size-3.5" />
								</Button>
							</DropdownMenuTrigger>
							<DropdownMenuContent align="end" className="w-36">
								<DropdownMenuItem onClick={() => onManageMembers(pool)}>
									<Users className="mr-2 size-3.5 text-primary" />
									<span>{t("network.poolActionManage")}</span>
								</DropdownMenuItem>
								<DropdownMenuItem onClick={() => onViewStats(pool)}>
									<BarChart3 className="mr-2 size-3.5 text-blue-500" />
									<span>{t("network.poolActionStats")}</span>
								</DropdownMenuItem>
								<DropdownMenuItem onClick={() => onEdit(pool)}>
									<Pencil className="mr-2 size-3.5" />
									<span>{t("common.edit")}</span>
								</DropdownMenuItem>
								<DropdownMenuSeparator />
								<DropdownMenuItem onClick={() => onDelete(pool)} className="text-destructive focus:text-destructive">
									<Trash2 className="mr-2 size-3.5" />
									<span>{t("common.delete")}</span>
								</DropdownMenuItem>
							</DropdownMenuContent>
						</DropdownMenu>
					</div>
				</div>

				{/* Strategy badge & Fallback route chip */}
				<div className="mt-3 flex flex-wrap items-center gap-1.5">
					<Badge variant="outline" className={cn("text-xs font-semibold px-2 py-0.5", strat.tone)}>
						{strat.label}
					</Badge>

					<Badge variant="secondary" className="text-[10px] px-2 py-0.5 flex items-center gap-1">
						<ArrowRight className="size-3 text-muted-foreground" />
						<span>
							{pool.fallbackMode === "none"
								? t("network.failClosed")
								: pool.fallbackMode === "direct"
									? t("network.failoverDirect")
									: t("network.failoverPool", { name: pool.fallbackPoolName || "Secondary" })}
						</span>
					</Badge>
				</div>

				{/* Health & Members Capacity Bar */}
				<div className="mt-3.5 rounded-lg border border-border/60 bg-muted/20 p-2.5">
					<div className="flex items-center justify-between text-xs">
						<span className="text-muted-foreground font-medium">{t("network.membersHealth")}</span>
						<span className="font-mono font-bold tabular-nums text-foreground">
							{t("network.availableMembersRatio", { ready: readyCount, total, pct: healthRatio })}
						</span>
					</div>

					<div className="mt-2 flex h-2 w-full overflow-hidden rounded-full bg-muted">
						{total === 0 ? (
							<div className="h-full w-full bg-zinc-300 dark:bg-zinc-700" />
						) : (
							<>
								<div
									className="h-full bg-emerald-500 transition-all duration-300"
									style={{ width: `${(readyCount / total) * 100}%` }}
								/>
								<div
									className="h-full bg-rose-500 transition-all duration-300"
									style={{ width: `${(unhealthyMembers.length / total) * 100}%` }}
								/>
								<div
									className="h-full bg-purple-500 transition-all duration-300"
									style={{ width: `${(quarantinedMembers.length / total) * 100}%` }}
								/>
							</>
						)}
					</div>
				</div>

				{/* Quick Member Tags Preview */}
				<div className="mt-3 flex flex-wrap gap-1">
					{members.slice(0, 4).map((m, idx) => {
						const c = nodeCondition(m, now);
						const isReady = c === "ready" || c === "dynamic";
						return (
							<span
								key={m.id}
								className={cn(
									"inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-mono",
									isReady
										? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 border border-emerald-500/20"
										: "bg-muted text-muted-foreground border border-border"
								)}
							>
								<span className="text-[9px] font-bold text-muted-foreground/80">#{idx + 1}</span>
								<span className="truncate max-w-[80px]">{m.name}</span>
								{m.rotatingEndpoint && (
									<span className="text-[8px] px-1 rounded bg-blue-500/10 text-blue-500 font-sans">
										{t("network.dynamicTunnel")}
									</span>
								)}
							</span>
						);
					})}
					{total > 4 && (
						<span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
							{t("network.moreCount", { count: total - 4 })}
						</span>
					)}
				</div>
			</div>

			{/* Card Footer */}
			<div className="mt-4 flex items-center justify-between border-t border-border/60 pt-2.5 text-xs text-muted-foreground">
				<span className="text-[10px]">
					{pool.updatedAt ? formatTimeAgo(pool.updatedAt, locale) : t("network.pulseLive")}
				</span>

				<div className="flex items-center gap-1.5">
					<Button
						size="sm"
						variant="ghost"
						className="h-7 text-xs gap-1 text-primary hover:text-primary"
						onClick={() => onManageMembers(pool)}
					>
						<Users className="size-3" />
						<span>{t("network.poolActionManage")} ({total})</span>
					</Button>
					<Button
						size="sm"
						variant="ghost"
						className="h-7 text-xs gap-1 text-blue-500 hover:text-blue-600"
						onClick={() => onViewStats(pool)}
					>
						<BarChart3 className="size-3" />
						<span>{t("network.poolActionStats")}</span>
					</Button>
				</div>
			</div>
		</div>
	);
});

/**
 * Pool Create / Edit Form Modal
 */
function PoolConfigModal({
	pool,
	existingPools,
	open,
	onOpenChange,
	onSuccess,
}: {
	pool?: EgressPoolDTO;
	existingPools: EgressPoolDTO[];
	open: boolean;
	onOpenChange: (open: boolean) => void;
	onSuccess: () => void;
}) {
	const { t } = useTranslation();
	const [name, setName] = useState(pool?.name ?? "");
	const [enabled, setEnabled] = useState(pool?.enabled ?? true);
	const [strategy, setStrategy] = useState<EgressPoolStrategy>(pool?.strategy ?? "least-used");
	const [fallbackMode, setFallbackMode] = useState<EgressPoolFallbackMode>(
		pool?.fallbackMode ?? "none"
	);
	const [fallbackPoolId, setFallbackPoolId] = useState<string>(pool?.fallbackPoolId ?? "");
	const [loading, setLoading] = useState(false);

	const availableFallbackPools = existingPools.filter((p) => !pool || p.id !== pool.id);

	const handleSave = async () => {
		if (!name.trim()) return;
		setLoading(true);
		try {
			const payload = {
				name: name.trim(),
				enabled,
				strategy,
				fallbackMode,
				fallbackPoolId: fallbackMode === "pool" ? fallbackPoolId || undefined : undefined,
			};

			if (pool) {
				await updateEgressPool(pool.id, payload);
				toast.success(t("network.poolUpdated"));
			} else {
				await createEgressPool(payload);
				toast.success(t("network.poolCreated"));
			}
			onSuccess();
			onOpenChange(false);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.saveError"));
		} finally {
			setLoading(false);
		}
	};

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<OperationsDialogContent className="max-w-md">
				<DialogHeader>
					<DialogTitle className="flex items-center gap-2">
						<Layers className="size-5 text-primary" />
						<span>{pool ? t("network.editPoolTitle", { name: pool.name }) : t("network.createPoolTitle")}</span>
					</DialogTitle>
				</DialogHeader>

				<div className="space-y-4 py-2">
					<div className="space-y-1.5">
						<Label className="text-xs font-semibold">{t("network.poolNameLabel")}</Label>
						<Input
							placeholder={t("network.poolNamePlaceholder")}
							value={name}
							onChange={(e) => setName(e.target.value)}
						/>
					</div>

					<div className="flex items-center justify-between rounded-lg border border-border/80 p-3">
						<Label className="text-xs font-semibold">{t("network.enablePoolDispatch")}</Label>
						<Switch checked={enabled} onCheckedChange={setEnabled} />
					</div>

					<div className="space-y-1.5">
						<Label className="text-xs font-semibold">{t("network.selectionStrategy")}</Label>
						<Select value={strategy} onValueChange={(val: EgressPoolStrategy) => setStrategy(val)}>
							<SelectTrigger className="w-full">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectItem value="least-used">
									{t("network.stratLeastUsed")}
								</SelectItem>
								<SelectItem value="affinity">
									{t("network.stratAffinity")}
								</SelectItem>
								<SelectItem value="rotation">
									{t("network.stratRotation")}
								</SelectItem>
								<SelectItem value="sticky">
									{t("network.stratSticky")}
								</SelectItem>
								<SelectItem value="random">
									{t("network.stratRandom")}
								</SelectItem>
							</SelectContent>
						</Select>
						<p className="text-[11px] text-muted-foreground">
							{getStrategyConfig(t)[strategy]?.desc}
						</p>
					</div>

					<div className="space-y-1.5">
						<Label className="text-xs font-semibold">{t("network.fallbackPolicy")}</Label>
						<Select
							value={fallbackMode}
							onValueChange={(val: EgressPoolFallbackMode) => setFallbackMode(val)}
						>
							<SelectTrigger className="w-full">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectItem value="none">{t("network.failClosedOption")}</SelectItem>
								<SelectItem value="direct">{t("network.directOption")}</SelectItem>
								<SelectItem value="pool">{t("network.fallbackPoolOption")}</SelectItem>
							</SelectContent>
						</Select>
					</div>

					{fallbackMode === "pool" && (
						<div className="space-y-1.5">
							<Label className="text-xs font-semibold">{t("network.fallbackSecondaryPool")}</Label>
							<Select value={fallbackPoolId} onValueChange={setFallbackPoolId}>
								<SelectTrigger className="w-full">
									<SelectValue placeholder={t("network.selectFallbackPoolPlaceholder")} />
								</SelectTrigger>
								<SelectContent>
									{availableFallbackPools.map((p) => (
										<SelectItem key={p.id} value={p.id}>
											{p.name}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</div>
					)}
				</div>

				<DialogFooter>
					<Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
						{t("common.cancel")}
					</Button>
					<Button size="sm" disabled={!name.trim() || loading} onClick={handleSave}>
						{loading && <Spinner className="size-3.5 mr-1.5" />}
						<span>{t("common.save")}</span>
					</Button>
				</DialogFooter>
			</OperationsDialogContent>
		</Dialog>
	);
}

/**
 * Pool Members Organizer Modal
 */
function PoolMembersOrganizerModal({
	pool,
	allNodes,
	open,
	onOpenChange,
	onSuccess,
}: {
	pool: EgressPoolDTO;
	allNodes: EgressNodeDTO[];
	open: boolean;
	onOpenChange: (open: boolean) => void;
	onSuccess: () => void;
}) {
	const { t } = useTranslation();
	const [memberIds, setMemberIds] = useState<string[]>(pool.memberIds);
	const [search, setSearch] = useState("");
	const [scope, setScope] = useState<"all" | "joined" | "unjoined">("all");
	const [loading, setLoading] = useState(false);

	const memberSet = useMemo(() => new Set(memberIds), [memberIds]);

	// Filter nodes based on scope and search
	const filtered = useMemo(() => {
		let list = allNodes;

		if (scope === "joined") {
			// In joined scope, keep priority order!
			const nodeMap = new Map(allNodes.map((n) => [n.id, n]));
			list = memberIds.map((id) => nodeMap.get(id)).filter((n): n is EgressNodeDTO => Boolean(n));
		} else if (scope === "unjoined") {
			list = allNodes.filter((n) => !memberSet.has(n.id));
		}

		if (search.trim()) {
			const needle = search.trim().toLowerCase();
			list = list.filter(
				(n) => n.name.toLowerCase().includes(needle) || (n.exitIp && n.exitIp.includes(needle))
			);
		}

		return list;
	}, [allNodes, memberIds, memberSet, scope, search]);

	const toggleMember = (nodeId: string) => {
		if (memberSet.has(nodeId)) {
			setMemberIds(memberIds.filter((id) => id !== nodeId));
		} else {
			setMemberIds([...memberIds, nodeId]);
		}
	};

	// Batch Selection Logic
	const filteredNodeIds = useMemo(() => filtered.map((n) => n.id), [filtered]);
	const selectedCountInFiltered = useMemo(
		() => filteredNodeIds.filter((id) => memberSet.has(id)).length,
		[filteredNodeIds, memberSet]
	);
	const isAllFilteredSelected = filtered.length > 0 && selectedCountInFiltered === filtered.length;
	const isSomeFilteredSelected = selectedCountInFiltered > 0 && !isAllFilteredSelected;

	const handleToggleMasterSelect = () => {
		if (isAllFilteredSelected) {
			const toRemove = new Set(filteredNodeIds);
			setMemberIds(memberIds.filter((id) => !toRemove.has(id)));
		} else {
			const toAdd = filteredNodeIds.filter((id) => !memberSet.has(id));
			setMemberIds([...memberIds, ...toAdd]);
		}
	};

	const handleAddAllFiltered = () => {
		const toAdd = filteredNodeIds.filter((id) => !memberSet.has(id));
		if (toAdd.length > 0) {
			setMemberIds([...memberIds, ...toAdd]);
			toast.success(t("network.membersAssignedCount", { count: memberIds.length + toAdd.length }));
		}
	};

	const handleRemoveAllFiltered = () => {
		const toRemove = new Set(filteredNodeIds);
		const remaining = memberIds.filter((id) => !toRemove.has(id));
		setMemberIds(remaining);
	};

	const handleInvertFiltered = () => {
		const next = new Set(memberIds);
		for (const id of filteredNodeIds) {
			if (next.has(id)) {
				next.delete(id);
			} else {
				next.add(id);
			}
		}
		const newOrdered = memberIds.filter((id) => next.has(id));
		for (const id of filteredNodeIds) {
			if (next.has(id) && !newOrdered.includes(id)) {
				newOrdered.push(id);
			}
		}
		setMemberIds(newOrdered);
	};

	// Smart Priority Sorters
	const handleSortByLatency = () => {
		const nodeMap = new Map(allNodes.map((n) => [n.id, n]));
		const sorted = [...memberIds].sort((a, b) => {
			const nodeA = nodeMap.get(a);
			const nodeB = nodeMap.get(b);
			const latA =
				(nodeA?.probeStatus === "healthy" ? nodeA.probeLatencyMs : 999999) || 999999;
			const latB =
				(nodeB?.probeStatus === "healthy" ? nodeB.probeLatencyMs : 999999) || 999999;
			return latA - latB;
		});
		setMemberIds(sorted);
		toast.success(t("network.sortByLatency") + " ✓");
	};

	const handleSortByName = () => {
		const nodeMap = new Map(allNodes.map((n) => [n.id, n]));
		const sorted = [...memberIds].sort((a, b) => {
			const nameA = nodeMap.get(a)?.name || "";
			const nameB = nodeMap.get(b)?.name || "";
			return nameA.localeCompare(nameB);
		});
		setMemberIds(sorted);
		toast.success(t("network.sortByName") + " ✓");
	};

	const movePriority = (index: number, direction: "up" | "down") => {
		const targetIndex = direction === "up" ? index - 1 : index + 1;
		if (targetIndex < 0 || targetIndex >= memberIds.length) return;
		const next = [...memberIds];
		const temp = next[index]!;
		next[index] = next[targetIndex]!;
		next[targetIndex] = temp;
		setMemberIds(next);
	};

	const handleSave = async () => {
		setLoading(true);
		try {
			await setEgressPoolMembers(pool.id, memberIds);
			toast.success(t("network.poolUpdated"));
			onSuccess();
			onOpenChange(false);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.updateMembersError"));
		} finally {
			setLoading(false);
		}
	};

	const unjoinedCount = allNodes.length - memberIds.length;

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<OperationsDialogContent className="max-w-2xl max-h-[85vh] flex flex-col">
				<DialogHeader>
					<div className="flex items-center justify-between pr-6">
						<DialogTitle className="flex items-center gap-2">
							<Users className="size-5 text-primary" />
							<span>{t("network.manageMembersTitle", { name: pool.name })}</span>
						</DialogTitle>
						<span className="text-xs font-semibold tabular-nums text-foreground bg-primary/10 text-primary px-2.5 py-0.5 rounded-full">
							{t("network.membersAssignedCount", { count: memberIds.length })}
						</span>
					</div>
				</DialogHeader>

				<div className="space-y-3 py-1 flex-1 flex flex-col min-h-0">
					{/* Scope Selector Tabs */}
					<div className="flex items-center gap-1.5 border-b border-border/70 pb-2">
						{[
							{ id: "all", label: t("network.filterAllScope", { count: allNodes.length }) },
							{ id: "joined", label: t("network.filterJoinedScope", { count: memberIds.length }) },
							{ id: "unjoined", label: t("network.filterUnjoinedScope", { count: unjoinedCount }) },
						].map((tab) => (
							<button
								type="button"
								key={tab.id}
								onClick={() => setScope(tab.id as "all" | "joined" | "unjoined")}
								className={cn(
									"rounded-lg px-2.5 py-1 text-xs font-medium transition-all cursor-pointer",
									scope === tab.id
										? "bg-primary text-primary-foreground shadow-xs font-semibold"
										: "bg-muted/50 border border-border/60 text-muted-foreground hover:border-border hover:text-foreground"
								)}
							>
								{tab.label}
							</button>
						))}
					</div>

					{/* Search & Batch Action Toolbar */}
					<div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
						<div className="relative flex-1">
							<Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
							<Input
								placeholder={t("network.searchNodesToInclude")}
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

						{/* Quick Sorters for Pool Members */}
						<div className="flex items-center gap-1.5 shrink-0">
							<Tooltip>
								<TooltipTrigger asChild>
									<Button
										size="sm"
										variant="outline"
										className="h-8 text-xs gap-1"
										disabled={memberIds.length < 2}
										onClick={handleSortByLatency}
									>
										<Zap className="size-3 text-amber-500" />
										<span>{t("network.sortByLatency")}</span>
									</Button>
								</TooltipTrigger>
								<TooltipContent>{t("network.sortByLatency")}</TooltipContent>
							</Tooltip>

							<Tooltip>
								<TooltipTrigger asChild>
									<Button
										size="sm"
										variant="outline"
										className="h-8 text-xs gap-1"
										disabled={memberIds.length < 2}
										onClick={handleSortByName}
									>
										<ArrowUpDown className="size-3" />
										<span>{t("network.sortByName")}</span>
									</Button>
								</TooltipTrigger>
								<TooltipContent>{t("network.sortByName")}</TooltipContent>
							</Tooltip>
						</div>
					</div>

					{/* Master Checkbox & Fast Batch Selection Bar */}
					<div className="flex items-center justify-between rounded-lg border border-border/70 bg-muted/20 px-3 py-1.5 text-xs">
						<div className="flex items-center gap-2">
							<Checkbox
								id="master-pool-select"
								checked={isAllFilteredSelected || (isSomeFilteredSelected ? "indeterminate" : false)}
								onCheckedChange={handleToggleMasterSelect}
								disabled={filtered.length === 0}
							/>
							<label
								htmlFor="master-pool-select"
								className="cursor-pointer text-xs font-semibold select-none text-foreground"
							>
								{isAllFilteredSelected ? t("network.batchRemoveAll") : t("network.batchAddAll")}
								<span className="ml-1 text-[11px] font-normal text-muted-foreground font-mono">
									({selectedCountInFiltered}/{filtered.length})
								</span>
							</label>
						</div>

						<div className="flex items-center gap-1.5">
							<Button
								size="sm"
								variant="ghost"
								className="h-6 text-[11px] px-2 text-primary hover:text-primary hover:bg-primary/10"
								disabled={filtered.length === 0 || isAllFilteredSelected}
								onClick={handleAddAllFiltered}
							>
								{t("network.batchAddAll")}
							</Button>
							<span className="text-border">|</span>
							<Button
								size="sm"
								variant="ghost"
								className="h-6 text-[11px] px-2 text-muted-foreground hover:text-destructive hover:bg-destructive/10"
								disabled={selectedCountInFiltered === 0}
								onClick={handleRemoveAllFiltered}
							>
								{t("network.batchRemoveAll")}
							</Button>
							<span className="text-border">|</span>
							<Button
								size="sm"
								variant="ghost"
								className="h-6 text-[11px] px-2 text-muted-foreground hover:text-foreground"
								disabled={filtered.length === 0}
								onClick={handleInvertFiltered}
							>
								{t("network.batchInvertSelection")}
							</Button>
						</div>
					</div>

					{/* Nodes checklist with rich telemetry */}
					<div className="flex-1 min-h-48 max-h-[360px] overflow-y-auto rounded-lg border border-border/80 divide-y divide-border/50">
						{filtered.length === 0 ? (
							<div className="flex h-32 flex-col items-center justify-center p-4 text-center text-xs text-muted-foreground">
								<Search className="size-6 text-muted-foreground/40 mb-1" />
								<span>{t("network.noMatchingPools")}</span>
							</div>
						) : (
							filtered.map((node) => {
								const isMember = memberSet.has(node.id);
								const priorityIndex = memberIds.indexOf(node.id);
								const tone = getLatencyTone(node.probeLatencyMs);

								return (
									<div
										key={node.id}
										className={cn(
											"flex items-center justify-between p-2.5 transition-colors cursor-pointer select-none",
											isMember ? "bg-primary/5 hover:bg-primary/10" : "hover:bg-muted/40"
										)}
										onClick={() => toggleMember(node.id)}
									>
										<div className="flex items-center gap-2.5 min-w-0">
											<Checkbox
												checked={isMember}
												onCheckedChange={() => toggleMember(node.id)}
												onClick={(e) => e.stopPropagation()}
											/>
											<div className="min-w-0">
												<div className="flex items-center gap-1.5">
													<span className="text-xs font-semibold text-foreground truncate">
														{node.name}
													</span>
													{node.rotatingEndpoint && (
														<Badge variant="outline" className="text-[9px] px-1 py-0 border-blue-500/30 text-blue-500 bg-blue-500/5">
															{t("network.dynamicTunnel")}
														</Badge>
													)}
												</div>
												<div className="flex items-center gap-2 mt-0.5 text-[10px] text-muted-foreground font-mono">
													<span>{maskIP(node.exitIp || node.proxyDisplay || t("network.noIpDetected"))}</span>
													{node.probeStatus === "healthy" && node.probeLatencyMs > 0 && (
														<span className={cn("font-bold tabular-nums", tone.textClass)}>
															· {node.probeLatencyMs}ms
														</span>
													)}
												</div>
											</div>
										</div>

										{/* Priority order controls if member */}
										{isMember && (
											<div className="flex items-center gap-1.5 shrink-0" onClick={(e) => e.stopPropagation()}>
												<Badge
													variant="secondary"
													className="font-mono text-[10px] px-1.5 py-0 bg-primary/10 text-primary border-primary/20"
													title={t("network.priorityTooltip")}
												>
													{t("network.priorityBadge", { num: priorityIndex + 1 })}
												</Badge>

												<Button
													size="icon"
													variant="ghost"
													className="size-6 text-muted-foreground hover:text-foreground"
													disabled={priorityIndex === 0}
													onClick={() => movePriority(priorityIndex, "up")}
													title={t("network.movePriorityUp")}
												>
													<ArrowUp className="size-3" />
												</Button>

												<Button
													size="icon"
													variant="ghost"
													className="size-6 text-muted-foreground hover:text-foreground"
													disabled={priorityIndex === memberIds.length - 1}
													onClick={() => movePriority(priorityIndex, "down")}
													title={t("network.movePriorityDown")}
												>
													<ArrowDown className="size-3" />
												</Button>
											</div>
										)}
									</div>
								);
							})
						)}
					</div>
				</div>

				<DialogFooter className="pt-2 border-t border-border/60">
					<Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
						{t("common.cancel")}
					</Button>
					<Button size="sm" disabled={loading} onClick={handleSave}>
						{loading && <Spinner className="size-3.5 mr-1.5" />}
						<span>{t("common.save")} ({memberIds.length})</span>
					</Button>
				</DialogFooter>
			</OperationsDialogContent>
		</Dialog>
	);
}

/**
 * Pool Dispatch Analytics Modal (No overlap with X close icon!)
 */
function PoolStatsModal({
	pool,
	nodeMap,
	open,
	onOpenChange,
	onReset,
}: {
	pool: EgressPoolDTO;
	nodeMap: Map<string, EgressNodeDTO>;
	open: boolean;
	onOpenChange: (open: boolean) => void;
	onReset: () => void;
}) {
	const { t, i18n } = useTranslation();
	const statsQuery = useQuery({
		queryKey: ["egress-pool-stats", pool.id],
		queryFn: () => getEgressPoolStats(pool.id),
	});

	const [resetting, setResetting] = useState(false);

	const handleResetStats = async () => {
		setResetting(true);
		try {
			await resetEgressPoolStats(pool.id);
			void statsQuery.refetch();
			onReset();
			toast.success(t("network.resetSuccess"));
		} catch (err) {
			toast.error(err instanceof Error ? err.message : t("network.resetError"));
		} finally {
			setResetting(false);
		}
	};

	const items = statsQuery.data?.items ?? [];
	const totalSelections = items.reduce((acc, cur) => acc + cur.selections, 0);

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<OperationsDialogContent className="max-w-lg">
				{/* Clean Header without overlapping buttons */}
				<DialogHeader className="pr-8">
					<DialogTitle className="flex items-center gap-2">
						<BarChart3 className="size-5 text-blue-500" />
						<span>{t("network.dispatchStatsTitle", { name: pool.name })}</span>
					</DialogTitle>
				</DialogHeader>

				<div className="space-y-4 py-2">
					<p className="text-xs text-muted-foreground leading-normal">
						{t("networkPools.statsDescription")}
					</p>

					{/* Summary cards */}
					<div className="flex items-center justify-between rounded-lg border border-border/80 bg-muted/20 p-3">
						<div>
							<span className="text-[11px] text-muted-foreground block">{t("network.totalSelections")}</span>
							<span className="text-xl font-bold tabular-nums text-foreground">
								{totalSelections.toLocaleString()}
							</span>
						</div>
						<div className="text-right">
							<span className="text-[11px] text-muted-foreground block">{t("network.trackingSince")}</span>
							<span className="text-xs font-medium text-foreground">
								{statsQuery.data?.since ? formatTimeAgo(statsQuery.data.since, i18n.language) : t("network.startupTracking")}
							</span>
						</div>
					</div>

					{/* Member Selection Distribution */}
					<div className="space-y-2 max-h-[300px] overflow-y-auto">
						{items.length === 0 ? (
							<div className="flex h-24 items-center justify-center text-xs text-muted-foreground border border-dashed rounded-lg">
								{t("network.noDispatchEvents")}
							</div>
						) : (
							items.map((stat) => {
								const node = nodeMap.get(stat.nodeId);
								const pct = totalSelections > 0 ? Math.round((stat.selections / totalSelections) * 100) : 0;

								return (
									<div
										key={stat.nodeId}
										className="rounded-lg border border-border/60 bg-card/60 p-2.5"
									>
										<div className="flex items-center justify-between text-xs">
											<span className="font-semibold text-foreground">
												{node?.name || t("network.nodeFallbackName", { id: stat.nodeId })}
											</span>
											<span className="font-mono tabular-nums text-foreground font-bold">
												{stat.selections} ({pct}%)
											</span>
										</div>

										<div className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-muted">
											<div
												className="h-full bg-blue-500 rounded-full transition-all duration-300"
												style={{ width: `${pct}%` }}
											/>
										</div>

										<div className="mt-1.5 flex items-center justify-between text-[10px] text-muted-foreground">
											<span>{t("network.cumulativeFailures")}: {stat.failures}</span>
											<span>
												{t("network.lastPicked")}: {stat.lastSelectedAt ? formatTimeAgo(stat.lastSelectedAt, i18n.language) : t("network.neverPicked")}
											</span>
										</div>
									</div>
								);
							})
						)}
					</div>
				</div>

				{/* Footer with Reset action */}
				<DialogFooter className="flex items-center justify-start w-full pt-2">
					<Button
						size="sm"
						variant="outline"
						className="h-8 text-xs gap-1.5 text-muted-foreground hover:text-destructive hover:border-destructive/30"
						disabled={resetting || items.length === 0}
						onClick={handleResetStats}
					>
						<RotateCcw className="size-3.5" />
						<span>{t("network.resetCounters")}</span>
					</Button>
				</DialogFooter>
			</OperationsDialogContent>
		</Dialog>
	);
}