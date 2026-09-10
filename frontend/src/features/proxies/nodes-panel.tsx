import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import {
	OperationsDialogContent as DialogContent,
	OperationsAlertDialogContent as AlertDialogContent,
} from "@/features/operations/operations-ui";
import {
	nodeCondition,
	nodeNeedsAttention,
} from "@/features/operations/operations-data";
import {
	StatusPill,
	OperationsField,
} from "@/features/operations/operations-ui";
import { useNow } from "@/features/guard/quality-hooks";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
	fetchQualityNodes,
	unbanQualityNode,
} from "@/features/guard/quality-api";
import { maskIP } from "@/features/guard/quality-view";
import {
	CircleAlert,
	LockOpen,
	MoreHorizontal,
	Network,
	Pencil,
	Plus,
	Power,
	PowerOff,
	RefreshCw,
	Search,
	Trash2,
	Upload,
} from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";
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
	DialogDescription,
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
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import {
	Table,
	TableActionCell,
	TableActionHead,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import {
	Tooltip,
	TooltipContent,
	TooltipTrigger,
} from "@/components/ui/tooltip";
import { SubscriptionsPanel } from "@/features/proxies/subscriptions-panel";
import { ProbeSettingsButton } from "@/features/proxies/probe-settings";
import { Rss } from "lucide-react";
import { listEgressSources } from "@/features/settings/settings-api";
import {
	getSettings,
	batchSetEgressRotation,
	cleanupUnhealthyEgressNodes,
	createEgressNode,
	deleteEgressNode,
	deleteEgressNodes,
	getEgressNodeProxyURL,
	getEgressNodeRotationURL,
	importEgressText,
	listAllEgressNodes,
	listEgressNodes,
	previewUnhealthyEgressNodes,
	refreshEgressClearance,
	rotateEgressNode,
	testEgressNode,
	testEgressNodes,
	updateEgressNode,
	updateEgressNodesEnabled,
	type ClearanceMode,
	type EgressNodeDTO,
	type EgressNodeInput,
} from "@/features/settings/settings-api";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import {
	nextTableSort,
	type SortOrder,
	type TableSort,
} from "@/shared/lib/table-sort";

const emptyInput: EgressNodeInput = {
	name: "",
	enabled: true,
	proxyPool: false,
	proxyURL: "",
	rotationURL: "",
	rotationEnabled: false,
};
// rotationOpen is the dialog-local switch state (single source of truth for
// 支持更换IP); rotationURL/rotationEnabled derive from it at save time.
type NodeForm = EgressNodeInput & { rotationOpen: boolean };
const emptyForm: NodeForm = { ...emptyInput, rotationOpen: false };
type ImportForm = { name: string; content: string };
const emptyImport: ImportForm = { name: "", content: "" };

// Batched probing keeps the admin HTTP timeout safe: each node probes IPv4
// and IPv6 in parallel with a 15-second ceiling.
const egressProbeBatchSize = 32;

async function testAllEgressNodes() {
	const nodes = await listAllEgressNodes();
	const ids = nodes.items
		.filter((node) => node.enabled && node.proxyConfigured)
		.map((node) => node.id);
	return probeAllEnabledNodes(ids);
}

async function probeAllEnabledNodes(ids: string[]) {
	const result = { requested: 0, healthy: 0, unhealthy: 0, failed: 0 };
	let firstError: unknown;
	for (let index = 0; index < ids.length; index += egressProbeBatchSize) {
		const batchIDs = ids.slice(index, index + egressProbeBatchSize);
		try {
			const batch = await testEgressNodes(batchIDs);
			result.requested += batch.requested;
			result.healthy += batch.healthy;
			result.unhealthy += batch.unhealthy;
		} catch (error) {
			firstError ??= error;
			result.failed += batchIDs.length;
		}
	}
	if (result.requested === 0 && result.failed > 0) {
		throw firstError;
	}
	return result;
}

export function NodesPanel({
	initialSearch = "",
	initialCondition = "all",
}: {
	initialSearch?: string;
	initialCondition?: string;
}) {
	const now = useNow(15000);
	const [conditionFilter, setConditionFilter] = useState(initialCondition);
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	// Clearance mode lives in the Grok Web settings form; the nodes table only
	// reads it to decide whether the per-row "refresh clearance" action applies.
	const settingsQuery = useQuery({
		queryKey: ["settings"],
		queryFn: ({ signal }) => getSettings(signal),
		staleTime: 30_000,
	});
	const clearanceMode: ClearanceMode =
		settingsQuery.data?.config.providerWeb.clearanceMode ?? "manual";
	const [editing, setEditing] = useState<EgressNodeDTO | null | undefined>(
		undefined,
	);
	const [revealedProxyURL, setRevealedProxyURL] = useState("");
	const [revealedRotationURL, setRevealedRotationURL] = useState("");
	const [importOpen, setImportOpen] = useState(false);
	const [sourcesOpen, setSourcesOpen] = useState(false);
	const [importForm, setImportForm] = useState<ImportForm>(emptyImport);
	const [form, setForm] = useState<NodeForm>(emptyForm);
	const [page, setPage] = useState(1);
	const [pageSize, setPageSize] = useState(20);
	const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
	const [search, setSearch] = useState(initialSearch);
	const [enabledFilter, setEnabledFilter] = useState("");
	const [probeFilter, setProbeFilter] = useState("");
	const [selected, setSelected] = useState<Map<string, EgressNodeDTO>>(
		() => new Map(),
	);
	const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
	const [cleanupOpen, setCleanupOpen] = useState(false);
	const debouncedSearch = useDebouncedValue(search);
	// Node mutations ripple into pool member counts, subscription summaries and
	// shared proxy profiles; one helper keeps every invalidation site in sync.
	const invalidateAfterNodeChange = () => {
		void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
		void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
		void queryClient.invalidateQueries({ queryKey: ["egress-sources"] });
	};
	// 节点降智计数以质量层台账为准(G8 单一数据源);台账缺席的节点
	// 回退底座历史计数(切换前存量,只减不增)。
	const qualityNodesQuery = useQuery({
		queryKey: ["quality", "nodes"],
		queryFn: ({ signal }) => fetchQualityNodes(signal),
		staleTime: 15_000,
		refetchInterval: 60_000,
	});
	const qualityDegrades = useMemo(() => {
		const totals = new Map<number, number>();
		for (const node of qualityNodesQuery.data ?? []) {
			totals.set(node.node_id, node.degrade_total);
		}
		return totals;
	}, [qualityNodesQuery.data]);
	const query = useQuery({
		queryKey: [
			"egress-nodes",
			"page",
			page,
			pageSize,
			debouncedSearch,
			enabledFilter,
			probeFilter,
			sort.field,
			sort.order,
			conditionFilter,
		],
		queryFn: async ({ signal }) => {
			if (conditionFilter === "all")
				return listEgressNodes(
					{
						page,
						pageSize,
						search: debouncedSearch,
						enabled: enabledFilter,
						probe: probeFilter,
						sortBy: sort.field || undefined,
						sortOrder: sort.field ? sort.order : undefined,
					},
					signal,
				);
			const data = await listAllEgressNodes();
			const filtered = data.items.filter((node) => {
				const state = nodeCondition(node, now);
				if (conditionFilter === "attention" && !nodeNeedsAttention(node, now))
					return false;
				if (
					conditionFilter === "ready" &&
					!["ready", "dynamic"].includes(state)
				)
					return false;
				if (conditionFilter === "disabled" && node.enabled) return false;
				if (conditionFilter === "unknown" && state !== "unknown") return false;
				if (
					debouncedSearch &&
					!`${node.name} ${node.exitIp ?? ""}`
						.toLowerCase()
						.includes(debouncedSearch.toLowerCase())
				)
					return false;
				if (enabledFilter && node.enabled !== (enabledFilter === "enabled"))
					return false;
				return !probeFilter || node.probeStatus === probeFilter;
			});
			if (sort.field)
				filtered.sort(
					(a, b) =>
						(sort.field === "health"
							? a.health - b.health
							: a.name.localeCompare(b.name)) *
						(sort.order === "desc" ? -1 : 1),
				);
			return {
				...data,
				total: filtered.length,
				page,
				pageSize,
				items: filtered.slice((page - 1) * pageSize, page * pageSize),
			};
		},
		staleTime: 15_000,
		refetchOnWindowFocus: true,
		refetchInterval: 15000,
	});
	const save = useMutation({
		mutationFn: () => {
			const normalizedProxyURL = form.proxyURL?.trim() || "";
			const trimmedRotationURL = form.rotationOpen
				? form.rotationURL?.trim() || ""
				: "";
			const formFields: EgressNodeInput = {
				name: form.name,
				enabled: form.enabled,
				proxyPool: form.proxyPool,
				proxyURL: form.proxyURL,
				rotationURL: form.rotationURL,
				rotationEnabled: form.rotationEnabled,
			};
			const input: EgressNodeInput = {
				...formFields,
				proxyURL:
					normalizedProxyURL &&
					(!editing || normalizedProxyURL !== revealedProxyURL)
						? normalizedProxyURL
						: undefined,
				clearProxyURL:
					!normalizedProxyURL && revealedProxyURL ? true : undefined,
				// 换 IP webhook 与代理地址同语义:仅在确认已有存量(reveal 已返回)且被清空时
				// 才显式清除;reveal 未返回/失败时保持存量, 避免快速保存把已配置的 webhook
				// 静默删除。开关关闭=暂停(保留 webhook), 未触及=不修改。
				rotationURL:
					form.rotationOpen &&
					trimmedRotationURL &&
					trimmedRotationURL !== revealedRotationURL
						? trimmedRotationURL
						: undefined,
				clearRotationURL:
					form.rotationOpen && !trimmedRotationURL && revealedRotationURL
						? true
						: undefined,
				// 开关=启用换 IP 轮换;代理池与轮换互斥(勾代理池时 UI 强制关闭并清零)。
				// 旧逻辑"开关开+存量URL未重填"会送 false,悄悄停用已配置的轮换。
				rotationEnabled: form.rotationOpen && form.rotationEnabled,
			};
			return editing
				? updateEgressNode(editing.id, input)
				: createEgressNode(input);
		},
		onSuccess: () => {
			invalidateAfterNodeChange();
			setEditing(undefined);
			toast.success(t("settings.egress.saved"));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const importText = useMutation({
		mutationFn: () => importEgressText(importForm),
		onSuccess: (value) => {
			invalidateAfterNodeChange();
			setImportOpen(false);
			toast.success(t("settings.egress.imported", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const remove = useMutation({
		mutationFn: deleteEgressNode,
		onSuccess: (_, id) => {
			if (page > 1 && query.data?.items.length === 1) setPage(page - 1);
			setSelected((current) => {
				const next = new Map(current);
				next.delete(id);
				return next;
			});
			invalidateAfterNodeChange();
			toast.success(t("settings.egress.deleted"));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const removeMany = useMutation({
		mutationFn: () => deleteEgressNodes([...selected.keys()]),
		onSuccess: (value) => {
			const selectedOnCurrentPage =
				query.data?.items.filter((node) => selected.has(node.id)).length ?? 0;
			if (
				page > 1 &&
				query.data &&
				query.data.items.length > 0 &&
				selectedOnCurrentPage === query.data.items.length
			)
				setPage(page - 1);
			setSelected(new Map());
			setBatchDeleteOpen(false);
			invalidateAfterNodeChange();
			toast.success(t("settings.egress.batchDeleted", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const updateManyEnabled = useMutation({
		mutationFn: (enabled: boolean) =>
			updateEgressNodesEnabled([...selected.keys()], enabled),
		onSuccess: (value, enabled) => {
			setSelected(new Map());
			invalidateAfterNodeChange();
			toast.success(
				t(
					enabled
						? "settings.egress.batchEnabled"
						: "settings.egress.batchDisabled",
					value,
				),
			);
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const cleanupPreview = useMutation({
		mutationFn: previewUnhealthyEgressNodes,
	});
	const cleanupUnhealthy = useMutation({
		mutationFn: cleanupUnhealthyEgressNodes,
		onSuccess: (value) => {
			setPage(1);
			setSelected(new Map());
			setCleanupOpen(false);
			cleanupPreview.reset();
			invalidateAfterNodeChange();
			toast.success(t("settings.egress.cleanupUnavailableComplete", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const refreshClearance = useMutation({
		mutationFn: (id: string) => refreshEgressClearance(id),
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			toast.success(t("settings.egress.clearanceRefreshed"));
		},
		onError: (error) =>
			toast.error(
				error instanceof Error
					? error.message
					: t("settings.egress.operationFailed"),
			),
	});
	const rotateNode = useMutation({
		mutationFn: (id: string) => rotateEgressNode(id),
		onSuccess: () => {
			toast.success(t("settings.egress.rotateQueued"));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	// 质量层人工解禁(G7):统一 ban 律的兜底出口,归口出口管理面。
	const unbanNode = useMutation({
		mutationFn: (id: string) => unbanQualityNode(Number(id)),
		onSuccess: (_result, id) => {
			void queryClient.invalidateQueries({ queryKey: ["quality", "nodes"] });
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			toast.success(t("quality.egressView.unbanDone", { id: Number(id) }));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	// 批量轮换所选(G18):逐个走单点轮换同一条门(出口层全局限速兜底)。
	const rotateMany = useMutation({
		mutationFn: async () => {
			const targets = [...selected.values()].filter(
				(node) => node.rotationConfigured,
			);
			let triggered = 0;
			let limited = 0;
			for (const node of targets) {
				try {
					await rotateEgressNode(node.id);
					triggered += 1;
				} catch {
					limited += 1;
				}
			}
			return { triggered, limited };
		},
		onSuccess: (value) => {
			toast.success(
				t("quality.egress.rotateDone", {
					triggered: value.triggered,
					limited: value.limited,
				}),
			);
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	// 降智台账明细(G8:节点看总量,点开看 IP 历史)。
	const [ledgerNodeID, setLedgerNodeID] = useState<number | null>(null);
	const [batchRotationOpen, setBatchRotationOpen] = useState(false);
	const [batchRotationTemplate, setBatchRotationTemplate] = useState("");

	const batchRotation = useMutation({
		mutationFn: () =>
			batchSetEgressRotation([...selected.keys()], batchRotationTemplate),
		onSuccess: (value) => {
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			setBatchRotationOpen(false);
			toast.success(t("settings.egress.batchRotationDone", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const testAll = useMutation({
		mutationFn: testAllEgressNodes,
		onSuccess: (value) => {
			if (value.failed > 0)
				toast.warning(t("settings.egress.testedPartial", value));
			else toast.success(t("settings.egress.tested", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
		onSettled: () => {
			invalidateAfterNodeChange();
		},
	});
	const testNode = useMutation({
		mutationFn: testEgressNode,
		onSuccess: (result) => {
			if (result.status === "healthy")
				toast.success(t("settings.egress.testedOne"));
			else toast.error(result.error || t("settings.egress.operationFailed"));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
		onSettled: () => {
			invalidateAfterNodeChange();
		},
	});

	function openCreate() {
		setRevealedProxyURL("");
		setRevealedRotationURL("");
		setForm(emptyForm);
		setEditing(null);
	}

	function openCleanup() {
		cleanupPreview.reset();
		setCleanupOpen(true);
		cleanupPreview.mutate();
	}

	function openEdit(node: EgressNodeDTO) {
		setForm({
			name: node.name,
			enabled: node.enabled,
			proxyPool: node.proxyPool,
			proxyURL: "",
			rotationURL: "",
			rotationEnabled:
				node.rotationConfigured && (node.rotationEnabled ?? true),
			rotationOpen: node.rotationConfigured && (node.rotationEnabled ?? true),
		});
		setRevealedProxyURL("");
		setRevealedRotationURL("");
		setEditing(node);
		// Proxy address and rotation webhook are plain editable fields: fetch the
		// stored values once so operators can see and delete them without a
		// reveal dance. Shared-profile nodes keep their managed address hidden.
		if (node.proxyConfigured) {
			getEgressNodeProxyURL(node.id)
				.then(({ proxyURL }) => {
					setRevealedProxyURL(proxyURL);
					setForm((current) =>
						current.proxyURL === "" ? { ...current, proxyURL } : current,
					);
				})
				.catch(() => undefined);
		}
		if (node.rotationConfigured) {
			getEgressNodeRotationURL(node.id)
				.then(({ rotationURL }) => {
					setRevealedRotationURL(rotationURL);
					setForm((current) =>
						current.rotationURL === "" ? { ...current, rotationURL } : current,
					);
				})
				.catch(() => undefined);
		}
	}

	function changeSort(field: string, initialOrder: SortOrder): void {
		setSort((current) => nextTableSort(current, field, initialOrder));
		setPage(1);
	}

	function togglePage(checked: boolean): void {
		setSelected((current) => {
			const next = new Map(current);
			for (const node of nodes) {
				if (checked) next.set(node.id, node);
				else next.delete(node.id);
			}
			return next;
		});
	}

	function toggleNode(node: EgressNodeDTO, checked: boolean): void {
		setSelected((current) => {
			const next = new Map(current);
			if (checked) next.set(node.id, node);
			else next.delete(node.id);
			return next;
		});
	}

	const nodes = query.data?.items ?? [];
	const selectedOnPage = nodes.filter((node) => selected.has(node.id));
	const allPageSelected =
		nodes.length > 0 && selectedOnPage.length === nodes.length;
	const selectedNodes = [...selected.values()];
	const selectedSourceNodes = selectedNodes.filter(
		(node) => node.sourceId,
	).length;
	const batchPending = removeMany.isPending || updateManyEnabled.isPending;
	const hasActiveFilters = Boolean(
		debouncedSearch ||
			enabledFilter ||
			probeFilter ||
			conditionFilter !== "all",
	);

	const sourcesQuery = useQuery({
		queryKey: ["egress-sources"],
		queryFn: () => listEgressSources(),
		staleTime: 15_000,
	});
	const sourceCount = sourcesQuery.data?.items.length ?? 0;

	return (
		<div className="space-y-5">
			<div className="ops-panel ops-node-inventory">
				<div className="ops-segment border-b px-4 py-3">
					{["all", "attention", "ready", "unknown", "disabled"].map((value) => (
						<button
							key={value}
							aria-pressed={conditionFilter === value}
							onClick={() => {
								setConditionFilter(value);
								setPage(1);
								setSelected(new Map());
							}}
						>
							{t(
								`ops.${value === "all" ? "filterAll" : value === "attention" ? "filterAttention" : value === "ready" ? "filterReady" : value === "unknown" ? "unchecked" : "filterDisabled"}`,
							)}
						</button>
					))}
				</div>
				<DataTableShell
					className="ops-node-table-shell"
					toolbar={
						<>
							<div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">
								<div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
									<Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
									<Input
										className="h-8 pl-9 text-xs"
										value={search}
										onChange={(event) => {
											setSearch(event.target.value);
											setPage(1);
											setSelected(new Map());
										}}
										placeholder={t("settings.egress.search")}
										aria-label={t("settings.egress.search")}
									/>
								</div>
								<DataTableFilters
									filters={[
										{
											id: "enabled",
											label: t("settings.egress.enabled"),
											value: enabledFilter,
											onChange: (value) => {
												setEnabledFilter(value);
												setPage(1);
												setSelected(new Map());
											},
											options: [
												{ value: "enabled", label: t("common.enable") },
												{ value: "disabled", label: t("common.disable") },
											],
										},
										{
											id: "probe",
											label: t("settings.egress.probe"),
											value: probeFilter,
											onChange: (value) => {
												setProbeFilter(value);
												setPage(1);
												setSelected(new Map());
											},
											options: [
												{
													value: "healthy",
													label: t("settings.egress.healthy"),
												},
												{
													value: "unhealthy",
													label: t("settings.egress.unhealthy"),
												},
												{
													value: "unknown",
													label: t("settings.egress.notTested"),
												},
											],
										},
									]}
								/>
							</div>
							<div className="flex flex-wrap items-center gap-1.5">
								{selected.size > 0 ? (
									<>
										<span className="mr-1 text-xs text-muted-foreground">
											{t("common.selectedCount", { count: selected.size })}
										</span>
										<Button
											type="button"
											size="sm"
											variant="secondary"
											disabled={
												batchPending ||
												rotateMany.isPending ||
												selectedNodes.every((node) => !node.rotationConfigured)
											}
											onClick={() => rotateMany.mutate()}
										>
											{rotateMany.isPending ? <Spinner /> : <RefreshCw />}
											{t("quality.egress.batchRotate", {
												count: selectedNodes.filter(
													(node) => node.rotationConfigured,
												).length,
											})}
										</Button>
										<Button
											type="button"
											size="sm"
											variant="secondary"
											disabled={
												batchPending ||
												selectedNodes.every((node) => node.enabled)
											}
											onClick={() => updateManyEnabled.mutate(true)}
										>
											<Power />
											{t("common.enable")}
										</Button>
										<Button
											type="button"
											size="sm"
											variant="secondary"
											disabled={batchPending}
											onClick={() => {
												setBatchRotationTemplate("");
												setBatchRotationOpen(true);
											}}
										>
											{t("settings.egress.batchRotation")}
										</Button>
										<Button
											type="button"
											size="sm"
											variant="secondary"
											disabled={
												batchPending ||
												selectedNodes.every((node) => !node.enabled)
											}
											onClick={() => updateManyEnabled.mutate(false)}
										>
											<PowerOff />
											{t("common.disable")}
										</Button>
										<Button
											type="button"
											size="sm"
											variant="secondary"
											className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive"
											disabled={batchPending}
											onClick={() => setBatchDeleteOpen(true)}
										>
											<Trash2 />
											{t("common.delete")}
										</Button>
									</>
								) : null}
								<Button
									type="button"
									size="icon"
									variant="secondary"
									className="size-8"
									disabled={query.isFetching}
									onClick={() => void query.refetch()}
									aria-label={t("common.refresh")}
									title={t("common.refresh")}
								>
									<RefreshCw
										className={cn(query.isFetching && "animate-spin")}
									/>
								</Button>
								<Button
									type="button"
									size="sm"
									variant="secondary"
									disabled={testAll.isPending}
									onClick={() => testAll.mutate()}
								>
									{testAll.isPending ? <Spinner /> : <Network />}
									{t("settings.egress.testAll")}
								</Button>
								<ProbeSettingsButton />
								<DropdownMenu>
									<DropdownMenuTrigger asChild>
										<Button type="button" size="sm" variant="outline">
											<MoreHorizontal />
											{t("ops.maintenance")}
										</Button>
									</DropdownMenuTrigger>
									<DropdownMenuContent align="end">
										<DropdownMenuItem onClick={() => setSourcesOpen(true)}>
											<Rss />
											{sourceCount > 0
												? t("proxies.supply.manageWithCount", {
														count: sourceCount,
													})
												: t("proxies.supply.manage")}
										</DropdownMenuItem>
										<DropdownMenuItem
											disabled={cleanupUnhealthy.isPending}
											onClick={openCleanup}
										>
											<Trash2 />
											{t("settings.egress.cleanupUnavailable")}
										</DropdownMenuItem>
									</DropdownMenuContent>
								</DropdownMenu>
								<DropdownMenu>
									<DropdownMenuTrigger asChild>
										<Button type="button" size="sm">
											<Plus />
											{t("settings.egress.add")}
										</Button>
									</DropdownMenuTrigger>
									<DropdownMenuContent align="end">
										<DropdownMenuItem onClick={openCreate}>
											<Plus />
											{t("settings.egress.addManually")}
										</DropdownMenuItem>
										<DropdownMenuItem
											onClick={() => {
												setImportForm(emptyImport);
												setImportOpen(true);
											}}
										>
											<Upload />
											{t("settings.egress.importText")}
										</DropdownMenuItem>
									</DropdownMenuContent>
								</DropdownMenu>
							</div>
						</>
					}
					footer={
						query.data && query.data.total > 0 ? (
							<Pagination
								page={query.data.page}
								pageSize={query.data.pageSize}
								total={query.data.total}
								onPageChange={setPage}
								onPageSizeChange={(value) => {
									setPageSize(value);
									setPage(1);
								}}
							/>
						) : undefined
					}
				>
					{query.isError ? (
						<ErrorState
							message={query.error.message}
							onRetry={() => void query.refetch()}
						/>
					) : null}
					{!query.isError ? (
						<>
							<div className="hidden md:block">
								<Table
									rowHeight={72}
									className="ops-compact-table min-w-[850px] table-fixed"
								>
									<TableHeader>
										<TableRow>
											<TableHead className="w-10 px-3">
												<Checkbox
													checked={
														allPageSelected
															? true
															: selectedOnPage.length
																? "indeterminate"
																: false
													}
													disabled={!nodes.length}
													onCheckedChange={(v) => togglePage(v === true)}
													aria-label={t("common.selectPage")}
												/>
											</TableHead>
											<SortableTableHead
												className="w-56 text-left"
												field="name"
												sortBy={sort.field}
												sortOrder={sort.order}
												onSort={changeSort}
											>
												{t("ops.nodeName")}
											</SortableTableHead>
											<TableHead className="w-36">
												{t("ops.nodeState")}
											</TableHead>
											<TableHead className="w-40">{t("ops.path")}</TableHead>
											<TableHead className="w-28">
												{t("ops.latencyShort")}
											</TableHead>
											<TableHead className="w-32">
												{t("ops.recovery")}
											</TableHead>
											<TableHead className="w-24">{t("ops.history")}</TableHead>
											<TableActionHead />
										</TableRow>
									</TableHeader>
									{query.isPending ? (
										<TableBody>
											<TableLoadingRow colSpan={8} />
										</TableBody>
									) : null}
									{!query.isPending && nodes.length === 0 ? (
										<TableBody>
											<TableRow>
												<TableCell
													colSpan={8}
													className="h-24 text-center text-xs text-muted-foreground"
												>
													{hasActiveFilters
														? t("settings.egress.noMatches")
														: t("ops.noNodesHelp")}
												</TableCell>
											</TableRow>
										</TableBody>
									) : null}
									{!query.isPending && nodes.length > 0 ? (
										<VirtualTableBody
											items={nodes}
											colSpan={8}
											rowHeight={72}
											renderRow={(node) => (
												<TableRow
													className="group h-[72px]"
													key={node.id}
													data-state={
														selected.has(node.id) ? "selected" : undefined
													}
												>
													<TableCell className="px-2">
														<Checkbox
															checked={selected.has(node.id)}
															onCheckedChange={(checked) =>
																toggleNode(node, checked === true)
															}
															aria-label={t("common.selectItem", {
																name: node.name,
															})}
														/>
													</TableCell>
													<TableCell>
														<div className="flex min-w-0 items-center gap-2">
															<button
																className="truncate text-left text-[13px] font-medium hover:text-primary"
																title={node.name}
																onClick={() => openEdit(node)}
															>
																{node.name}
															</button>
															{node.lastError && (
																<ErrorTooltip message={node.lastError} />
															)}
														</div>
														<p className="mt-1.5 truncate text-[11px] text-muted-foreground">
															{node.sourceName ?? t("proxies.nodes.manual")}
															<span className="mx-1.5 opacity-40">·</span>
															{node.rotatingEndpoint
																? t("ops.condition.dynamic")
																: node.rotationConfigured
																	? t("ops.recoveryAuto")
																	: t("ops.recoveryManual")}
														</p>
													</TableCell>
													<TableCell>
														<NodeStateLabel node={node} now={now} />
														{node.quality?.caseId && (
															<Link
																to={`/guard?case=${node.quality.caseId}#tribunal`}
																className="mt-1.5 block text-[11px] text-primary"
															>
																{t("ops.inspectCase")} #{node.quality.caseId}
															</Link>
														)}
													</TableCell>
													<TableCell>
														<ExitIPCell node={node} />
													</TableCell>
													<TableCell>
														<LatencyCell node={node} />
														<p className="mt-1 text-[10px] text-muted-foreground">
															{node.lastProbedAt
																? new Date(
																		node.lastProbedAt,
																	).toLocaleTimeString([], {
																		hour: "2-digit",
																		minute: "2-digit",
																	})
																: "—"}
														</p>
													</TableCell>
													<TableCell>
														<RotatableCell node={node} />
													</TableCell>
													<TableCell>
														<DegradeBadge
															nodeID={Number(node.id)}
															fallback={node.degradeCount}
															totals={qualityDegrades}
															onOpenLedger={setLedgerNodeID}
														/>
													</TableCell>
													<TableActionCell>
														<DropdownMenu>
															<DropdownMenuTrigger asChild>
																<Button
																	type="button"
																	variant="ghost"
																	size="icon"
																	className="size-8"
																	aria-label={t("common.actions")}
																>
																	<MoreHorizontal />
																</Button>
															</DropdownMenuTrigger>
															<DropdownMenuContent align="end">
																<DropdownMenuItem
																	onClick={() => openEdit(node)}
																>
																	<Pencil />
																	{t("common.edit")}
																</DropdownMenuItem>
																<DropdownMenuSeparator />
																{clearanceMode !== "manual" &&
																!node.accountBoundProxy ? (
																	<DropdownMenuItem
																		disabled={refreshClearance.isPending}
																		onClick={() =>
																			refreshClearance.mutate(node.id)
																		}
																	>
																		<RefreshCw />
																		{t("settings.egress.refreshClearance")}
																	</DropdownMenuItem>
																) : null}
																<DropdownMenuItem
																	disabled={
																		testNode.isPending || !node.proxyConfigured
																	}
																	onClick={() => testNode.mutate(node.id)}
																>
																	<RefreshCw />
																	{t("settings.egress.test")}
																</DropdownMenuItem>
																<DropdownMenuItem
																	disabled={!node.rotationConfigured}
																	onClick={() => rotateNode.mutate(node.id)}
																>
																	<RefreshCw />
																	{t("settings.egress.rotateExitIP")}
																</DropdownMenuItem>
																{node.quality?.state === "banned" ? (
																	<DropdownMenuItem
																		disabled={unbanNode.isPending}
																		onClick={() => unbanNode.mutate(node.id)}
																	>
																		<LockOpen />
																		{t("quality.egress.unban")}
																	</DropdownMenuItem>
																) : null}
																<DropdownMenuItem
																	className="text-destructive focus:text-destructive"
																	onClick={() => remove.mutate(node.id)}
																>
																	<Trash2 />
																	{t("common.delete")}
																</DropdownMenuItem>
															</DropdownMenuContent>
														</DropdownMenu>
													</TableActionCell>
												</TableRow>
											)}
										/>
									) : null}
								</Table>
							</div>
							<div className="divide-y md:hidden">
								{query.isPending ? (
									<div className="p-8">
										<Spinner />
									</div>
								) : nodes.length === 0 ? (
									<p className="p-8 text-center text-sm text-muted-foreground">
										{t("settings.egress.noMatches")}
									</p>
								) : (
									nodes.map((node) => (
										<article key={node.id} className="space-y-4 p-4">
											<div className="flex items-center gap-3">
												<Checkbox
													checked={selected.has(node.id)}
													onCheckedChange={(v) => toggleNode(node, v === true)}
													aria-label={t("common.selectItem", {
														name: node.name,
													})}
												/>
												<button
													onClick={() => openEdit(node)}
													className="min-w-0 flex-1 truncate text-left text-sm font-semibold"
												>
													{node.name}
												</button>
												<NodeStateLabel node={node} now={now} />
											</div>
											<div className="grid grid-cols-2 gap-4">
												<div>
													<p className="mb-1.5 text-[11px] text-muted-foreground">
														{t("ops.path")}
													</p>
													<ExitIPCell node={node} />
												</div>
												<div>
													<p className="mb-1.5 text-[11px] text-muted-foreground">
														{t("ops.latencyShort")}
													</p>
													<LatencyCell node={node} />
												</div>
											</div>
											<div className="flex flex-wrap items-center justify-between gap-2">
												<RotatableCell node={node} />
												<div className="flex items-center gap-1">
													<Button
														type="button"
														size="sm"
														variant="ghost"
														onClick={() => openEdit(node)}
													>
														{t("ops.openNode")}
													</Button>
													<DropdownMenu>
														<DropdownMenuTrigger asChild>
															<Button
																type="button"
																variant="ghost"
																size="icon"
																className="size-8"
																aria-label={t("common.actions")}
															>
																<MoreHorizontal />
															</Button>
														</DropdownMenuTrigger>
														<DropdownMenuContent align="end">
															<DropdownMenuItem onClick={() => openEdit(node)}>
																<Pencil />
																{t("common.edit")}
															</DropdownMenuItem>
															<DropdownMenuSeparator />
															{clearanceMode !== "manual" &&
															!node.accountBoundProxy ? (
																<DropdownMenuItem
																	disabled={refreshClearance.isPending}
																	onClick={() =>
																		refreshClearance.mutate(node.id)
																	}
																>
																	<RefreshCw />
																	{t("settings.egress.refreshClearance")}
																</DropdownMenuItem>
															) : null}
															<DropdownMenuItem
																disabled={
																	testNode.isPending || !node.proxyConfigured
																}
																onClick={() => testNode.mutate(node.id)}
															>
																<RefreshCw />
																{t("settings.egress.test")}
															</DropdownMenuItem>
															<DropdownMenuItem
																disabled={!node.rotationConfigured}
																onClick={() => rotateNode.mutate(node.id)}
															>
																<RefreshCw />
																{t("settings.egress.rotateExitIP")}
															</DropdownMenuItem>
															{node.quality?.state === "banned" ? (
																<DropdownMenuItem
																	disabled={unbanNode.isPending}
																	onClick={() => unbanNode.mutate(node.id)}
																>
																	<LockOpen />
																	{t("quality.egress.unban")}
																</DropdownMenuItem>
															) : null}
															<DropdownMenuItem
																className="text-destructive focus:text-destructive"
																onClick={() => remove.mutate(node.id)}
															>
																<Trash2 />
																{t("common.delete")}
															</DropdownMenuItem>
														</DropdownMenuContent>
													</DropdownMenu>
												</div>
											</div>
											{node.quality?.caseId && (
												<Link
													to={`/guard?case=${node.quality.caseId}#tribunal`}
													className="block text-xs text-primary"
												>
													{t("ops.inspectCase")} #{node.quality.caseId}
												</Link>
											)}
										</article>
									))
								)}
							</div>
						</>
					) : null}
				</DataTableShell>
			</div>

			<Dialog
				open={ledgerNodeID !== null}
				onOpenChange={(open) => {
					if (!open) setLedgerNodeID(null);
				}}
			>
				<DialogContent className="sm:max-w-[520px]">
					<DialogHeader>
						<DialogTitle>
							{t("quality.egressView.history")} · #{ledgerNodeID ?? ""}
						</DialogTitle>
						<DialogDescription>
							{t("quality.egressView.ledgerHelp")}
						</DialogDescription>
					</DialogHeader>
					{(() => {
						const view = (qualityNodesQuery.data ?? []).find(
							(node) => node.node_id === ledgerNodeID,
						);
						const entries = view?.degrade_detail ?? [];
						if (entries.length === 0) {
							return (
								<p className="py-6 text-center text-sm text-muted-foreground">
									{t("quality.egressView.noHistory")}
								</p>
							);
						}
						return (
							<div className="max-h-[50vh] space-y-1.5 overflow-y-auto">
								{entries.map((entry, index) => (
									<div
										key={index}
										className="flex flex-wrap items-center gap-x-4 gap-y-0.5 rounded-md border px-3 py-2 font-mono text-[11px] text-muted-foreground"
									>
										<span>epoch {entry.epoch}</span>
										<span>{maskIP(entry.ip)}</span>
										<span
											className={
												entry.count > 2
													? "font-medium text-amber-600 dark:text-amber-400"
													: undefined
											}
										>
											×{entry.count}
										</span>
										<span className="ml-auto">
											{new Date(entry.first_at).toLocaleString()} →{" "}
											{new Date(entry.last_at).toLocaleString()}
										</span>
									</div>
								))}
							</div>
						);
					})()}
				</DialogContent>
			</Dialog>

			<Dialog open={sourcesOpen} onOpenChange={setSourcesOpen}>
				<DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[640px]">
					<DialogHeader>
						<DialogTitle>{t("proxies.supply.title")}</DialogTitle>
					</DialogHeader>
					<SubscriptionsPanel showHeader={false} />
				</DialogContent>
			</Dialog>

			<Dialog open={batchRotationOpen} onOpenChange={setBatchRotationOpen}>
				<DialogContent className="sm:max-w-lg">
					<DialogHeader>
						<DialogTitle>{t("settings.egress.batchRotation")}</DialogTitle>
						<DialogDescription>
							{t("settings.egress.batchRotationDescription", {
								count: selected.size,
							})}
						</DialogDescription>
					</DialogHeader>
					<div className="space-y-2">
						<Input
							value={batchRotationTemplate}
							placeholder="http://203.0.113.10:9000/rotate/{port}?token=xxx"
							onChange={(event) => setBatchRotationTemplate(event.target.value)}
						/>
						<p className="text-xs leading-5 text-muted-foreground">
							{t("settings.egress.batchRotationHelp")}
						</p>
					</div>
					<DialogFooter>
						<Button
							type="button"
							variant="secondary"
							size="sm"
							onClick={() => setBatchRotationOpen(false)}
						>
							{t("common.cancel")}
						</Button>
						<Button
							type="button"
							size="sm"
							disabled={batchRotation.isPending}
							onClick={() => batchRotation.mutate()}
						>
							{batchRotation.isPending ? <Spinner /> : null}
							{t("common.save")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>

			<AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							{t("settings.egress.batchDeleteTitle", { count: selected.size })}
						</AlertDialogTitle>
						<AlertDialogDescription className="space-y-1">
							<span className="block">
								{t("settings.egress.batchDeleteDescription", {
									count: selected.size,
								})}
							</span>
							{selectedSourceNodes > 0 ? (
								<span className="block">
									{t("settings.egress.batchDeleteSourceHint", {
										count: selectedSourceNodes,
									})}
								</span>
							) : null}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={removeMany.isPending}>
							{t("common.cancel")}
						</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={removeMany.isPending}
							onClick={(event) => {
								event.preventDefault();
								removeMany.mutate();
							}}
						>
							{removeMany.isPending ? <Spinner /> : null}
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>

			<AlertDialog
				open={cleanupOpen}
				onOpenChange={(open) => {
					if (!open && cleanupUnhealthy.isPending) return;
					if (!open) cleanupPreview.reset();
					setCleanupOpen(open);
				}}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							{t("settings.egress.cleanupUnavailableTitle")}
						</AlertDialogTitle>
						<AlertDialogDescription className="space-y-2">
							<span className="block">
								{t("settings.egress.cleanupUnavailableDescription")}
							</span>
							<span className="block">
								{t("settings.egress.cleanupUnavailableImpact")}
							</span>
						</AlertDialogDescription>
					</AlertDialogHeader>
					<div className="min-h-20 rounded-md bg-muted/45 p-3 text-xs">
						{cleanupPreview.isPending ? (
							<div className="flex h-14 items-center justify-center gap-2 text-muted-foreground">
								<Spinner />
								{t("settings.egress.cleanupPreviewLoading")}
							</div>
						) : null}
						{cleanupPreview.isError ? (
							<div className="flex h-14 items-center justify-center text-center text-destructive">
								{t("settings.egress.cleanupPreviewFailed")}
							</div>
						) : null}
						{cleanupPreview.data ? (
							<div className="grid grid-cols-2 gap-3 text-center">
								<CleanupPreviewValue
									label={t("settings.egress.cleanupNodeCount")}
									value={cleanupPreview.data.nodes}
								/>
								<CleanupPreviewValue
									label={t("settings.egress.cleanupSubscriptionCount")}
									value={cleanupPreview.data.subscriptionManaged}
								/>
							</div>
						) : null}
					</div>
					{cleanupPreview.data &&
					cleanupPreview.data.subscriptionManaged > 0 ? (
						<p className="text-xs leading-5 text-amber-700 dark:text-amber-300">
							{t("settings.egress.cleanupSubscriptionHint")}
						</p>
					) : null}
					<AlertDialogFooter>
						<AlertDialogCancel disabled={cleanupUnhealthy.isPending}>
							{t("common.cancel")}
						</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={
								cleanupUnhealthy.isPending ||
								!cleanupPreview.data ||
								cleanupPreview.data.nodes === 0
							}
							onClick={(event) => {
								event.preventDefault();
								cleanupUnhealthy.mutate();
							}}
						>
							{cleanupUnhealthy.isPending ? <Spinner /> : null}
							{t("settings.egress.cleanupUnavailableConfirm")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>

			<Dialog
				open={editing !== undefined}
				onOpenChange={(open) => {
					if (!open) setEditing(undefined);
				}}
			>
				<DialogContent className="ops-editor-dialog sm:max-w-[720px]">
					<DialogHeader className="border-b px-6 py-5 pr-12 text-left">
						<DialogTitle>
							{editing ? editing.name : t("settings.egress.addTitle")}
						</DialogTitle>
						<DialogDescription>{t("ops.changeSavedHelp")}</DialogDescription>
					</DialogHeader>
					<form
						className="flex min-h-0 flex-1 flex-col"
						onSubmit={(event) => {
							event.preventDefault();
							event.stopPropagation();
							save.mutate();
						}}
					>
						<div className="min-h-0 flex-1 overflow-y-auto px-6 py-2">
							{editing && (
								<div className="flex flex-wrap items-center gap-3 border-b py-3 text-xs">
									<StatusPill
										tone={
											nodeNeedsAttention(editing, now)
												? "warn"
												: editing.enabled
													? "good"
													: "neutral"
										}
									>
										{t(`ops.condition.${nodeCondition(editing, now)}`)}
									</StatusPill>
									<span className="font-mono text-muted-foreground">
										{editing.exitIp || "—"}
									</span>
									{editing.quality?.caseId && (
										<Link
											className="ml-auto underline underline-offset-4"
											to={`/guard?case=${editing.quality.caseId}#tribunal`}
										>
											{t("ops.inspectCase")} #{editing.quality.caseId}
										</Link>
									)}
								</div>
							)}
							<fieldset className="ops-fieldset">
								<legend>{t("ops.nodeBasics")}</legend>
								<OperationsField
									label={t("settings.egress.name")}
									controlId="egress-name"
								>
									<Input
										id="egress-name"
										maxLength={160}
										value={form.name}
										onChange={(event) =>
											setForm({ ...form, name: event.target.value })
										}
									/>
								</OperationsField>
								<OperationsField
									label={t("settings.egress.proxyURL")}
									controlId="egress-proxy"
									description={t("settings.egress.proxyProtocols")}
								>
									<Input
										id="egress-proxy"
										autoComplete="off"
										placeholder="socks5h://user:pass@host:port"
										value={form.proxyURL}
										onChange={(event) => {
											const proxyURL = event.target.value,
												hasProxy =
													Boolean(editing?.proxyConfigured) ||
													Boolean(proxyURL.trim());
											setForm({
												...form,
												proxyURL,
												proxyPool: hasProxy ? form.proxyPool : false,
												rotationURL: hasProxy ? form.rotationURL : "",
												rotationOpen: hasProxy ? form.rotationOpen : false,
												rotationEnabled: hasProxy
													? form.rotationEnabled
													: false,
											});
										}}
									/>
								</OperationsField>
								<OperationsField
									label={t("settings.egress.enabled")}
									controlId="egress-enabled"
								>
									<Switch
										id="egress-enabled"
										checked={form.enabled}
										onCheckedChange={(enabled) => setForm({ ...form, enabled })}
									/>
								</OperationsField>
							</fieldset>
							<fieldset className="ops-fieldset">
								<legend>{t("ops.nodeRecovery")}</legend>
								<OperationsField
									label={t("ops.nodeBehavior")}
									controlId="egress-kind"
									description={t(
										form.proxyPool
											? "settings.egress.proxyPoolNoRotation"
											: form.rotationOpen
												? "settings.egress.rotationHelp"
												: "ops.fixedExitHelp",
									)}
								>
									<Select
										value={
											form.proxyPool
												? "dynamic"
												: form.rotationOpen
													? "rotating"
													: "fixed"
										}
										disabled={
											!editing?.proxyConfigured && !form.proxyURL?.trim()
										}
										onValueChange={(value) =>
											setForm({
												...form,
												proxyPool: value === "dynamic",
												rotationOpen: value === "rotating",
												rotationURL:
													value === "rotating" ? form.rotationURL : "",
												rotationEnabled:
													value === "rotating" &&
													(Boolean(form.rotationURL) ||
														Boolean(editing?.rotationConfigured)),
											})
										}
									>
										<SelectTrigger id="egress-kind">
											<SelectValue />
										</SelectTrigger>
										<SelectContent>
											<SelectItem value="fixed">
												{t("ops.fixedExit")}
											</SelectItem>
											<SelectItem value="dynamic">
												{t("ops.dynamicExit")}
											</SelectItem>
											<SelectItem value="rotating">
												{t("ops.rotatingExit")}
											</SelectItem>
										</SelectContent>
									</Select>
								</OperationsField>
								{form.rotationOpen && (
									<>
										<OperationsField
											label={t("settings.egress.webhook")}
											controlId="egress-rotation"
											description={t("ops.rotationEndpointHelp")}
										>
											<Input
												id="egress-rotation"
												autoComplete="off"
												placeholder={
													editing?.rotationConfigured &&
													!form.rotationURL?.trim()
														? t("settings.egress.keepConfigured")
														: "https://host/rotate"
												}
												value={form.rotationURL}
												onChange={(event) => {
													const rotationURL = event.target.value.trim();
													setForm({
														...form,
														rotationURL,
														rotationEnabled:
															Boolean(rotationURL) ||
															Boolean(editing?.rotationConfigured),
													});
												}}
											/>
										</OperationsField>
										<OperationsField
											label={t("ops.allowRotation")}
											controlId="egress-rotation-enabled"
											description={t("ops.allowRotationHelp")}
										>
											<Switch
												id="egress-rotation-enabled"
												checked={form.rotationEnabled}
												onCheckedChange={(rotationEnabled) =>
													setForm({ ...form, rotationEnabled })
												}
											/>
										</OperationsField>
									</>
								)}
							</fieldset>
						</div>
						<DialogFooter className="border-t px-6 py-4">
							<Button
								type="button"
								variant="outline"
								size="sm"
								onClick={() => setEditing(undefined)}
							>
								{t("common.cancel")}
							</Button>
							<Button
								type="submit"
								size="sm"
								disabled={!form.name.trim() || save.isPending}
							>
								{save.isPending && <Spinner />}
								{t("common.save")}
							</Button>
						</DialogFooter>
					</form>
				</DialogContent>
			</Dialog>

			<Dialog open={importOpen} onOpenChange={setImportOpen}>
				<DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[620px]">
					<DialogHeader className="pr-8">
						<DialogTitle>{t("settings.egress.importText")}</DialogTitle>
						<DialogDescription>
							{t("settings.egress.importDialogDescription")}
						</DialogDescription>
					</DialogHeader>
					<form
						className="space-y-3.5"
						onSubmit={(event) => {
							event.preventDefault();
							event.stopPropagation();
							importText.mutate();
						}}
					>
						<Field
							label={t("settings.egress.name")}
							controlId="egress-import-name"
						>
							<Input
								id="egress-import-name"
								maxLength={160}
								value={importForm.name}
								onChange={(event) =>
									setImportForm({ ...importForm, name: event.target.value })
								}
							/>
						</Field>
						<Field
							label={t("settings.egress.proxyList")}
							controlId="egress-import-list"
						>
							<Textarea
								className="min-h-52 font-mono text-xs"
								id="egress-import-list"
								value={importForm.content}
								onChange={(event) =>
									setImportForm({ ...importForm, content: event.target.value })
								}
							/>
						</Field>
						<DialogFooter>
							<Button
								type="button"
								size="sm"
								variant="secondary"
								onClick={() => setImportOpen(false)}
							>
								{t("common.cancel")}
							</Button>
							<Button
								type="submit"
								size="sm"
								disabled={
									!importForm.name.trim() ||
									!importForm.content.trim() ||
									importText.isPending
								}
							>
								{importText.isPending ? <Spinner /> : null}
								{t("settings.egress.importText")}
							</Button>
						</DialogFooter>
					</form>
				</DialogContent>
			</Dialog>
		</div>
	);
}

function CleanupPreviewValue({
	label,
	value,
}: {
	label: string;
	value: number;
}) {
	return (
		<div className="space-y-1">
			<div className="text-base font-medium tabular-nums text-foreground">
				{value}
			</div>
			<div className="text-muted-foreground">{label}</div>
		</div>
	);
}

function ErrorTooltip({ message }: { message: string }) {
	return (
		<Tooltip>
			<TooltipTrigger asChild>
				<span
					className="inline-flex shrink-0 cursor-help text-destructive"
					tabIndex={0}
					aria-label={message}
				>
					<CircleAlert className="size-3.5" />
				</span>
			</TooltipTrigger>
			<TooltipContent className="max-w-80">{message}</TooltipContent>
		</Tooltip>
	);
}

function NodeStateLabel({ node, now }: { node: EgressNodeDTO; now: number }) {
	const { t } = useTranslation();
	const condition = nodeCondition(node, now);
	const tone = ["banned", "unhealthy"].includes(condition)
		? "bad"
		: ["held", "cooling", "unconfigured"].includes(condition)
			? "warn"
			: ["ready", "dynamic"].includes(condition)
				? "good"
				: "neutral";
	return <StatusPill tone={tone}>{t(`ops.condition.${condition}`)}</StatusPill>;
}
function RotatableCell({ node }: { node: EgressNodeDTO }) {
	const { t } = useTranslation();
	return (
		<span className="text-xs text-muted-foreground">
			{t(
				node.rotatingEndpoint
					? "ops.recoveryDynamic"
					: node.rotationConfigured
						? node.rotationEnabled
							? "ops.recoveryAuto"
							: "ops.recoveryPaused"
						: "ops.recoveryManual",
			)}
		</span>
	);
}
/** Latency column: probe latency of the healthy family, em-dash when unknown. */
function LatencyCell({ node }: { node: EgressNodeDTO }) {
	const { t } = useTranslation();
	const probe =
		node.ipv4Probe?.status === "healthy"
			? node.ipv4Probe
			: node.ipv6Probe?.status === "healthy"
				? node.ipv6Probe
				: undefined;
	if (probe && probe.latencyMs) {
		return (
			<span
				className="text-xs tabular-nums text-muted-foreground"
				title={t("proxies.nodes.latencyHelp")}
			>
				{probe.latencyMs} ms
			</span>
		);
	}
	const probed =
		node.ipv4Probe?.status !== "unknown" ||
		node.ipv6Probe?.status !== "unknown";
	return (
		<span
			className="text-[10px] text-muted-foreground/70"
			title={
				probed
					? t("proxies.nodes.latencyUnhealthy")
					: t("proxies.nodes.latencyUntested")
			}
		>
			{probed
				? t("proxies.nodes.latencyUnhealthy")
				: t("proxies.nodes.latencyUntested")}
		</span>
	);
}
/** Compact exit-IP cell: IPv4 status + address; full dual-stack
 *  detail stays in the tooltip so the column stays narrow. */
function ExitIPCell({ node }: { node: EgressNodeDTO }) {
	const { t } = useTranslation();
	const probe =
		node.ipv4Probe?.status === "healthy"
			? node.ipv4Probe
			: node.ipv6Probe?.status === "healthy"
				? node.ipv6Probe
				: (node.ipv4Probe ?? node.ipv6Probe);
	if (!probe || probe.status === "unknown") {
		return (
			<span className="text-[10px] text-muted-foreground/70">
				{t("settings.egress.notTested")}
			</span>
		);
	}
	const healthy = probe.status === "healthy";
	const detail = [
		node.ipv4Probe?.exitIp
			? "IPv4 " +
				node.ipv4Probe.exitIp +
				(node.ipv4Probe.error ? " · " + node.ipv4Probe.error : "")
			: null,
		node.ipv6Probe?.exitIp
			? "IPv6 " +
				node.ipv6Probe.exitIp +
				(node.ipv6Probe.error ? " · " + node.ipv6Probe.error : "")
			: null,
		node.ipv4Probe?.error && node.ipv4Probe?.status !== "healthy"
			? "IPv4: " + node.ipv4Probe.error
			: null,
		node.ipv6Probe?.error && node.ipv6Probe?.status !== "healthy"
			? "IPv6: " + node.ipv6Probe.error
			: null,
	]
		.filter(Boolean)
		.join(String.fromCharCode(10));
	return (
		<Tooltip>
			<TooltipTrigger asChild>
				<span
					className={cn(
						"flex min-w-0 cursor-help items-center justify-start gap-1.5 text-xs",
						healthy ? "text-foreground" : "text-destructive",
					)}
				>
					<span
						className={cn(
							"size-1.5 shrink-0 rounded-full",
							healthy ? "bg-emerald-500" : "bg-red-500",
						)}
					/>
					<span className="truncate font-mono text-xs tabular-nums">
						{probe.exitIp ||
							(healthy
								? t("settings.egress.healthy")
								: t("settings.egress.unhealthy"))}
					</span>
				</span>
			</TooltipTrigger>
			<TooltipContent className="max-w-80 whitespace-pre-line">
				{detail || t("settings.egress.probeHelp")}
			</TooltipContent>
		</Tooltip>
	);
}
function Field({
	label,
	controlId,
	description,
	help,
	children,
}: {
	label: string;
	controlId: string;
	description?: string;
	help?: string;
	children: ReactNode;
}) {
	return (
		<OperationsField
			label={label}
			controlId={controlId}
			description={description ?? help}
		>
			{children}
		</OperationsField>
	);
}

function showError(error: unknown, fallback: string) {
	toast.error(error instanceof Error ? error.message : fallback);
}

// DegradeBadge 降智徽章(G8):计数取质量层台账(单一数据源),台账无
// 此节点时回退底座历史计数;点击弹出 IP 历史明细(代理网络=出口唯一
// 管理面,台账详情归口于此)。
function DegradeBadge({
	nodeID,
	fallback,
	totals,
	onOpenLedger,
}: {
	nodeID: number;
	fallback: number;
	totals: Map<number, number>;
	onOpenLedger: (nodeID: number) => void;
}) {
	const { t } = useTranslation();
	const total = totals.get(nodeID) ?? fallback;
	if (total <= 0) {
		return null;
	}
	return (
		<button
			type="button"
			onClick={() => onOpenLedger(nodeID)}
			title={t("quality.egressView.history")}
			className="shrink-0 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
		>
			<Badge
				variant="outline"
				className="border-transparent text-[11px] text-muted-foreground hover:border-border hover:text-primary"
			>
				{t("ops.historyCount", { count: total })}
			</Badge>
		</button>
	);
}
