import { NetworkField as OperationsField } from "./network-ui";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { NetworkText, NetworkTooltip } from "./network-ui";
import {
	NetworkDialogContent as DialogContent,
	NetworkDialogHeader as DialogHeader,
	NetworkDialogFooter as DialogFooter,
	NetworkSelect,
} from "./network-ui";
import { nodeCondition, nodeNeedsAttention } from "@/features/operations/operations-data";
import { useNow } from "@/features/guard/quality-hooks";
import { AlertDialogContent } from "@/components/ui/alert-dialog";
import { StatusPill, OperationsError } from "@/features/operations/operations-ui";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
	Ban,
	ArrowRight,
	BarChart3,
	MoreHorizontal,
	Gauge,
	Globe,
	Layers2,
	Plus,
	Repeat2,
	Shuffle,
	Star,
	Trash2,
	UserRound,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
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
import { NetworkButton as Button } from "./network-ui";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import {
	DropdownMenu,
	DropdownMenuTrigger,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu";
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
import "./pools-panel.css";

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

export function PoolsPanel({
	focusPoolId,
	onViewNodes,
}: {
	focusPoolId?: string;
	onViewNodes?: () => void;
} = {}) {
	const { t } = useTranslation();
	const now = useNow(30_000);
	const queryClient = useQueryClient();
	const [editing, setEditingState] = useState<EgressPoolDTO | null>(null);
	const editorRevision = useRef(0);
	function setEditing(value: EgressPoolDTO | null) {
		editorRevision.current++;
		setEditingState(value);
	}
	const [form, setForm] = useState<PoolForm>(emptyForm);
	// 存 id 而不是池对象:对象是打开瞬间的快照,星标/成员变化后旧快照会把
	// 已清除的首选又带回来。渲染时从最新查询取池。
	const [selectedId, setSelectedId] = useState<string | null>(focusPoolId ?? null);
	const [previousFocusId, setPreviousFocusId] = useState(focusPoolId);
	if (focusPoolId !== previousFocusId) {
		setPreviousFocusId(focusPoolId);
		if (focusPoolId) setSelectedId(focusPoolId);
	}
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
		mutationFn: ({ form, id }: { form: PoolForm; id?: string; revision: number; }) => {
			const input = {
				name: form.name.trim(),
				enabled: form.enabled,
				strategy: form.strategy,
				fallbackMode: form.fallbackMode,
				fallbackPoolId: form.fallbackMode === "pool" ? form.fallbackPoolId : undefined,
			};
			return id ? updateEgressPool(id, input) : createEgressPool(input);
		},
		onSuccess: (_, submission) => {
			invalidate();
			if (editorRevision.current === submission.revision) setEditing(null);
			toast.success(t("settings.egress.pools.saved"));
		},
		onError: (error) =>
			toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
	});
	const remove = useMutation({
		mutationFn: (id: string) => deleteEgressPool(id),
		onSuccess: () => {
			invalidate();
			setDeletingId(null);
			toast.success(t("settings.egress.pools.deleted"));
		},
		onError: (error) =>
			toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
	});

	const pools = query.data ?? [];
	const nodes = nodesQuery.data?.items ?? [];
	// 自动调度是全量兜底池:入池只是分组,不减少兜底容量。
	const autoNodes = nodes.filter((node) => node.enabled);

	const strategyLabel = (strategy: EgressPoolStrategy) =>
		strategy === "least-used"
			? t("proxies.pools.strategyLeastUsed")
			: strategy === "random"
				? t("proxies.pools.strategyRandom")
				: strategy === "sticky"
					? t("proxies.pools.strategySticky")
					: strategy === "rotation"
						? t("proxies.pools.strategyRotation")
						: t("proxies.pools.strategyAffinity");

	const nodeName = (id: string) => nodes.find((node) => node.id === id)?.name ?? id;
	const poolSignal = (pool: EgressPoolDTO) => {
		if (!pool.enabled) return undefined;
		const first = pool.preferredNodeId ?? pool.memberIds[0];
		if (pool.strategy === "rotation") {
			const id = pool.rotationCursorNodeId ?? first;
			if (id) return { label: t(pool.rotationCursorNodeId ? "networkPools.rotationPosition" : "networkPools.startsWith"), name: nodeName(id) };
		}
		if (pool.strategy === "sticky" && first)
			return { label: t("networkPools.preferredExit"), name: nodeName(first) };
		if (pool.lastSelectedNodeId)
			return { label: t("networkPools.lastSelected"), name: nodeName(pool.lastSelectedNodeId) };
		return undefined;
	};
	const fallbackLabel = (pool: EgressPoolDTO) =>
		pool.fallbackMode === "pool"
			? (pool.fallbackPoolName ?? pool.fallbackPoolId ?? "")
			: pool.fallbackMode === "direct"
				? t("settings.egress.direct")
				: t("settings.egress.none");

	const selectedPool = selectedId === "auto" ? null : pools.find((pool) => pool.id === selectedId);
	const members = selectedPool
		? nodes.filter((node) => (selectedPool.memberIds ?? []).includes(node.id))
		: autoNodes;
	const ready = members.filter((node) =>
		["ready", "dynamic"].includes(nodeCondition(node, now)),
	).length;
	const editPool = (pool: EgressPoolDTO) => {
		setForm({
			name: pool.name,
			enabled: pool.enabled,
			strategy: pool.strategy,
			fallbackMode: pool.fallbackMode,
			fallbackPoolId: pool.fallbackPoolId ?? "",
		});
		setEditing(pool);
	};
	return (
		<section className="npool-page">
			<header className="npool-heading">
				<div>
					<h2>{t("networkPools.title")}</h2>
					<p>{t("networkPools.description")}</p>
				</div>
				<Button size="sm" onClick={() => { setForm(emptyForm); setEditing({} as EgressPoolDTO); }}>
					<Plus />{t("settings.egress.pools.add")}
				</Button>
			</header>
			{query.isError || nodesQuery.isError ? (
				<OperationsError retry={() => { void query.refetch(); void nodesQuery.refetch(); }} />
			) : query.isPending || nodesQuery.isPending ? (
				<div className="npool-loading" role="status">
					<Spinner />
					<span>{t("common.loading")}</span>
				</div>
			) : (
				<>
					{pools.length === 0 ? (
						<div className="npool-empty">
							<Layers2 aria-hidden="true" />
							<h3>{t("networkPools.emptyTitle")}</h3>
							<p>{t("networkPools.emptyDescription")}</p>
							<Button size="sm" onClick={() => { setForm(emptyForm); setEditing({} as EgressPoolDTO); }}>
								<Plus />{t("settings.egress.pools.add")}
							</Button>
						</div>
					) : (
						<div className="npool-grid">
							{pools.map((pool) => {
								const memberIds = new Set(pool.memberIds);
								const available = pool.enabled ? nodes.filter((node) => memberIds.has(node.id) && ["ready", "dynamic"].includes(nodeCondition(node, now))).length : 0;
								const total = memberIds.size;
								const signal = poolSignal(pool);
								return (
									<article className="npool-card" key={pool.id} data-enabled={pool.enabled} aria-labelledby={`npool-name-${pool.id}`}>
										<div className="npool-card-body">
											<header className="npool-card-heading">
												<h3 id={`npool-name-${pool.id}`}>
													<button type="button" onClick={() => editPool(pool)}>
														<NetworkText>{pool.name}</NetworkText>
													</button>
												</h3>
												<span className={cn("npool-badge", pool.enabled && "npool-badge-enabled")}>{t(pool.enabled ? "common.enabled" : "common.disabled")}</span>
											</header>
											<div className="npool-capacity">
												<strong>{available}</strong>
												<span>/ {total} {t("networkPools.availableMembers")}</span>
											</div>
											<div className="npool-meter" role="meter" aria-label={t("networkPools.capacityLabel", { name: pool.name })} aria-valuemin={0} aria-valuemax={Math.max(1, total)} aria-valuenow={available} aria-valuetext={t("networkPools.capacityValue", { available, total })}>
												<span style={{ width: `${total ? available / total * 100 : 0}%` }} />
											</div>
											<dl className="npool-details">
												<div>
													<dt>{t("proxies.pools.strategy")}</dt>
													<dd>{strategyLabel(pool.strategy)}</dd>
												</div>
												<div>
													<dt>{t("networkPools.fallbackPath")}</dt>
													<dd>{pool.fallbackMode === "pool" && pool.fallbackPoolId ? <button className="npool-resource-link" type="button" onClick={() => setSelectedId(pool.fallbackPoolId!)}>
														<NetworkText>{fallbackLabel(pool)}</NetworkText>
														<ArrowRight aria-hidden="true" />
													</button> : fallbackLabel(pool)}</dd>
												</div>
												<div>
													<dt>{signal?.label ?? t("networkPools.lastSelected")}</dt>
													<dd>
														<NetworkText>{signal?.name ?? "—"}</NetworkText>
													</dd>
												</div>
											</dl>
										</div>
										<footer className="npool-card-actions">
											<Button variant="outline" size="sm" onClick={() => setSelectedId(pool.id)}>{t("networkPools.manageMembers", { count: total })}</Button>
											<div>
												<Button variant="ghost" size="sm" onClick={() => editPool(pool)}>{t("common.edit")}</Button>
												<DropdownMenu>
													<DropdownMenuTrigger asChild>
														<Button variant="ghost" size="icon" aria-label={t("networkPools.poolActions", { name: pool.name })}>
															<MoreHorizontal />
														</Button>
													</DropdownMenuTrigger>
													<DropdownMenuContent align="end">
														<DropdownMenuItem onClick={() => setStatsId(pool.id)}>
															<BarChart3 />{t("proxies.pools.statsAction")}</DropdownMenuItem>
														<DropdownMenuSeparator />
														<DropdownMenuItem className="text-destructive" onClick={() => setDeletingId(pool.id)}>
															<Trash2 />{t("common.delete")}</DropdownMenuItem>
													</DropdownMenuContent>
												</DropdownMenu>
											</div>
										</footer>
									</article>
								);
							})}
						</div>
					)}
					<section className="npool-automatic">
						<Globe aria-hidden="true" />
						<div>
							<h3>{t("proxies.pools.defaultPool")}</h3>
							<p>{t("networkPools.automaticDescription", { count: autoNodes.length })}</p>
						</div>
						<Button variant="ghost" size="sm" onClick={() => onViewNodes ? onViewNodes() : setSelectedId("auto")}>{t("networkPools.viewCandidates")}<ArrowRight />
						</Button>
					</section>
				</>
			)}

			<Dialog open={selectedId !== null && (selectedId === "auto" || Boolean(selectedPool))} onOpenChange={(open) => { if (!open) setSelectedId(null); }}>
				<DialogContent className="npool-members-dialog" aria-describedby="npool-members-description">
					<DialogHeader className="npool-members-heading">
						<DialogTitle>{selectedPool?.name ?? t("proxies.pools.defaultPool")}</DialogTitle>
						<DialogDescription id="npool-members-description">{selectedPool ? t("networkPools.membersDescription") : t("networkPools.automaticDescription", { count: autoNodes.length })}</DialogDescription>
					</DialogHeader>
					{selectedPool ? (
						<PoolMembersEditor key={selectedPool.id} pool={selectedPool} onSaved={invalidate} onClose={() => setSelectedId(null)} />
					) : (
						<>
							<div className="npool-automatic-members">
								<p className="npool-dialog-summary">{t("networkPools.capacityValue", { available: ready, total: members.length })}</p>
								<Table className="min-w-[500px] table-fixed" viewportRows={8} rowHeight={64}>
									<TableHeader>
										<TableRow>
											<TableHead>{t("ops.nodeName")}</TableHead>
											<TableHead>{t("ops.path")}</TableHead>
											<TableHead className="w-32">{t("ops.nodeState")}</TableHead>
										</TableRow>
									</TableHeader>
									<VirtualTableBody items={autoNodes} colSpan={3} rowHeight={64} renderRow={(node) => <TableRow key={node.id} className="h-16">
										<TableCell className="text-xs">
											<NetworkText>{node.name}</NetworkText>
										</TableCell>
										<TableCell className="text-xs font-mono">{node.exitIp || "—"}</TableCell>
										<TableCell>
											<StatusPill tone={nodeNeedsAttention(node, now) ? "warn" : ["ready", "dynamic"].includes(nodeCondition(node, now)) ? "good" : "neutral"}>{t(`ops.condition.${nodeCondition(node, now)}`)}</StatusPill>
										</TableCell>
									</TableRow>} />
								</Table>
								{autoNodes.length === 0 && <p className="npool-dialog-summary">{t("proxies.pools.noEligibleNodes")}</p>}
							</div>
							<DialogFooter className="npool-members-footer">
								<Button variant="secondary" size="sm" onClick={() => setSelectedId(null)}>{t("common.close")}</Button>
							</DialogFooter>
						</>
					)}
				</DialogContent>
			</Dialog>

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
						<AlertDialogTitle>{t("settings.egress.pools.deleteTitle")}</AlertDialogTitle>
						<AlertDialogDescription>
							{t("settings.egress.pools.deleteDescription")}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={remove.isPending}>{t("common.cancel")}</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={remove.isPending}
							onClick={(event) => {
								event.preventDefault();
								if (deletingId) remove.mutate(deletingId);
							}}
						>
							{remove.isPending && <Spinner />}
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
				<DialogContent layout="editor" aria-describedby={undefined}>
					<DialogHeader>
						<DialogTitle>
							<Layers2 aria-hidden="true" />
							{editing?.id
								? t("settings.egress.pools.editTitle")
								: t("settings.egress.pools.addTitle")}
						</DialogTitle>
					</DialogHeader>
					<form
						className="net-editor-form"
						onSubmit={(event) => {
							event.preventDefault();
							if (
								!save.isPending &&
								form.name.trim() &&
								(form.fallbackMode !== "pool" || form.fallbackPoolId)
							)
								save.mutate({ form, id: editing?.id, revision: editorRevision.current });
						}}
					>
						<fieldset className="net-editor-body min-w-0" disabled={save.isPending}>
							<div className="net-identity">
								<OperationsField label={t("settings.egress.name")} controlId="pool-name">
									<Input
										id="pool-name"
										placeholder={t("ops.netPoolExample")}
										maxLength={160}
										value={form.name}
										onChange={(event) => setForm({ ...form, name: event.target.value })}
									/>
								</OperationsField>
								<OperationsField label={t("settings.egress.enabled")} controlId="pool-enabled">
									<Switch
										id="pool-enabled"
										checked={form.enabled}
										onCheckedChange={(enabled) => setForm({ ...form, enabled })}
									/>
								</OperationsField>
							</div>
							<OperationsField
								label={t("proxies.pools.strategy")}
								controlId="pool-strategy"
								description={t(
									`proxies.pools.${{ affinity: "strategyAffinityHelp", random: "strategyRandomHelp", sticky: "strategyStickyHelp", rotation: "strategyRotationHelp", "least-used": "strategyLeastUsedHelp" }[form.strategy]}`,
								)}
							>
								<NetworkSelect
									id="pool-strategy"
									label={t("proxies.pools.strategy")}
									columns={2}
									value={form.strategy}
									onChange={(strategy) => setForm({ ...form, strategy })}
									options={[
										{
											value: "affinity",
											label: t("proxies.pools.strategyAffinity"),
											icon: UserRound,
										},
										{ value: "random", label: t("proxies.pools.strategyRandom"), icon: Shuffle },
										{ value: "sticky", label: t("proxies.pools.strategySticky"), icon: Star },
										{
											value: "rotation",
											label: t("proxies.pools.strategyRotation"),
											icon: Repeat2,
										},
										{
											value: "least-used",
											label: t("proxies.pools.strategyLeastUsed"),
											icon: Gauge,
										},
									]}
								/>
							</OperationsField>
							<OperationsField
								label={t("settings.egress.pools.fallbackLabel")}
								controlId="pool-fallback"
								description={t("settings.egress.pools.fallbackHelp")}
							>
								<div className="space-y-2">
									<NetworkSelect
										id="pool-fallback"
										label={t("settings.egress.pools.fallbackLabel")}
										value={form.fallbackMode}
										onChange={(fallbackMode) => setForm({ ...form, fallbackMode })}
										options={[
											{ value: "none", label: t("ops.netNoFallback"), icon: Ban },
											{ value: "direct", label: t("ops.netDirect"), icon: Globe },
											{ value: "pool", label: t("ops.netOtherPool"), icon: Layers2 },
										]}
									/>
									{form.fallbackMode === "pool" && (
										<Select
											value={form.fallbackPoolId}
											onValueChange={(fallbackPoolId) => setForm({ ...form, fallbackPoolId })}
										>
											<SelectTrigger aria-label={t("settings.egress.pools.selectFallback")}>
												<SelectValue placeholder={t("settings.egress.pools.selectFallback")} />
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
						</fieldset>
						<DialogFooter>
							<Button type="button" variant="secondary" size="sm" onClick={() => setEditing(null)}>
								{t(save.isPending ? "common.close" : "common.cancel")}
							</Button>
							<Button
								type="submit"
								size="sm"
								disabled={
									!form.name.trim() ||
									save.isPending ||
									(form.fallbackMode === "pool" && !form.fallbackPoolId)
								}
							>
								{save.isPending ? <Spinner /> : null}
								{t("common.save")}
							</Button>
						</DialogFooter>
					</form>
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
	const now = useNow(30_000);
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
		mutationFn: (poolId: string) => resetEgressPoolStats(poolId),
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: ["egress-pool-stats"] });
			toast.success(t("proxies.pools.statsResetDone"));
		},
		onError: (error) =>
			toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
	});
	const testMembers = useMutation({
		mutationFn: (ids: string[]) => testEgressNodes(ids),
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
			toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
	});
	if (!pool) return null;
	const stats = new Map((statsQuery.data?.items ?? []).map((item) => [item.nodeId, item]));
	const members = (nodesQuery.data?.items ?? []).filter((node) =>
		(pool.memberIds ?? []).includes(node.id),
	);
	const rows = [
		...members.map((node) => ({
			id: node.id,
			name: node.name,
			condition: nodeCondition(node, now),
			latency: node.rotatingEndpoint ? 0 : node.probeLatencyMs,
			stat: stats.get(node.id),
		})),
		...[...stats.entries()]
			.filter(([id]) => !(pool.memberIds ?? []).includes(id))
			.map(([id, stat]) => ({
				id,
				name: t("proxies.pools.statsRemovedNode", { id }),
				condition: "unknown" as const,
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
			<DialogContent className="npool-stats-dialog" aria-describedby="npool-stats-description">
				<DialogHeader className="npool-members-heading">
					<DialogTitle>{t("proxies.pools.statsTitle", { name: pool.name })}</DialogTitle>
					<DialogDescription id="npool-stats-description">{t("networkPools.statsDescription")}</DialogDescription>
				</DialogHeader>
				<div className="npool-stats-body">
					{statsQuery.isError || nodesQuery.isError ? (
						<OperationsError
							retry={() => {
								void statsQuery.refetch();
								void nodesQuery.refetch();
							}}
						/>
					) : statsQuery.isPending || nodesQuery.isPending ? (
						<div className="flex h-20 items-center justify-center">
							<Spinner />
						</div>
					) : rows.length === 0 ? (
						<p className="py-6 text-center text-xs text-muted-foreground">
							{t("proxies.pools.statsEmpty")}
						</p>
					) : (
						<Table className="min-w-[700px] table-fixed" viewportRows={8} rowHeight={48}>
							<TableHeader>
								<TableRow className="hover:bg-transparent">
								<TableHead className="w-[170px] text-center">{t("settings.egress.name")}</TableHead>
								<TableHead className="w-[110px] text-center">
										{t("proxies.pools.statsStatus")}
									</TableHead>
									<TableHead className="w-[64px] text-center">
										{t("proxies.pools.statsSelections")}
									</TableHead>
								<TableHead className="w-[110px] text-center">
									{t("networkPools.nodeFailures")}
									</TableHead>
									<TableHead className="w-[84px] whitespace-nowrap text-center">
										{t("networkPools.probeLatency")}
									</TableHead>
									<TableHead className="w-[172px] whitespace-nowrap text-center">
										{t("proxies.pools.statsLastSelected")}
									</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{rows.map((row) => (
									<TableRow key={row.id} className="h-12">
										<TableCell className="text-center text-xs font-medium">
											{row.name}
											{row.id === currentNode ? (
												<span className="npool-current-badge">
													{t(pool.strategy === "sticky" ? "networkPools.preferredExit" : "networkPools.rotationPosition")}
												</span>
											) : null}
										</TableCell>
										<TableCell className="text-center text-[11px]">
											<StatusPill tone={["ready", "dynamic"].includes(row.condition) ? "good" : ["held", "banned", "cooling", "unhealthy", "unconfigured"].includes(row.condition) ? "warn" : "neutral"}>{t(`ops.condition.${row.condition}`)}</StatusPill>
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
											{row.stat?.failures ?? "—"}
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
												? formatCompactDateTime(row.stat.lastSelectedAt, i18n.language)
												: t("settings.egress.never")}
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</div>
				<DialogFooter className="npool-stats-footer">
					<div className="npool-stats-since">
						<span>{t("networkPools.selectionTotal", { value: statsQuery.isError || nodesQuery.isError || statsQuery.isPending || nodesQuery.isPending ? "—" : rows.reduce((sum, row) => sum + (row.stat?.selections ?? 0), 0) })}</span>
						{statsQuery.data?.since && <span>{t("networkPools.statsSince", { time: formatCompactDateTime(statsQuery.data.since, i18n.language) })}</span>}
					</div>
					<div className="flex items-center gap-2">
						{(pool.memberIds ?? []).length > 0 ? (
							<Button
								type="button"
								size="sm"
								variant="secondary"
								disabled={testMembers.isPending}
								onClick={() => testMembers.mutate([...pool.memberIds])}
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
							onClick={() => reset.mutate(pool.id)}
						>
							{t("proxies.pools.statsReset")}
						</Button>
						<Button type="button" size="sm" variant="secondary" onClick={() => onOpenChange(false)}>
							{t("common.close")}
						</Button>
					</div>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}

class PoolMemberPreferenceError extends Error {
	constructor(readonly ids: string[], readonly preferred: string | undefined, cause: unknown) {
		super(cause instanceof Error ? cause.message : String(cause));
	}
}

/** Pool-side node management: checkbox list with search + selected-only
 *  filter — with dozens of nodes, finding the ones to toggle is the hard
 *  part, not the toggling. One save applies every checked/unchecked row. */
function PoolMembersEditor({ pool, onSaved, onClose }: {
	pool: EgressPoolDTO;
	onSaved: () => void;
	onClose: () => void;
}) {
	const { t } = useTranslation();
	const now = useNow(30_000);
	const active = useRef(true);
	useEffect(() => {
		active.current = true;
		return () => {
			active.current = false;
		};
	}, []);
	const [draft, setDraft] = useState<Set<string>>(() => new Set(pool.memberIds));
	const [baseline, setBaseline] = useState({
		ids: pool.memberIds,
		preferred: pool.preferredNodeId,
	});
	const [search, setSearch] = useState("");
	const [onlySelected, setOnlySelected] = useState(true);
	// 首选的本地镜像:点击后立即变实心,不等父级列表刷新。
	const [preferredId, setPreferredId] = useState<string | undefined>(pool.preferredNodeId);
	const nodesQuery = useQuery({
		queryKey: ["egress-nodes", "pool-nodes", pool?.id ?? ""],
		queryFn: () => listAllEgressNodes(),
		enabled: pool !== null,
		staleTime: 10_000,
	});
	const all: EgressNodeDTO[] = nodesQuery.data?.items ?? [];
	// Membership and priority have separate endpoints. Capture both identities in
	// the submission and retain committed membership if the priority write fails.
	const apply = useMutation({
		mutationFn: async (submission: { poolId: string; ids: string[]; preferred?: string; previousPreferred?: string; }) => {
			await setEgressPoolMembers(submission.poolId, submission.ids);
			let savedPreferred = submission.previousPreferred && submission.ids.includes(submission.previousPreferred)
				? submission.previousPreferred : undefined;
			try {
				if (savedPreferred && savedPreferred !== submission.preferred) {
					await setEgressPoolMemberPriority(submission.poolId, savedPreferred, 0);
					savedPreferred = undefined;
				}
				if (submission.preferred && submission.ids.includes(submission.preferred)) {
					await setEgressPoolMemberPriority(submission.poolId, submission.preferred, 1);
					savedPreferred = submission.preferred;
				}
			} catch (error) {
				throw new PoolMemberPreferenceError(submission.ids, savedPreferred, error);
			}
		},
		onSuccess: (_, submission) => {
			onSaved();
			if (active.current) setBaseline({ ids: submission.ids, preferred: submission.preferred });
			toast.success(t("proxies.pools.nodesSaved"));
		},
		onError: (error) => {
			if (error instanceof PoolMemberPreferenceError) {
				onSaved();
				if (active.current) setBaseline({ ids: error.ids, preferred: error.preferred });
			}
			toast.error(error instanceof PoolMemberPreferenceError ? t("networkPools.preferenceSaveFailed") : error.message);
		},
	});
	const dirty =
		draft.size !== baseline.ids.length ||
		baseline.ids.some((id) => !draft.has(id)) ||
		(preferredId && draft.has(preferredId) ? preferredId : undefined) !== baseline.preferred;
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
		<fieldset className="npool-members-editor" disabled={apply.isPending}>
			<div className="npool-members-body">
				<div className="min-w-0 space-y-3">
					{nodesQuery.isError ? (
						<OperationsError retry={() => void nodesQuery.refetch()} />
					) : nodesQuery.isPending ? (
						<div className="flex h-20 items-center justify-center">
							<Spinner />
						</div>
					) : (
						<>
							<div className="npool-members-toolbar">
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
									variant="outline"
									aria-pressed={!onlySelected}
									onClick={() => setOnlySelected((current) => !current)}
								>
									{t(onlySelected ? "ops.netAddMembers" : "ops.netViewMembers")}
								</Button>
							</div>
							<div className="npool-member-selection">
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
										disabled={!visible.some((node) => draft.has(node.id))}
										onClick={() => setDraft((current) => {
											const next = new Set(current);
											for (const node of visible) next.delete(node.id);
											return next;
										})}
									>
										{t("networkPools.removeVisible")}
									</Button>
								</div>
							</div>
							<div className="min-w-0">
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

								{
									<Table className="min-w-[540px] table-fixed" viewportRows={7} rowHeight={64}>
										<TableHeader>
											<TableRow>
												<TableHead className="w-10" />
												<TableHead>{t("ops.nodeName")}</TableHead>
												<TableHead className="w-32">{t("ops.nodeState")}</TableHead>
												<TableHead className="w-16">{t("proxies.pools.preferSet")}</TableHead>
											</TableRow>
										</TableHeader>
										<VirtualTableBody
											items={visible}
											colSpan={4}
											rowHeight={64}
											renderRow={(node) => {
												const selected = draft.has(node.id),
													isPreferred = preferredId === node.id,
													condition = nodeCondition(node, now);
												return (
													<TableRow
														key={node.id}
														className="h-16"
														onClick={() => {
															if (!apply.isPending) toggle(node.id, !selected);
														}}
													>
														<TableCell>
															<Checkbox
																checked={selected}
																onCheckedChange={(checked) => toggle(node.id, checked === true)}
																aria-label={node.name}
																onClick={(event) => event.stopPropagation()}
															/>
														</TableCell>
														<TableCell>
															<div className="min-w-0">
																<NetworkText className="text-xs font-medium">{node.name}</NetworkText>
																<span className="mt-1 block truncate text-[10px] text-muted-foreground">
																	{[node.exitIp, node.sourceName].filter(Boolean).join(" · ")}
																</span>
															</div>
														</TableCell>
														<TableCell>
															<StatusPill
																tone={
																	nodeNeedsAttention(node, now)
																		? "warn"
																		: ["ready", "dynamic"].includes(condition)
																			? "good"
																			: "neutral"
																}
															>
																{t(`ops.condition.${condition}`)}
															</StatusPill>
														</TableCell>
														<TableCell>
															{selected ? (
																<NetworkTooltip
																	content={t(
																		isPreferred
																			? "proxies.pools.preferClear"
																			: "proxies.pools.preferSet",
																	)}
																>
																	<button
																		type="button"
																		className={cn(
																			"inline-flex size-7 items-center justify-center",
																		isPreferred && "npool-preferred",
																		)}
																		aria-label={t(
																			isPreferred
																				? "proxies.pools.preferClear"
																				: "proxies.pools.preferSet",
																		)}
																		onClick={(event) => {
																			event.stopPropagation();
																			setPreferredId(isPreferred ? undefined : node.id);
																		}}
																	>
																		<Star className={cn("size-3.5", isPreferred && "fill-current")} />
																	</button>
																</NetworkTooltip>
															) : (
																<span className="inline-flex size-7 items-center justify-center" />
															)}
														</TableCell>
													</TableRow>
												);
											}}
										/>
									</Table>
								}
							</div>
						</>
					)}
				</div>
				{apply.isError && <p className="npool-save-error" role="alert">{apply.error instanceof PoolMemberPreferenceError ? `${t("networkPools.preferenceSaveFailed")} ${apply.error.message}` : apply.error.message}</p>}
			</div>
			<div className="npool-members-footer">
				<span aria-live="polite">{dirty ? t("ops.netMembersDraft") : t("ops.settingsSaved")}</span>
				<Button type="button" variant="ghost" size="sm" onClick={onClose}>{t("common.close")}</Button>
				<Button
					type="button"
					variant="ghost"
					size="sm"
					disabled={!dirty || apply.isPending}
					onClick={() => {
						setDraft(new Set(baseline.ids));
						setPreferredId(baseline.preferred);
						apply.reset();
					}}
				>
					{t("proxies.discard")}
				</Button>
				<Button
					type="button"
					size="sm"
					disabled={!dirty || apply.isPending || nodesQuery.isPending || nodesQuery.isError}
					onClick={() =>
						apply.mutate({
							poolId: pool.id,
							previousPreferred: baseline.preferred,
							ids: [...draft],
							preferred: preferredId && draft.has(preferredId) ? preferredId : undefined,
						})
					}
				>
					{apply.isPending && <Spinner />}
					{t("common.save")}
				</Button>
			</div>
		</fieldset>
	);
}
