import {
	nodeCondition,
	nodeNeedsAttention,
} from "@/features/operations/operations-data";
import { useNow } from "@/features/guard/quality-hooks";
import {
	OperationsDialogContent as DialogContent,
	OperationsAlertDialogContent as AlertDialogContent,
} from "@/features/operations/operations-ui";
import {
	OperationsField,
	StatusPill,
	OperationsError,
} from "@/features/operations/operations-ui";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { BarChart3, Pencil, Plus, Settings2, Star, Trash2 } from "lucide-react";
import { useState } from "react";
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
import { OperationsButton as Button } from "@/features/operations/operations-ui";
import { Checkbox } from "@/components/ui/checkbox";
import {
	Dialog,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { MoreHorizontal } from "lucide-react";
import { Input } from "@/components/ui/input";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";

import { cn } from "@/shared/lib/cn";
import { formatCompactDateTime } from "@/shared/lib/format";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import {
	createEgressPool,
	deleteEgressPool,
	getEgressPoolStats,
	listAllEgressNodes,
	listEgressPools,
	resetEgressPoolStats,
	setEgressPoolMemberPriority,
	setEgressPoolMembers,
	testEgressNodes,
	updateEgressPool,
	type EgressNodeDTO,
	type EgressPoolDTO,
	type EgressPoolStrategy,
} from "@/features/settings/settings-api";

type PoolForm = {
	name: string;
	enabled: boolean;
	strategy: EgressPoolStrategy;
	fallbackMode: "none" | "pool" | "direct";
	fallbackPoolId: string;
};

const emptyForm: PoolForm = {
	name: "",
	enabled: true,
	strategy: "affinity",
	fallbackMode: "none",
	fallbackPoolId: "",
};

export function PoolsPanel() {
	const { t } = useTranslation();
	const now = useNow(30_000);
	const queryClient = useQueryClient();
	const [editing, setEditing] = useState<EgressPoolDTO | null>(null);
	const [form, setForm] = useState<PoolForm>(emptyForm);
	// 存 id 而不是池对象:对象是打开瞬间的快照,星标/成员变化后旧快照会把
	// 已清除的首选又带回来。渲染时从最新查询取池。
	const [managingId, setManagingId] = useState<string | null>(null);
	const [statsId, setStatsId] = useState<string | null>(null);
	// 删除是不可逆的分组级操作且可能被路由目标引用:与批量删节点一致走确认弹窗,
	// 而不是菜单一点就删。
	const [deletingId, setDeletingId] = useState<string | null>(null);

	const query = useQuery({
		queryKey: ["egress-pools"],
		queryFn: () => listEgressPools(),
		staleTime: 15_000,
		refetchOnWindowFocus: true,
	});
	const nodesQuery = useQuery({
		queryKey: ["egress-nodes", "pool-panel"],
		queryFn: () => listAllEgressNodes(),
		staleTime: 15_000,
		refetchOnWindowFocus: true,
	});
	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
		void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
	};
	const save = useMutation({
		mutationFn: () => {
			const input = {
				name: form.name.trim(),
				enabled: form.enabled,
				strategy: form.strategy,
				fallbackMode: form.fallbackMode,
				fallbackPoolId:
					form.fallbackMode === "pool" ? form.fallbackPoolId : undefined,
			};
			return editing?.id
				? updateEgressPool(editing.id, input)
				: createEgressPool(input);
		},
		onSuccess: () => {
			invalidate();
			setEditing(null);
			toast.success(t("settings.egress.pools.saved"));
		},
		onError: (error) =>
			toast.error(
				error instanceof Error
					? error.message
					: t("settings.egress.operationFailed"),
			),
	});
	const remove = useMutation({
		mutationFn: (id: string) => deleteEgressPool(id),
		onSuccess: () => {
			invalidate();
			setDeletingId(null);
			toast.success(t("settings.egress.pools.deleted"));
		},
		onError: (error) =>
			toast.error(
				error instanceof Error
					? error.message
					: t("settings.egress.operationFailed"),
			),
	});

	const pools = query.data ?? [];
	const nodes = nodesQuery.data?.items ?? [];
	// 自动调度是全量兜底池:入池只是分组,不减少兜底容量。
	const autoNodes = nodes.filter((node) => node.enabled);

	const strategyLabel = (strategy: EgressPoolStrategy) =>
		strategy === "random"
			? t("proxies.pools.strategyRandom")
			: strategy === "sticky"
				? t("proxies.pools.strategySticky")
				: strategy === "rotation"
					? t("proxies.pools.strategyRotation")
					: t("proxies.pools.strategyAffinity");

	// 当前出口,按策略取最准确的信号:
	// 节点轮询 = 持久化游标;首选优先 = 首选节点;其余 = 最近一次被调度选中的节点。
	const nodeName = (id?: string) => nodes.find((node) => node.id === id)?.name;
	const currentNodeName = (pool: EgressPoolDTO): string | undefined => {
		const ordered = pool.memberIds ?? [];
		const first = pool.preferredNodeId ?? ordered[0];
		if (pool.strategy === "rotation") {
			// 有游标显示游标;还没流量时显示即将开始的第一个(首选/顺序首个)。
			const id = pool.rotationCursorNodeId ?? first;
			if (id)
				return t("proxies.pools.cardCurrent", { name: nodeName(id) ?? id });
		}
		if (pool.strategy === "sticky" && first) {
			return t("proxies.pools.cardPreferred", {
				name: nodeName(first) ?? first,
			});
		}
		if (pool.lastSelectedNodeId) {
			return t("proxies.pools.cardCurrent", {
				name: nodeName(pool.lastSelectedNodeId) ?? pool.lastSelectedNodeId,
			});
		}
		return undefined;
	};
	const fallbackLabel = (pool: EgressPoolDTO) =>
		pool.fallbackMode === "pool"
			? (pool.fallbackPoolName ?? pool.fallbackPoolId ?? "")
			: pool.fallbackMode === "direct"
				? t("settings.egress.direct")
				: t("settings.egress.none");

	return (
		<section className="space-y-3">
			<div className="flex flex-wrap items-center justify-between gap-3">
				<div>
					<h2 className="text-base font-semibold">{t("ops.poolTitle")}</h2>
				</div>
				<Button
					type="button"
					size="sm"
					onClick={() => {
						setForm(emptyForm);
						setEditing({} as EgressPoolDTO);
					}}
				>
					<Plus />
					{t("settings.egress.pools.add")}
				</Button>
			</div>
			{query.isError || nodesQuery.isError ? (
				<OperationsError
					retry={() => {
						void query.refetch();
						void nodesQuery.refetch();
					}}
				/>
			) : query.isPending || nodesQuery.isPending ? (
				<div className="flex h-16 items-center justify-center text-xs text-muted-foreground">
					<Spinner />
				</div>
			) : pools.length === 0 && nodes.length === 0 ? (
				<div className="rounded-md border border-dashed px-3 py-6 text-center text-xs text-muted-foreground">
					{t("settings.egress.pools.empty")}
				</div>
			) : (
				<div className="ops-panel">
					{autoNodes.length > 0 ? (
						<PoolCard
							virtual
							name={t("proxies.pools.defaultPool")}
							members={autoNodes.length}
							healthy={
								autoNodes.filter((node) =>
									["ready", "dynamic"].includes(nodeCondition(node, now)),
								).length
							}
							meta={t("proxies.pools.defaultPoolFlow", {
								healthy: autoNodes.filter((node) =>
									["ready", "dynamic"].includes(nodeCondition(node, now)),
								).length,
							})}
						/>
					) : null}
					{pools.map((pool) => (
						<PoolCard
							key={pool.id}
							enabled={pool.enabled}
							name={pool.name}
							members={pool.memberCount}
							healthy={
								nodes.filter(
									(node) =>
										(pool.memberIds ?? []).includes(node.id) &&
										["ready", "dynamic"].includes(nodeCondition(node, now)),
								).length
							}
							quarantined={pool.quarantinedCount}
							meta={
								strategyLabel(pool.strategy) +
								" · " +
								t("settings.egress.pools.fallback", {
									mode: fallbackLabel(pool),
								})
							}
							current={currentNodeName(pool)}
							onManage={() => setManagingId(pool.id)}
							onStats={() => setStatsId(pool.id)}
							onEdit={() => {
								setForm({
									name: pool.name,
									enabled: pool.enabled,
									strategy: pool.strategy,
									fallbackMode: pool.fallbackMode,
									fallbackPoolId: pool.fallbackPoolId ?? "",
								});
								setEditing(pool);
							}}
							onDelete={() => setDeletingId(pool.id)}
						/>
					))}
				</div>
			)}

			<PoolMembersDialog
				key={`members-${managingId ?? "none"}`}
				pool={pools.find((item) => item.id === managingId) ?? null}
				onOpenChange={(open) => {
					if (!open) setManagingId(null);
				}}
				onSaved={invalidate}
			/>
			<PoolStatsDialog
				key={`stats-${statsId ?? "none"}`}
				pool={pools.find((item) => item.id === statsId) ?? null}
				onOpenChange={(open) => {
					if (!open) setStatsId(null);
				}}
			/>

			<AlertDialog
				open={deletingId !== null}
				onOpenChange={(open) => {
					if (!open && !remove.isPending) setDeletingId(null);
				}}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							{t("settings.egress.pools.deleteTitle")}
						</AlertDialogTitle>
						<AlertDialogDescription>
							{t("settings.egress.pools.deleteDescription")}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={remove.isPending}>
							{t("common.cancel")}
						</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={remove.isPending}
							onClick={(event) => {
								event.preventDefault();
								if (deletingId) remove.mutate(deletingId);
							}}
						>
							{remove.isPending ? null : null}
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>

			<Dialog
				open={editing !== null}
				onOpenChange={(open) => {
					if (!open) setEditing(null);
				}}
			>
				<DialogContent className="max-h-[92svh] overflow-y-auto sm:max-w-[680px]">
					<DialogHeader>
						<DialogTitle>
							{editing?.id
								? t("settings.egress.pools.editTitle")
								: t("settings.egress.pools.addTitle")}
						</DialogTitle>
					</DialogHeader>
					<div>
						<OperationsField
							label={t("settings.egress.name")}
							controlId="pool-name"
						>
							<Input
								id="pool-name"
								maxLength={160}
								value={form.name}
								onChange={(event) =>
									setForm({ ...form, name: event.target.value })
								}
							/>
						</OperationsField>
						<OperationsField
							label={t("proxies.pools.strategy")}
							controlId="pool-strategy"
							description={t(
								`proxies.pools.${{ affinity: "strategyAffinityHelp", random: "strategyRandomHelp", sticky: "strategyStickyHelp", rotation: "strategyRotationHelp", "least-used": "strategyLeastUsedHelp" }[form.strategy]}`,
							)}
						>
							<Select
								value={form.strategy}
								onValueChange={(strategy) =>
									setForm({ ...form, strategy: strategy as EgressPoolStrategy })
								}
							>
								<SelectTrigger id="pool-strategy">
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									{(
										[
											["affinity", "strategyAffinity"],
											["random", "strategyRandom"],
											["sticky", "strategySticky"],
											["rotation", "strategyRotation"],
											["least-used", "strategyLeastUsed"],
										] as const
									).map(([value, key]) => (
										<SelectItem value={value} key={value}>
											{t(`proxies.pools.${key}`)}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</OperationsField>
						<OperationsField
							label={t("settings.egress.pools.fallbackLabel")}
							controlId="pool-fallback"
							description={t("settings.egress.pools.fallbackHelp")}
						>
							<div className="space-y-2">
								<Select
									value={form.fallbackMode}
									onValueChange={(mode) =>
										setForm({
											...form,
											fallbackMode: mode as PoolForm["fallbackMode"],
										})
									}
								>
									<SelectTrigger id="pool-fallback">
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										{[
											["none", "fallbackNone"],
											["direct", "fallbackDirect"],
											["pool", "fallbackPool"],
										].map(([value, key]) => (
											<SelectItem value={value} key={value}>
												{t(`settings.egress.pools.${key}`)}
											</SelectItem>
										))}
									</SelectContent>
								</Select>
								{form.fallbackMode === "pool" && (
									<Select
										value={form.fallbackPoolId}
										onValueChange={(fallbackPoolId) =>
											setForm({ ...form, fallbackPoolId })
										}
									>
										<SelectTrigger
											aria-label={t("settings.egress.pools.selectFallback")}
										>
											<SelectValue
												placeholder={t("settings.egress.pools.selectFallback")}
											/>
										</SelectTrigger>
										<SelectContent>
											{pools
												.filter((pool) => pool.id !== editing?.id)
												.map((pool) => (
													<SelectItem key={pool.id} value={pool.id}>
														{pool.name}
													</SelectItem>
												))}
										</SelectContent>
									</Select>
								)}
							</div>
						</OperationsField>
						<OperationsField
							label={t("settings.egress.enabled")}
							controlId="pool-enabled"
						>
							<Switch
								id="pool-enabled"
								checked={form.enabled}
								onCheckedChange={(enabled) => setForm({ ...form, enabled })}
							/>
						</OperationsField>
					</div>
					<DialogFooter>
						<Button
							type="button"
							variant="secondary"
							size="sm"
							onClick={() => setEditing(null)}
						>
							{t("common.cancel")}
						</Button>
						<Button
							type="button"
							size="sm"
							disabled={
								!form.name.trim() ||
								save.isPending ||
								(form.fallbackMode === "pool" && !form.fallbackPoolId)
							}
							onClick={() => save.mutate()}
						>
							{save.isPending ? <Spinner /> : null}
							{t("common.save")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</section>
	);
}

function PoolStatsDialog({
	pool,
	onOpenChange,
}: {
	pool: EgressPoolDTO | null;
	onOpenChange: (open: boolean) => void;
}) {
	const { t, i18n } = useTranslation();
	const queryClient = useQueryClient();
	const statsQuery = useQuery({
		queryKey: ["egress-pool-stats", pool?.id ?? ""],
		queryFn: () => getEgressPoolStats(pool!.id),
		enabled: pool !== null,
		refetchInterval: pool !== null ? 3000 : false,
	});
	const nodesQuery = useQuery({
		queryKey: ["egress-nodes", "pool-stats"],
		queryFn: () => listAllEgressNodes(),
		enabled: pool !== null,
		staleTime: 10_000,
	});
	const reset = useMutation({
		mutationFn: () => resetEgressPoolStats(pool!.id),
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: ["egress-pool-stats"] });
			toast.success(t("proxies.pools.statsResetDone"));
		},
		onError: (error) =>
			toast.error(
				error instanceof Error
					? error.message
					: t("settings.egress.operationFailed"),
			),
	});
	const testMembers = useMutation({
		mutationFn: () => testEgressNodes(pool!.memberIds ?? []),
		onSuccess: (result) => {
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
			toast.success(
				t("proxies.pools.statsTestDone", {
					healthy: result.healthy,
					unhealthy: result.unhealthy,
				}),
			);
		},
		onError: (error) =>
			toast.error(
				error instanceof Error
					? error.message
					: t("settings.egress.operationFailed"),
			),
	});
	if (!pool) return null;
	const stats = new Map(
		(statsQuery.data?.items ?? []).map((item) => [item.nodeId, item]),
	);
	const members = (nodesQuery.data?.items ?? []).filter((node) =>
		(pool.memberIds ?? []).includes(node.id),
	);
	const rows = [
		...members.map((node) => ({
			id: node.id,
			name: node.name,
			status: node.probeStatus,
			latency: node.probeLatencyMs,
			stat: stats.get(node.id),
		})),
		...[...stats.entries()]
			.filter(([id]) => !(pool.memberIds ?? []).includes(id))
			.map(([id, stat]) => ({
				id,
				name: t("proxies.pools.statsRemovedNode", { id }),
				status: "unknown" as const,
				latency: 0,
				stat,
			})),
	].sort((a, b) => (b.stat?.selections ?? 0) - (a.stat?.selections ?? 0));
	const currentNode =
		pool.strategy === "rotation" && pool.rotationCursorNodeId
			? pool.rotationCursorNodeId
			: pool.strategy === "sticky"
				? (pool.preferredNodeId ?? (pool.memberIds ?? [])[0])
				: undefined;
	return (
		<Dialog open onOpenChange={onOpenChange}>
			<DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col overflow-hidden sm:max-w-2xl">
				<DialogHeader>
					<DialogTitle>
						{t("proxies.pools.statsTitle", { name: pool.name })}
					</DialogTitle>
				</DialogHeader>
				{statsQuery.isPending ? (
					<div className="flex h-20 items-center justify-center">
						<Spinner />
					</div>
				) : rows.length === 0 ? (
					<p className="py-6 text-center text-xs text-muted-foreground">
						{t("proxies.pools.statsEmpty")}
					</p>
				) : (
					<Table className="table-fixed" viewportRows={10} rowHeight={40}>
						<TableHeader>
							<TableRow className="hover:bg-transparent">
								<TableHead className="w-[132px] text-center">
									{t("settings.egress.name")}
								</TableHead>
								<TableHead className="w-[72px] text-center">
									{t("proxies.pools.statsStatus")}
								</TableHead>
								<TableHead className="w-[64px] text-center">
									{t("proxies.pools.statsSelections")}
								</TableHead>
								<TableHead className="w-[64px] text-center">
									{t("proxies.pools.statsFailures")}
								</TableHead>
								<TableHead className="w-[84px] whitespace-nowrap text-center">
									{t("proxies.pools.statsLatency")}
								</TableHead>
								<TableHead className="w-[172px] whitespace-nowrap text-center">
									{t("proxies.pools.statsLastSelected")}
								</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{rows.map((row) => (
								<TableRow key={row.id} className="h-10">
									<TableCell
										className="truncate text-center text-xs font-medium"
										title={row.name}
									>
										{row.name}
										{row.id === currentNode ? (
											<span className="ml-1.5 text-[10px] text-emerald-600">
												{t("proxies.pools.statsCurrentBadge")}
											</span>
										) : null}
									</TableCell>
									<TableCell
										className={cn(
											"text-center text-[11px]",
											row.status === "healthy"
												? "text-emerald-600"
												: row.status === "unhealthy"
													? "text-destructive"
													: "text-muted-foreground",
										)}
									>
										{row.status === "healthy"
											? t("settings.egress.healthy")
											: row.status === "unhealthy"
												? t("settings.egress.unhealthy")
												: t("settings.egress.notTested")}
									</TableCell>
									<TableCell className="text-center text-xs tabular-nums">
										{row.stat?.selections ?? 0}
									</TableCell>
									<TableCell
										className={cn(
											"text-center text-xs tabular-nums",
											(row.stat?.failures ?? 0) > 0 && "text-destructive",
										)}
									>
										{row.stat?.failures ?? 0}
									</TableCell>
									<TableCell className="whitespace-nowrap px-1 text-center text-[11px] tabular-nums text-muted-foreground">
										{row.latency > 0
											? t("proxies.pools.statsLatencyValue", {
													ms: row.latency,
												})
											: "-"}
									</TableCell>
									<TableCell className="whitespace-nowrap text-center text-xs tabular-nums text-muted-foreground">
										{row.stat?.lastSelectedAt
											? formatCompactDateTime(
													row.stat.lastSelectedAt,
													i18n.language,
												)
											: t("settings.egress.never")}
									</TableCell>
								</TableRow>
							))}
						</TableBody>
					</Table>
				)}
				<DialogFooter className="items-center sm:justify-between">
					<span className="min-w-0 truncate text-xs tabular-nums text-muted-foreground">
						{t("proxies.pools.statsTotals", {
							selections: rows.reduce(
								(sum, row) => sum + (row.stat?.selections ?? 0),
								0,
							),
							failures: rows.reduce(
								(sum, row) => sum + (row.stat?.failures ?? 0),
								0,
							),
						})}
					</span>
					<div className="flex items-center gap-2">
						{(pool.memberIds ?? []).length > 0 ? (
							<Button
								type="button"
								size="sm"
								variant="secondary"
								disabled={testMembers.isPending}
								onClick={() => testMembers.mutate()}
							>
								{testMembers.isPending ? <Spinner /> : null}
								{t("proxies.pools.statsTest")}
							</Button>
						) : null}
						<Button
							type="button"
							size="sm"
							variant="secondary"
							disabled={reset.isPending}
							onClick={() => reset.mutate()}
						>
							{t("proxies.pools.statsReset")}
						</Button>
						<Button
							type="button"
							size="sm"
							variant="secondary"
							onClick={() => onOpenChange(false)}
						>
							{t("common.close")}
						</Button>
					</div>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}

/** Pool-side node management: checkbox list with search + selected-only
 *  filter — with dozens of nodes, finding the ones to toggle is the hard
 *  part, not the toggling. One save applies every checked/unchecked row. */
function PoolMembersDialog({
	pool,
	onOpenChange,
	onSaved,
}: {
	pool: EgressPoolDTO | null;
	onOpenChange: (open: boolean) => void;
	onSaved: () => void;
}) {
	const { t } = useTranslation();
	const now = useNow(30_000);
	const [draft, setDraft] = useState<Set<string>>(new Set());
	const [loadedFor, setLoadedFor] = useState<string>("");
	const [search, setSearch] = useState("");
	const [onlySelected, setOnlySelected] = useState(false);
	// 首选的本地镜像:点击后立即变实心,不等父级列表刷新。
	const [preferredId, setPreferredId] = useState<string | undefined>(undefined);
	const nodesQuery = useQuery({
		queryKey: ["egress-nodes", "pool-nodes", pool?.id ?? ""],
		queryFn: () => listAllEgressNodes(),
		enabled: pool !== null,
		staleTime: 10_000,
	});
	const all: EgressNodeDTO[] = nodesQuery.data?.items ?? [];
	if (pool && loadedFor !== pool.id) {
		setLoadedFor(pool.id);
		setDraft(new Set(pool.memberIds ?? []));
		setPreferredId(pool.preferredNodeId);
	}
	// 一次保存全部生效:成员与首选一起提交。星标点击只改本地草稿,不发请求。
	const apply = useMutation({
		mutationFn: async () => {
			await setEgressPoolMembers(pool!.id, [...draft]);
			if (preferredId && draft.has(preferredId)) {
				await setEgressPoolMemberPriority(pool!.id, preferredId, 1);
			} else if (pool?.preferredNodeId) {
				await setEgressPoolMemberPriority(pool!.id, pool.preferredNodeId, 0);
			}
		},
		onSuccess: () => {
			onSaved();
			onOpenChange(false);
			toast.success(t("proxies.pools.nodesSaved"));
		},
		onError: (error) =>
			toast.error(
				error instanceof Error
					? error.message
					: t("settings.egress.operationFailed"),
			),
	});
	if (!pool) return null;
	const toggle = (id: string, checked: boolean) =>
		setDraft((current) => {
			const next = new Set(current);
			if (checked) next.add(id);
			else next.delete(id);
			return next;
		});
	const needle = search.trim().toLocaleLowerCase();
	const visible = all.filter((node) => {
		if (onlySelected && !draft.has(node.id)) return false;
		if (!needle) return true;
		return (
			node.name.toLocaleLowerCase().includes(needle) ||
			(node.exitIp ?? "").includes(needle) ||
			(node.sourceName ?? "").toLocaleLowerCase().includes(needle)
		);
	});
	// 批量勾选：节点多时逐个点是不现实的。全选可见跟随当前搜索/过滤结果。
	const selectAllVisible = () =>
		setDraft((current) => {
			const next = new Set(current);
			for (const node of visible) next.add(node.id);
			return next;
		});
	return (
		<Dialog open onOpenChange={onOpenChange}>
			<DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col overflow-hidden sm:max-w-lg">
				<DialogHeader>
					<DialogTitle>
						{t("proxies.pools.nodesTitle", { name: pool.name })}
					</DialogTitle>
				</DialogHeader>
				{nodesQuery.isPending ? (
					<div className="flex h-20 items-center justify-center">
						<Spinner />
					</div>
				) : (
					<>
						<div className="flex items-center gap-2">
							<Input
								className="h-8 flex-1 text-xs"
								value={search}
								onChange={(event) => setSearch(event.target.value)}
								placeholder={t("settings.egress.search")}
								aria-label={t("settings.egress.search")}
							/>
							<Button
								type="button"
								size="sm"
								variant={onlySelected ? "default" : "secondary"}
								onClick={() => setOnlySelected((current) => !current)}
							>
								{t("proxies.pools.onlySelected")}
							</Button>
						</div>
						<div className="flex items-center justify-between">
							<span className="text-xs tabular-nums text-muted-foreground">
								{t("proxies.pools.selectedCount", {
									count: draft.size,
									total: all.length,
								})}
							</span>
							<div className="flex items-center gap-1.5">
								<Button
									type="button"
									size="sm"
									variant="secondary"
									disabled={
										onlySelected ||
										visible.length === 0 ||
										visible.every((node) => draft.has(node.id))
									}
									onClick={selectAllVisible}
								>
									{t("proxies.pools.selectAllVisible")}
								</Button>
								<Button
									type="button"
									size="sm"
									variant="secondary"
									disabled={draft.size === 0}
									onClick={() => setDraft(new Set())}
								>
									{t("proxies.pools.clearSelection")}
								</Button>
							</div>
						</div>
						<div className="max-h-72 min-h-0 flex-1 space-y-1 overflow-y-auto overscroll-contain rounded-md border p-1.5">
							{all.length === 0 ? (
								<p className="p-4 text-center text-xs text-muted-foreground">
									{t("proxies.pools.noEligibleNodes")}
								</p>
							) : null}
							{all.length > 0 && visible.length === 0 ? (
								<p className="p-4 text-center text-xs text-muted-foreground">
									{t("settings.egress.noMatches")}
								</p>
							) : null}
							{visible.map((node) => {
								const isPreferred = preferredId === node.id;
								return (
									// 不用 label 包裹:label 会把点击转发给内部第一个可标记控件,
									// checkbox/星标被二次触发,勾选时好时坏。改为显式点击区。
									<div
										key={node.id}
										className="flex min-h-9 cursor-pointer items-center gap-2.5 rounded-md px-2 hover:bg-muted/45"
										onClick={() => toggle(node.id, !draft.has(node.id))}
									>
										<Checkbox
											checked={draft.has(node.id)}
											onCheckedChange={(checked) =>
												toggle(node.id, checked === true)
											}
											aria-label={node.name}
											onClick={(event) => event.stopPropagation()}
										/>
										{draft.has(node.id) ? (
											<button
												type="button"
												className={cn(
													"shrink-0",
													isPreferred
														? "text-amber-500"
														: "text-muted-foreground/40 hover:text-foreground",
												)}
												aria-label={
													isPreferred
														? t("proxies.pools.preferClear")
														: t("proxies.pools.preferSet")
												}
												onClick={(event) => {
													event.stopPropagation();
													setPreferredId(isPreferred ? undefined : node.id);
												}}
											>
												<Star
													className={cn(
														"size-3.5",
														isPreferred && "fill-current",
													)}
												/>
											</button>
										) : null}
										<span className="min-w-0 flex-1 truncate text-xs font-medium">
											{node.name}
										</span>
										<StatusPill
											tone={
												nodeNeedsAttention(node, now)
													? "warn"
													: ["ready", "dynamic"].includes(
																nodeCondition(node, now),
														  )
														? "good"
														: "neutral"
											}
										>
											{t(`ops.condition.${nodeCondition(node, now)}`)}
										</StatusPill>
										{node.exitIp ? (
											<span className="shrink-0 text-[10px] tabular-nums text-muted-foreground">
												{node.exitIp}
											</span>
										) : null}
										{node.sourceName ? (
											<Badge
												variant="outline"
												className="shrink-0 text-[10px] text-muted-foreground"
											>
												{node.sourceName}
											</Badge>
										) : null}
									</div>
								);
							})}
						</div>
					</>
				)}
				<DialogFooter>
					<Button
						type="button"
						variant="secondary"
						size="sm"
						onClick={() => onOpenChange(false)}
					>
						{t("common.cancel")}
					</Button>
					<Button
						type="button"
						size="sm"
						disabled={apply.isPending || nodesQuery.isPending}
						onClick={() => apply.mutate()}
					>
						{apply.isPending ? <Spinner /> : null}
						{t("common.save")}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}

/** Comparable pool rows use the same candidate eligibility as the node inventory. */
function PoolCard({
	virtual,
	enabled = true,
	name,
	members,
	healthy,
	quarantined = 0,
	meta,
	current,
	onManage,
	onStats,
	onEdit,
	onDelete,
}: {
	virtual?: boolean;
	enabled?: boolean;
	name: string;
	members: number;
	healthy: number;
	current?: string;
	quarantined?: number;
	meta: string;
	onManage?: () => void;
	onStats?: () => void;
	onEdit?: () => void;
	onDelete?: () => void;
}) {
	const { t } = useTranslation();
	return (
		<div className="ops-pool-row">
			<div className="min-w-0">
				<div className="flex flex-wrap items-center gap-2">
					<h3 className="truncate text-sm font-semibold" title={name}>
						{name}
					</h3>
					<StatusPill
						tone={!enabled ? "neutral" : healthy > 0 ? "good" : "warn"}
					>
						{!enabled
							? t("ops.disabled")
							: healthy > 0
								? t("common.enable")
								: t("ops.attention")}
					</StatusPill>
					{virtual && (
						<span className="text-[11px] text-muted-foreground">
							{t("proxies.pools.virtual")}
						</span>
					)}
				</div>
				{current && (
					<p className="mt-2 truncate text-xs text-muted-foreground">
						{current}
					</p>
				)}
			</div>
			<div>
				<p className="text-[11px] text-muted-foreground">
					{t("ops.poolAvailability")}
				</p>
				<p className="mt-2 text-sm tabular-nums">
					<strong className="font-semibold">{healthy}</strong>
					<span className="text-muted-foreground">
						{" "}
						/ {members} · {t("ops.poolHealthy")}
					</span>
				</p>
				{quarantined > 0 && (
					<p className="mt-1 text-[11px] ops-text-warn">
						{quarantined} {t("ops.poolLimited")}
					</p>
				)}
			</div>
			<div>
				<p className="text-[11px] text-muted-foreground">
					{t("ops.poolStrategy")}
				</p>
				<p className="mt-2 text-xs leading-5">{meta}</p>
			</div>
			<div className="flex items-center gap-1">
				{onManage && (
					<Button variant="outline" size="sm" onClick={onManage}>
						<Settings2 className="size-3.5" />
						{t("ops.poolManage")}
					</Button>
				)}
				{(onStats || onEdit || onDelete) && (
					<DropdownMenu>
						<DropdownMenuTrigger asChild>
							<Button
								type="button"
								variant="ghost"
								size="icon"
								className="size-8"
								aria-label={t("common.actions")}
							>
								<MoreHorizontal className="size-4" />
							</Button>
						</DropdownMenuTrigger>
						<DropdownMenuContent align="end">
							{onStats && (
								<DropdownMenuItem onClick={onStats}>
									<BarChart3 />
									{t("proxies.pools.statsAction")}
								</DropdownMenuItem>
							)}
							{onEdit && (
								<DropdownMenuItem onClick={onEdit}>
									<Pencil />
									{t("common.edit")}
								</DropdownMenuItem>
							)}
							{onDelete && (
								<>
									<DropdownMenuSeparator />
									<DropdownMenuItem
										className="text-destructive focus:text-destructive"
										onClick={onDelete}
									>
										<Trash2 />
										{t("common.delete")}
									</DropdownMenuItem>
								</>
							)}
						</DropdownMenuContent>
					</DropdownMenu>
				)}
			</div>
		</div>
	);
}
