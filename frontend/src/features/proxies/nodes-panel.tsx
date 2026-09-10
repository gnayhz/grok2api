import { NetworkField as OperationsField } from "./network-ui";
import {
	NetworkDialogContent as DialogContent,
	NetworkDialogHeader as DialogHeader,
	NetworkDialogFooter as DialogFooter,
	NetworkSelect,
	NetworkTooltip,
	NetworkText,
	NetworkError,
} from "./network-ui";

import { AlertDialogContent } from "@/components/ui/alert-dialog";
import "./resources-panel.css";
import {
	networkSummary,
	nodeCondition,
	nodeNeedsAttention,
} from "@/features/operations/operations-data";
import { useOperationsNodes } from "@/features/operations/operations-queries";
import { SubscriptionsPanel } from "./subscriptions-panel";
import {
	Table,
	TableHeader,
	TableHead,
	TableBody,
	TableRow,
	TableCell,
	TableActionHead,
	TableActionCell,
} from "@/components/ui/table";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { StatusPill, OperationsHelp } from "@/features/operations/operations-ui";
import { useNow } from "@/features/guard/quality-hooks";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchQualityNodes, unbanQualityNode } from "@/features/guard/quality-api";
import { maskIP } from "@/features/guard/quality-view";
import {
	ArrowUpRight,
	MapPin,
	Shuffle,
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
import { type ReactNode, memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
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
import { NetworkButton as Button } from "./network-ui";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogDescription, DialogTitle } from "@/components/ui/dialog";
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
import { Textarea } from "@/components/ui/textarea";
import { ProbeSettingsButton } from "@/features/proxies/probe-settings";
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
import { ErrorState } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const noNodes: EgressNodeDTO[] = [];
const resourceConditions = ["all", "attention", "ready", "unknown", "disabled", "restricted", "held", "banned", "unhealthy", "cooling", "unconfigured"];
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
type NodeEditorSession = {
	controller: AbortController;
	proxyEdited: boolean;
	rotationEdited: boolean;
};
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

export const NodesPanel = memo(function NodesPanel({
	sourcesOpen,
	onManageSources,
	onShowNodes,
	initialCondition = "all",
	focusKey,
}: {
	sourcesOpen: boolean;
	onShowNodes: () => void;
	onManageSources: () => void;
	initialCondition?: string;
	focusKey?: string;
}) {
	const now = useNow(15000);
	const summaryQuery = useOperationsNodes();
	const summary = networkSummary(summaryQuery.data?.items ?? [], now);
	const [conditionFilter, setConditionFilter] = useState(resourceConditions.includes(initialCondition) ? initialCondition : "all");
	const [filterRequest, setFilterRequest] = useState({ initialCondition, focusKey });
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
	const [deletingNode, setDeletingNode] = useState<EgressNodeDTO | null>(null);
	const [editing, setEditing] = useState<EgressNodeDTO | null>(null);
	const [inspecting, setInspecting] = useState<EgressNodeDTO | null>(null);
	const [editorOpen, setEditorOpen] = useState(false);
	const editorSession = useRef<NodeEditorSession | null>(null);
	useEffect(() => () => { editorSession.current?.controller.abort(); editorSession.current = null; }, []);
	const beginEditor = useCallback(() => {
		editorSession.current?.controller.abort();
		const session = {
			controller: new AbortController(),
			proxyEdited: false,
			rotationEdited: false,
		};
		editorSession.current = session;
		return session;
	}, []);
	const closeEditor = useCallback(() => {
		editorSession.current?.controller.abort();
		editorSession.current = null;
		setEditorOpen(false);
	}, []);
	const [revealedProxyURL, setRevealedProxyURL] = useState("");
	const [revealedRotationURL, setRevealedRotationURL] = useState("");
	const [importOpen, setImportOpen] = useState(false);
	const importSession = useRef<object | null>(null);
	useEffect(() => () => { importSession.current = null; }, []);
	const [importForm, setImportForm] = useState<ImportForm>(emptyImport);
	const [form, setForm] = useState<NodeForm>(emptyForm);
	const [page, setPage] = useState(1);
	const [pageSize, setPageSize] = useState(20);
	const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
	const [search, setSearch] = useState("");
	const [enabledFilter, setEnabledFilter] = useState("");
	const [probeFilter, setProbeFilter] = useState("");
	const [selected, setSelected] = useState<Map<string, EgressNodeDTO>>(() => new Map());
	const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
	const [cleanupOpen, setCleanupOpen] = useState(false);
	if (filterRequest.initialCondition !== initialCondition || filterRequest.focusKey !== focusKey) {
		setFilterRequest({ initialCondition, focusKey });
		setConditionFilter(resourceConditions.includes(initialCondition) ? initialCondition : "all");
		setPage(1);
		setSearch("");
		setEnabledFilter("");
		setProbeFilter("");
		setSelected(new Map());
	}
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
	const filteredNodes = useMemo(() => {
		const searchTerm = debouncedSearch.trim().toLocaleLowerCase();
		const filtered = (summaryQuery.data?.items ?? noNodes).filter((node) => {
			const state = nodeCondition(node, now);
			if (conditionFilter === "attention" && !nodeNeedsAttention(node, now)) return false;
			if (conditionFilter === "restricted" && !["held", "banned"].includes(state)) return false;
			if (conditionFilter === "ready" && !["ready", "dynamic"].includes(state)) return false;
			if (conditionFilter === "disabled" && state !== "disabled") return false;
			if (conditionFilter === "unknown" && state !== "unknown") return false;
			if (["held", "banned", "unhealthy", "cooling", "unconfigured"].includes(conditionFilter) && state !== conditionFilter) return false;
			const searchable = [node.name, node.proxyDisplay, node.exitIp, node.ipv4Probe.exitIp, node.ipv6Probe.exitIp, node.sourceName, ...(node.pools ?? []).map((pool) => pool.name)].filter(Boolean).join(" ").toLocaleLowerCase();
			if (searchTerm && !searchable.includes(searchTerm)) return false;
			if (enabledFilter && node.enabled !== (enabledFilter === "enabled")) return false;
			return !probeFilter || node.probeStatus === probeFilter;
		});
		if (sort.field) filtered.sort((a, b) => a.name.localeCompare(b.name) * (sort.order === "desc" ? -1 : 1));
		return filtered;
	}, [summaryQuery.data, now, conditionFilter, debouncedSearch, enabledFilter, probeFilter, sort]);
	const currentPage = Math.min(page, Math.max(1, Math.ceil(filteredNodes.length / pageSize)));
	const pageNodes = useMemo(() => filteredNodes.slice((currentPage - 1) * pageSize, currentPage * pageSize), [filteredNodes, currentPage, pageSize]);
	const query = {
		...summaryQuery,
		data: summaryQuery.data ? { items: pageNodes, page: currentPage, pageSize, total: filteredNodes.length } : undefined,
	};
	const inspectedNode = inspecting ? (summaryQuery.data?.items.find((node) => node.id === inspecting.id) ?? inspecting) : null;
	const save = useMutation({
		mutationFn: ({
			editing,
			form,
			revealedProxyURL,
			revealedRotationURL,
		}: {
			editing: EgressNodeDTO | null;
			form: NodeForm;
			revealedProxyURL: string;
			revealedRotationURL: string;
			session: NodeEditorSession;
		}) => {
			const normalizedProxyURL = form.proxyURL?.trim() || "";
			const trimmedRotationURL = form.rotationOpen ? form.rotationURL?.trim() || "" : "";
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
					normalizedProxyURL && (!editing || normalizedProxyURL !== revealedProxyURL)
						? normalizedProxyURL
						: undefined,
				clearProxyURL: !normalizedProxyURL && revealedProxyURL ? true : undefined,
				// 换 IP webhook 与代理地址同语义:仅在确认已有存量(reveal 已返回)且被清空时
				// 才显式清除;reveal 未返回/失败时保持存量, 避免快速保存把已配置的 webhook
				// 静默删除。开关关闭=暂停(保留 webhook), 未触及=不修改。
				rotationURL:
					form.rotationOpen && trimmedRotationURL && trimmedRotationURL !== revealedRotationURL
						? trimmedRotationURL
						: undefined,
				clearRotationURL:
					form.rotationOpen && !trimmedRotationURL && revealedRotationURL ? true : undefined,
				// 开关=启用换 IP 轮换;代理池与轮换互斥(勾代理池时 UI 强制关闭并清零)。
				// 旧逻辑"开关开+存量URL未重填"会送 false,悄悄停用已配置的轮换。
				rotationEnabled: form.rotationOpen && form.rotationEnabled,
			};
			return editing ? updateEgressNode(editing.id, input) : createEgressNode(input);
		},
		onSuccess: (_, submitted) => {
			invalidateAfterNodeChange();
			if (editorSession.current === submitted.session) closeEditor();
			toast.success(t("settings.egress.saved"));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const importText = useMutation({
		mutationFn: ({ input }: { input: ImportForm; session: object }) => importEgressText(input),
		onSuccess: (value, submitted) => {
			invalidateAfterNodeChange();
			if (importSession.current === submitted.session) {
				importSession.current = null;
				setImportOpen(false);
			}
			toast.success(t("settings.egress.imported", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const remove = useMutation({
		mutationFn: deleteEgressNode,
		onSuccess: (_, id) => {
			setDeletingNode(null);
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
		mutationFn: (enabled: boolean) => updateEgressNodesEnabled([...selected.keys()], enabled),
		onSuccess: (value, enabled) => {
			setSelected(new Map());
			invalidateAfterNodeChange();
			toast.success(
				t(enabled ? "settings.egress.batchEnabled" : "settings.egress.batchDisabled", value),
			);
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const updateOneEnabled = useMutation({
		mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => updateEgressNodesEnabled([id], enabled),
		onSuccess: () => invalidateAfterNodeChange(),
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
			toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
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
			const targets = [...selected.values()].filter((node) => node.rotationConfigured);
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
		mutationFn: () => batchSetEgressRotation([...selected.keys()], batchRotationTemplate),
		onSuccess: (value) => {
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			setBatchRotationOpen(false);
			toast.success(t("settings.egress.batchRotationDone", value));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
	});
	const testAll = useMutation({
		mutationFn: (ids?: string[]) => ids ? probeAllEnabledNodes(ids) : testAllEgressNodes(),
		onSuccess: (value) => {
			if (value.failed > 0) toast.warning(t("settings.egress.testedPartial", value));
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
			if (result.status === "healthy") toast.success(t("settings.egress.testedOne"));
			else toast.error(result.error || t("settings.egress.operationFailed"));
		},
		onError: (error) => showError(error, t("settings.egress.operationFailed")),
		onSettled: () => {
			invalidateAfterNodeChange();
		},
	});

	function openCreate() {
		beginEditor();
		setRevealedProxyURL("");
		setRevealedRotationURL("");
		setForm(emptyForm);
		setEditing(null);
		setEditorOpen(true);
	}

	function openCleanup() {
		cleanupPreview.reset();
		setCleanupOpen(true);
		cleanupPreview.mutate();
	}

	const openEdit = useCallback(
		(node: EgressNodeDTO) => {
			const session = beginEditor();
			setForm({
				name: node.name,
				enabled: node.enabled,
				proxyPool: node.proxyPool,
				proxyURL: "",
				rotationURL: "",
				rotationEnabled: node.rotationConfigured && (node.rotationEnabled ?? true),
				rotationOpen: node.rotationConfigured,
			});
			setRevealedProxyURL("");
			setRevealedRotationURL("");
			setEditing(node);
			setEditorOpen(true);
			// Proxy address and rotation webhook are plain editable fields: fetch the
			// stored values once so operators can see and delete them without a
			// reveal dance. Shared-profile nodes keep their managed address hidden.
			if (node.proxyConfigured) {
				getEgressNodeProxyURL(node.id, session.controller.signal)
					.then(({ proxyURL }) => {
						if (session.controller.signal.aborted) return;
						setRevealedProxyURL(proxyURL);
						setForm((current) => (!session.proxyEdited ? { ...current, proxyURL } : current));
					})
					.catch(() => undefined);
			}
			if (node.rotationConfigured) {
				getEgressNodeRotationURL(node.id, session.controller.signal)
					.then(({ rotationURL }) => {
						if (session.controller.signal.aborted) return;
						setRevealedRotationURL(rotationURL);
						setForm((current) => (!session.rotationEdited ? { ...current, rotationURL } : current));
					})
					.catch(() => undefined);
			}
		},
		[beginEditor],
	);

	const changeSort = useCallback((field: string, initialOrder: SortOrder): void => {
		setSort((current) => nextTableSort(current, field, initialOrder));
		setPage(1);
	}, []);

	const nodes = query.data?.items ?? noNodes;
	const togglePage = useCallback(
		(checked: boolean): void => {
			setSelected((current) => {
				const next = new Map(current);
				for (const node of nodes) {
					if (checked) next.set(node.id, node);
					else next.delete(node.id);
				}
				return next;
			});
		},
		[nodes],
	);

	const toggleNode = useCallback((node: EgressNodeDTO, checked: boolean): void => {
		setSelected((current) => {
			const next = new Map(current);
			if (checked) next.set(node.id, node);
			else next.delete(node.id);
			return next;
		});
	}, []);
	const selectedOnPage = nodes.filter((node) => selected.has(node.id));
	const allPageSelected = nodes.length > 0 && selectedOnPage.length === nodes.length;
	const selectedNodes = [...selected.values()];
	const selectedSourceNodes = selectedNodes.filter((node) => node.sourceId).length;
	const batchPending = removeMany.isPending || updateManyEnabled.isPending;
	const hasActiveFilters = Boolean(
		debouncedSearch || enabledFilter || probeFilter || conditionFilter !== "all",
	);

	const { mutate: refreshNodeClearance, isPending: refreshingClearance } = refreshClearance;
	const { mutate: probeNode, isPending: probingNode } = testNode;
	const { mutate: rotateExit } = rotateNode;
	const { mutate: unbanExit, isPending: unbanningNode } = unbanNode;
	const { mutate: toggleExit, isPending: togglingExit } = updateOneEnabled;
	// Editing a dialog does not change the inventory. Reuse its tree until a
	// row, selection, layout or operation state actually changes.
	const inventory = useMemo(
		() => (
			<Table className="nres-table" viewportRows={20} rowHeight={80}>
				<TableHeader>
					<TableRow>
						<TableHead className="w-10">
							<Checkbox
								checked={allPageSelected ? true : selectedOnPage.length ? "indeterminate" : false}
								disabled={!nodes.length}
								onCheckedChange={(checked) => togglePage(checked === true)}
								aria-label={t("common.selectPage")}
							/>
						</TableHead>
						<SortableTableHead
							className="nres-name-column"
							field="name"
							sortBy={sort.field}
							sortOrder={sort.order}
							onSort={changeSort}
						>
							{t("networkResources.exitSource")}
						</SortableTableHead>
						<TableHead className="nres-state-column">{t("ops.nodeState")}</TableHead>
						<TableHead className="nres-address-column">
							<span className="inline-flex items-center gap-1">
								{t("networkResources.exitAddress")}
								<OperationsHelp label={t("ops.path")}>
									{t("ops.netProbeAddressHelp")}
								</OperationsHelp>
							</span>
						</TableHead>
						<TableHead className="w-28">{t("networkResources.probeLatency")}</TableHead>
						<TableHead className="w-28">{t("ops.recovery")}</TableHead>
						<TableActionHead />
					</TableRow>
				</TableHeader>
				{query.isPending || !nodes.length ? (
					<TableBody>
						<TableRow>
							<TableCell colSpan={7} className="h-32 text-center text-xs text-muted-foreground">
								{query.isPending ? (
									<Spinner className="mx-auto" />
								) : hasActiveFilters ? (
									t("settings.egress.noMatches")
								) : (
									t("ops.noNodesHelp")
								)}
							</TableCell>
						</TableRow>
					</TableBody>
				) : (
					<VirtualTableBody
						items={nodes}
						colSpan={7}
						rowHeight={80}
						renderRow={(node) => (
							<TableRow
								key={node.id}
								className="nres-row group"
								data-state={selected.has(node.id) ? "selected" : undefined}
							>
								<TableCell>
									<Checkbox
										checked={selected.has(node.id)}
										onCheckedChange={(checked) => toggleNode(node, checked === true)}
										aria-label={t("common.selectItem", { name: node.name })}
									/>
								</TableCell>
								<TableCell>
									<div className="flex min-w-0 items-center gap-1.5">
										<button
											type="button"
											className="nres-node-name"
											onClick={() => setInspecting(node)}
										>
											<NetworkText>{node.name}</NetworkText>
										</button>
										{node.lastError && <NetworkError message={node.lastError} />}
									</div>
									<div className="nres-node-origin">{node.sourceId ? (
										<button
											type="button"
											className="nres-origin-link"
											onClick={onManageSources}
										>
											<NetworkText>{node.sourceName || t("ops.netSources")}</NetworkText>
										</button>
									) : <span>{t("networkResources.manualSource")}</span>}
									{Boolean(node.pools?.length) && <><span aria-hidden="true">·</span><Link className="nres-origin-link" to={`/proxies?pool=${node.pools![0]!.id}#pools`}><NetworkText>{node.pools!.map((pool) => pool.name).join(" · ")}</NetworkText></Link></>}
									</div>
								</TableCell>
								<TableCell>
									<div className="flex items-center gap-1.5">
										<NodeStateLabel node={node} now={now} />
										{node.quality?.caseId && (
											<Link
												to={`/guard?case=${node.quality.caseId}#tribunal`}
												aria-label={`${t("ops.inspectCase")} #${node.quality.caseId}`}
											>
												<ArrowUpRight className="size-3.5" />
											</Link>
										)}
									</div>
								</TableCell>
								<TableCell>
									<ExitIPCell node={node} />
								</TableCell>
								<TableCell>
									<div className="space-y-1">
										<LatencyCell node={node} />
										<ProbeTime node={node} />
									</div>
								</TableCell>
								<TableCell>
									<RotatableCell node={node} />
								</TableCell>
								<TableActionCell>
									<DropdownMenu>
										<DropdownMenuTrigger asChild>
											<Button type="button" variant="ghost" size="icon" className="size-8">
												<MoreHorizontal aria-hidden="true" />
												<span className="sr-only">{t("common.actions")}</span>
											</Button>
										</DropdownMenuTrigger>
										<DropdownMenuContent align="end">
											<DropdownMenuItem onClick={() => openEdit(node)}>
												<Pencil />
												{t("common.edit")}
											</DropdownMenuItem>
											<DropdownMenuItem disabled={togglingExit} onClick={() => toggleExit({ id: node.id, enabled: !node.enabled })}>
												{node.enabled ? <PowerOff /> : <Power />}{t(node.enabled ? "common.disable" : "common.enable")}
											</DropdownMenuItem>
											<DropdownMenuSeparator />
											{clearanceMode !== "manual" && !node.accountBoundProxy ? (
												<DropdownMenuItem
													disabled={refreshingClearance}
													onClick={() => refreshNodeClearance(node.id)}
												>
													<RefreshCw />
													{t("settings.egress.refreshClearance")}
												</DropdownMenuItem>
											) : null}
											<DropdownMenuItem
												disabled={probingNode || !node.proxyConfigured}
												onClick={() => probeNode(node.id)}
											>
												<RefreshCw />
												{t("settings.egress.test")}
											</DropdownMenuItem>
											<DropdownMenuItem
												disabled={!node.rotationConfigured}
												onClick={() => rotateExit(node.id)}
											>
												<RefreshCw />
												{t("settings.egress.rotateExitIP")}
											</DropdownMenuItem>
											{node.quality?.state === "banned" ? (
												<DropdownMenuItem
													disabled={unbanningNode}
													onClick={() => unbanExit(node.id)}
												>
													<LockOpen />
													{t("quality.egress.unban")}
												</DropdownMenuItem>
											) : null}
											<DropdownMenuItem
												className="text-destructive focus:text-destructive"
												onClick={() => setDeletingNode(node)}
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
				)}
			</Table>
		),
		[
			query.isPending,
			nodes,
			hasActiveFilters,
			t,
			selected,
			allPageSelected,
			selectedOnPage.length,
			togglePage,
			sort,
			changeSort,
			toggleNode,
			openEdit,
			now,
			onManageSources,
			clearanceMode,
			refreshingClearance,
			refreshNodeClearance,
			probingNode,
			probeNode,
			rotateExit,
			unbanningNode,
			unbanExit,
			toggleExit,
			togglingExit,
		],
	);

	return (
		<div className="nres-resources">
			<nav className="nres-subnav" aria-label={t("networkResources.resourcesNavigation")}>
				<button type="button" aria-current={!sourcesOpen ? "page" : undefined} onClick={onShowNodes}>{t("ops.nodeTab")}<span>{summaryQuery.data?.total ?? "—"}</span></button>
				<button type="button" aria-current={sourcesOpen ? "page" : undefined} onClick={onManageSources}>{t("ops.netSources")}</button>
			</nav>
			{sourcesOpen && <SubscriptionsPanel showHeader={false} />}
			<div hidden={sourcesOpen}>
				<DataTableShell
					className="nres-shell"
					toolbar={
						<div className="nres-toolbar">
							<div className="nres-toolbar-search">
								<Search aria-hidden="true" />
								<Input value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); setSelected(new Map()); }} placeholder={t("networkResources.searchNodes")} aria-label={t("networkResources.searchNodes")} />
							</div>
							<DataTableFilters filters={[
								{ id: "condition", label: t("ops.nodeState"), value: conditionFilter === "all" ? "" : conditionFilter,
									onChange: (value) => { setConditionFilter(value || "all"); setPage(1); setSelected(new Map()); },
									options: ["restricted", "held", "banned", "unhealthy", "cooling", "unconfigured"].map((value) => ({ value, label: t(value === "restricted" ? "networkResources.restricted" : `ops.condition.${value}`) })) },
								{ id: "enabled", label: t("settings.egress.enabled"), value: enabledFilter,
									onChange: (value) => { setEnabledFilter(value); setPage(1); setSelected(new Map()); },
									options: [{ value: "enabled", label: t("common.enable") }, { value: "disabled", label: t("common.disable") }] },
								{ id: "probe", label: t("settings.egress.probe"), value: probeFilter,
									onChange: (value) => { setProbeFilter(value); setPage(1); setSelected(new Map()); },
									options: [{ value: "healthy", label: t("settings.egress.healthy") }, { value: "unhealthy", label: t("settings.egress.unhealthy") }, { value: "unknown", label: t("settings.egress.notTested") }] },
							]} />
							<div className="nres-toolbar-actions">
								<Button type="button" variant="ghost" size="icon" disabled={query.isFetching} onClick={() => void query.refetch()} aria-label={t("common.refresh")}><RefreshCw className={cn(query.isFetching && "animate-spin")} /></Button>
								<Button type="button" size="sm" variant="outline" disabled={testAll.isPending || (selected.size > 0 && selectedNodes.every((node) => !node.proxyConfigured))} onClick={() => testAll.mutate(selected.size ? selectedNodes.filter((node) => node.proxyConfigured).map((node) => node.id) : undefined)}>
									{testAll.isPending ? <Spinner /> : <Network />}{t(selected.size ? "networkResources.probeSelected" : "settings.egress.testAll", { count: selectedNodes.filter((node) => node.proxyConfigured).length })}
								</Button>
								<ProbeSettingsButton />
								<Button type="button" size="sm" variant="outline" onClick={() => { importSession.current = {}; setImportForm(emptyImport); setImportOpen(true); }}><Upload />{t("networkResources.import")}</Button>
								<Button type="button" size="sm" onClick={openCreate}><Plus />{t("settings.egress.add")}</Button>
								<Button type="button" size="sm" variant="outline" disabled={cleanupUnhealthy.isPending} onClick={openCleanup}>
									{cleanupUnhealthy.isPending ? <Spinner /> : <Trash2 className="text-destructive" />}{t("settings.egress.cleanupUnavailable")}
								</Button>
							</div>
						</div>
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
					<div className="nres-chips" aria-label={t("ops.nodeState")}>
						{(["all", "attention", "ready", "unknown", "disabled"] as const).map((condition) => (
							<button key={condition} type="button" aria-pressed={conditionFilter === condition} onClick={() => { setConditionFilter(condition); setPage(1); setSelected(new Map()); }}>
								{t(`networkResources.filter.${condition}`)}<span>{summaryQuery.data ? condition === "all" ? summary.total : summary[condition] : "—"}</span>
							</button>
						))}
						{!["all", "attention", "ready", "unknown", "disabled"].includes(conditionFilter) && <button type="button" aria-pressed="true" onClick={() => { setConditionFilter("all"); setPage(1); }}>{t(conditionFilter === "restricted" ? "networkResources.restricted" : `ops.condition.${conditionFilter}`)}<span>×</span></button>}
					</div>
					{query.isError ? (
						<ErrorState message={query.error.message} onRetry={() => void query.refetch()} />
					) : null}
					{selected.size > 0 ? (
						<div className="nres-selection">
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
									count: selectedNodes.filter((node) => node.rotationConfigured).length,
								})}
							</Button>
							<Button
								type="button"
								size="sm"
								variant="secondary"
								disabled={batchPending || selectedNodes.every((node) => node.enabled)}
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
								disabled={batchPending || selectedNodes.every((node) => !node.enabled)}
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
						</div>
					) : null}
					<div className="nres-inventory">{query.data || !query.isError ? inventory : null}</div>
				</DataTableShell>
			</div>

			<Dialog open={Boolean(inspectedNode)} onOpenChange={(open) => { if (!open) setInspecting(null); }}>
				<DialogContent className="nres-detail-dialog">
					{inspectedNode && <>
						<DialogHeader>
							<DialogTitle>{inspectedNode.name}</DialogTitle>
							<DialogDescription>{t("networkResources.detailsHelp")}</DialogDescription>
						</DialogHeader>
						<div className="nres-detail-body">
							<div className="nres-detail-status"><NodeStateLabel node={inspectedNode} now={now} /><span>{t(inspectedNode.enabled ? "networkResources.enabledForRouting" : "networkResources.disabledForRouting")}</span>{inspectedNode.quality?.caseId && <Link to={`/guard?case=${inspectedNode.quality.caseId}#tribunal`}>{t("ops.inspectCase")} #{inspectedNode.quality.caseId}<ArrowUpRight /></Link>}</div>
							<dl className="nres-detail-facts">
								<div><dt>{t("settings.egress.proxyURL")}</dt><dd><code>{inspectedNode.proxyDisplay || t("networkResources.addressNotConfigured")}</code></dd></div>
								<div><dt>{t("ops.netSources")}</dt><dd>{inspectedNode.sourceId ? <button type="button" onClick={() => { setInspecting(null); onManageSources(); }}>{inspectedNode.sourceName || t("ops.netSources")}</button> : t("networkResources.manualSource")}</dd></div>
								<div><dt>{t("networkResources.poolMembership")}</dt><dd className="nres-detail-memberships">{inspectedNode.pools?.length ? inspectedNode.pools.map((pool) => <Link key={pool.id} to={`/proxies?pool=${pool.id}#pools`}>{pool.name}<ArrowUpRight /></Link>) : t("networkResources.noPool")}</dd></div>
							</dl>
							<div className="nres-detail-section-heading"><h3>{t("networkResources.connectivity")}</h3><span>{inspectedNode.probeProvider === "ipinfo" ? "IPinfo" : inspectedNode.probeProvider === "cloudflare" ? "Cloudflare" : ""}</span></div>
							{inspectedNode.rotatingEndpoint && <p className="nres-detail-note">{t("networkResources.dynamicObservation")}</p>}
							<div className="nres-probe-families">
								<ProbeFamilyDetails family="IPv4" probe={inspectedNode.ipv4Probe} />
								<ProbeFamilyDetails family="IPv6" probe={inspectedNode.ipv6Probe} />
							</div>
							<div className="nres-detail-section-heading"><h3>{t("ops.recovery")}</h3><RotatableCell node={inspectedNode} /></div>
							<dl className="nres-detail-facts">
								{inspectedNode.cooldownUntil && Date.parse(inspectedNode.cooldownUntil) > now && <div><dt>{t("networkResources.cooldownUntil")}</dt><dd><time dateTime={inspectedNode.cooldownUntil}>{new Date(inspectedNode.cooldownUntil).toLocaleString()}</time></dd></div>}
								{inspectedNode.rotationConfigured && <><div><dt>{t("networkResources.lastRotation")}</dt><dd>{inspectedNode.lastRotatedAt ? <time dateTime={inspectedNode.lastRotatedAt}>{new Date(inspectedNode.lastRotatedAt).toLocaleString()}</time> : t("settings.egress.never")}</dd></div><div><dt>{t("networkResources.rotationAttempts")}</dt><dd>{inspectedNode.rotationAttempts}</dd></div></>}
								<div><dt>{t("quality.egressView.history")}</dt><dd><DegradeBadge nodeID={Number(inspectedNode.id)} fallback={inspectedNode.degradeCount} totals={qualityDegrades} onOpenLedger={(id) => { setInspecting(null); setLedgerNodeID(id); }} /></dd></div>
							</dl>
							{inspectedNode.lastError && <p className="nres-detail-error">{inspectedNode.lastError}</p>}
							{inspectedNode.lastRotationError && <p className="nres-detail-error">{inspectedNode.lastRotationError}</p>}
							<div className="nres-detail-actions">
								<Button type="button" size="sm" variant="outline" disabled={probingNode || !inspectedNode.proxyConfigured} onClick={() => probeNode(inspectedNode.id)}>{probingNode ? <Spinner /> : <Network />}{t("settings.egress.test")}</Button>
								{inspectedNode.rotationConfigured && <Button type="button" size="sm" variant="outline" disabled={rotateNode.isPending} onClick={() => rotateExit(inspectedNode.id)}><RefreshCw />{t("settings.egress.rotateExitIP")}</Button>}
								{inspectedNode.quality?.state === "banned" && <Button type="button" size="sm" variant="outline" disabled={unbanningNode} onClick={() => unbanExit(inspectedNode.id)}><LockOpen />{t("quality.egress.unban")}</Button>}
							</div>
						</div>
						<DialogFooter><Button type="button" variant="outline" size="sm" onClick={() => setInspecting(null)}>{t("common.close")}</Button><Button type="button" size="sm" onClick={() => { setInspecting(null); openEdit(inspectedNode); }}><Pencil />{t("common.edit")}</Button></DialogFooter>
					</>}
				</DialogContent>
			</Dialog>

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
						<DialogDescription>{t("quality.egressView.ledgerHelp")}</DialogDescription>
					</DialogHeader>
					{(() => {
						if (qualityNodesQuery.isError)
							return (
								<ErrorState
									message={qualityNodesQuery.error.message}
									onRetry={() => void qualityNodesQuery.refetch()}
								/>
							);
						if (qualityNodesQuery.isPending)
							return (
								<div className="flex justify-center p-6">
									<Spinner />
								</div>
							);
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

			<Dialog open={batchRotationOpen} onOpenChange={setBatchRotationOpen}>
				<DialogContent layout="compact" aria-describedby={undefined}>
					<DialogHeader>
						<DialogTitle>
							{t("settings.egress.batchRotation")} · {selected.size}
						</DialogTitle>
					</DialogHeader>
					<OperationsField
						controlId="egress-batch-rotation"
						label={t("settings.egress.webhook")}
						description={`${t("settings.egress.batchRotationDescription", { count: selected.size })} ${t("settings.egress.batchRotationHelp")}`}
					>
						<Input
							id="egress-batch-rotation"
							autoFocus
							value={batchRotationTemplate}
							placeholder="http://203.0.113.10:9000/rotate/{port}?token=xxx"
							onChange={(event) => setBatchRotationTemplate(event.target.value)}
						/>
					</OperationsField>
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

			<AlertDialog
				open={Boolean(deletingNode)}
				onOpenChange={(open) => {
					if (!open && !remove.isPending) setDeletingNode(null);
				}}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							{t("ops.netDeleteNode", { name: deletingNode?.name })}
						</AlertDialogTitle>
						<AlertDialogDescription>
							{t("settings.egress.batchDeleteDescription", { count: 1 })}
							{deletingNode?.sourceId && (
								<span className="mt-2 block">
									{t("settings.egress.batchDeleteSourceHint", { count: 1 })}
								</span>
							)}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={remove.isPending}>{t("common.cancel")}</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-destructive-foreground"
							disabled={remove.isPending}
							onClick={(event) => {
								event.preventDefault();
								if (deletingNode) remove.mutate(deletingNode.id);
							}}
						>
							{remove.isPending && <Spinner />}
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
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
						<AlertDialogTitle>{t("settings.egress.cleanupUnavailableTitle")}</AlertDialogTitle>
						<AlertDialogDescription className="space-y-2">
							<span className="block">{t("settings.egress.cleanupUnavailableDescription")}</span>
							<span className="block">{t("settings.egress.cleanupUnavailableImpact")}</span>
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
					{cleanupPreview.data && cleanupPreview.data.subscriptionManaged > 0 ? (
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
				open={editorOpen}
				onOpenChange={(open) => {
					if (!open) closeEditor();
				}}
			>
				<DialogContent layout="editor" aria-describedby={undefined}>
					<DialogHeader>
						<DialogTitle>
							<Network aria-hidden="true" />
							{editing ? editing.name : t("settings.egress.addTitle")}
						</DialogTitle>
					</DialogHeader>
					<form
						className="net-editor-form"
						onSubmit={(event) => {
							event.preventDefault();
							event.stopPropagation();
							const session = editorSession.current;
							if (session && !save.isPending)
								save.mutate({ editing, form, revealedProxyURL, revealedRotationURL, session });
						}}
					>
						<fieldset className="net-editor-body nres-editor-fields" disabled={save.isPending}>
							{editing && (
								<div className="net-node-state">
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
									<span className="font-mono text-muted-foreground">{editing.exitIp || "—"}</span>
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
							<div>
								<div className="net-identity">
									<OperationsField label={t("settings.egress.name")} controlId="egress-name">
										<Input
											id="egress-name"
											placeholder={t("ops.netNameExample")}
											maxLength={160}
											value={form.name}
											onChange={(event) => setForm({ ...form, name: event.target.value })}
										/>
									</OperationsField>
									<OperationsField label={t("settings.egress.enabled")} controlId="egress-enabled">
										<Switch
											id="egress-enabled"
											checked={form.enabled}
											onCheckedChange={(enabled) => setForm({ ...form, enabled })}
										/>
									</OperationsField>
								</div>
								<OperationsField
									label={t("settings.egress.proxyURL")}
									controlId="egress-proxy"
									description={t("settings.egress.proxyProtocols")}
								>
									<Input
										className="net-address-input"
										id="egress-proxy"
										autoComplete="off"
										placeholder="socks5h://user:pass@host:port"
										value={form.proxyURL}
										onChange={(event) => {
											if (editorSession.current) editorSession.current.proxyEdited = true;
											const proxyURL = event.target.value,
												hasProxy = Boolean(editing?.proxyConfigured) || Boolean(proxyURL.trim());
											setForm({
												...form,
												proxyURL,
												proxyPool: hasProxy ? form.proxyPool : false,
												rotationURL: hasProxy ? form.rotationURL : "",
												rotationOpen: hasProxy ? form.rotationOpen : false,
												rotationEnabled: hasProxy ? form.rotationEnabled : false,
											});
										}}
									/>
								</OperationsField>

								<OperationsField
									label={t("ops.nodeBehavior")}
									controlId="egress-kind"
									description={t(
										!editing?.proxyConfigured && !form.proxyURL?.trim()
											? "ops.netAddressFirst"
											: form.proxyPool
												? "settings.egress.proxyPoolNoRotation"
												: form.rotationOpen
													? "settings.egress.rotationHelp"
													: "ops.fixedExitHelp",
									)}
								>
									<NetworkSelect
										id="egress-kind"
										label={t("ops.nodeBehavior")}
										value={form.proxyPool ? "dynamic" : form.rotationOpen ? "rotating" : "fixed"}
										disabled={!editing?.proxyConfigured && !form.proxyURL?.trim()}
										options={[
											{ value: "fixed", label: t("ops.netFixed"), icon: MapPin },
											{ value: "dynamic", label: t("ops.netDynamic"), icon: Shuffle },
											{ value: "rotating", label: t("ops.netRotating"), icon: RefreshCw },
										]}
										onChange={(value) =>
											setForm({
												...form,
												proxyPool: value === "dynamic",
												rotationOpen: value === "rotating",
											})
										}
									/>
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
													editing?.rotationConfigured && !form.rotationURL?.trim()
														? t("settings.egress.keepConfigured")
														: "https://host/rotate"
												}
												value={form.rotationURL}
												onChange={(event) => {
													if (editorSession.current) editorSession.current.rotationEdited = true;
													// 与代理地址输入同语义:原样保留键入内容,保存时统一 trim。
													setForm({
														...form,
														rotationURL: event.target.value,
														rotationEnabled:
															Boolean(event.target.value.trim()) ||
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
												onCheckedChange={(rotationEnabled) => setForm({ ...form, rotationEnabled })}
											/>
										</OperationsField>
									</>
								)}
							</div>
						</fieldset>
						<DialogFooter>
							<Button type="button" variant="outline" size="sm" onClick={closeEditor}>
								{t(save.isPending ? "common.close" : "common.cancel")}
							</Button>
							<Button type="submit" size="sm" disabled={!form.name.trim() || save.isPending}>
								{save.isPending && <Spinner />}
								{t("common.save")}
							</Button>
						</DialogFooter>
					</form>
				</DialogContent>
			</Dialog>

			<Dialog open={importOpen} onOpenChange={(open) => { if (!open) importSession.current = null; setImportOpen(open); }}>
				<DialogContent layout="editor" aria-describedby={undefined}>
					<DialogHeader className="pr-8">
						<DialogTitle>{t("settings.egress.importText")}</DialogTitle>
					</DialogHeader>
					<form
						className="net-editor-form"
						onSubmit={(event) => {
							event.preventDefault();
							event.stopPropagation();
							const session = importSession.current;
							if (session && !importText.isPending) importText.mutate({ input: importForm, session });
						}}
					>
						<fieldset className="net-editor-body nres-editor-fields" disabled={importText.isPending}>
							<Field label={t("settings.egress.name")} controlId="egress-import-name">
								<Input
									id="egress-import-name"
									maxLength={160}
									value={importForm.name}
									onChange={(event) => setImportForm({ ...importForm, name: event.target.value })}
								/>
							</Field>
							<Field
								label={t("settings.egress.proxyList")}
								controlId="egress-import-list"
								description={t("settings.egress.importDialogDescription")}
							>
								<Textarea
									className="min-h-72 font-mono text-xs"
									id="egress-import-list"
									value={importForm.content}
									onChange={(event) =>
										setImportForm({ ...importForm, content: event.target.value })
									}
								/>
							</Field>
						</fieldset>
						<DialogFooter>
							<Button
								type="button"
								size="sm"
								variant="secondary"
								onClick={() => { importSession.current = null; setImportOpen(false); }}
							>
								{t("common.cancel")}
							</Button>
							<Button
								type="submit"
								size="sm"
								disabled={
									!importForm.name.trim() || !importForm.content.trim() || importText.isPending
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
});

function CleanupPreviewValue({ label, value }: { label: string; value: number }) {
	return (
		<div className="space-y-1">
			<div className="text-base font-medium tabular-nums text-foreground">{value}</div>
			<div className="text-muted-foreground">{label}</div>
		</div>
	);
}

function ProbeFamilyDetails({ family, probe }: { family: string; probe: EgressNodeDTO["ipv4Probe"] }) {
	const { t, i18n } = useTranslation();
	return <section className="nres-probe-family">
		<header><h4>{family}</h4><StatusPill tone={probe.status === "healthy" ? "good" : probe.status === "unhealthy" ? "bad" : "neutral"}>{t(probe.status === "healthy" ? "settings.egress.healthy" : probe.status === "unhealthy" ? "settings.egress.unhealthy" : "settings.egress.notTested")}</StatusPill></header>
		<code>{probe.exitIp || "—"}</code>
		<dl><div><dt>{t("networkResources.probeLatency")}</dt><dd>{probe.status === "healthy" && probe.testedAt ? `${probe.latencyMs} ms` : "—"}</dd></div><div><dt>{t("networkResources.measuredAt")}</dt><dd>{probe.testedAt ? <time dateTime={probe.testedAt}>{new Date(probe.testedAt).toLocaleString(i18n.language)}</time> : t("settings.egress.notTested")}</dd></div></dl>
		{probe.error && <p className="nres-detail-error">{probe.error}</p>}
	</section>;
}

function NodeStateLabel({ node, now }: { node: EgressNodeDTO; now: number }) {
	const { t } = useTranslation();
	const condition = nodeCondition(node, now);
	// 动态隧道计入可用候选(与 networkSummary 口径一致)，与 ready 同用绿色。
	const tone = ["banned", "unhealthy"].includes(condition)
		? "bad"
		: ["held", "cooling", "unconfigured"].includes(condition)
			? "warn"
			: condition === "ready" || condition === "dynamic"
				? "good"
				: "neutral";
	return <StatusPill tone={tone}>{t(`ops.condition.${condition}`)}</StatusPill>;
}
function RotatableCell({ node }: { node: EgressNodeDTO }) {
	const { t } = useTranslation();
	return (
		<span className="block truncate text-xs text-muted-foreground">
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
function ProbeTime({ node }: { node: EgressNodeDTO }) {
	if (!node.lastProbedAt) return null;
	return (
		<NetworkTooltip content={new Date(node.lastProbedAt).toLocaleString()} tapToOpen>
			<time
				dateTime={node.lastProbedAt}
				tabIndex={0}
				className="block w-fit text-[10px] leading-3 tabular-nums text-muted-foreground/75"
			>
				{new Date(node.lastProbedAt).toLocaleString([], {
					month: "2-digit",
					day: "2-digit",
					hour: "2-digit",
					minute: "2-digit",
					hour12: false,
				})}
			</time>
		</NetworkTooltip>
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
	if (probe?.testedAt) {
		return <span className="text-xs tabular-nums text-muted-foreground">{probe.latencyMs} ms</span>;
	}
	const probed = [node.ipv4Probe, node.ipv6Probe].some(
		(probe) => probe && probe.status !== "unknown",
	);
	return (
		<span className="text-[10px] text-muted-foreground/70">
			{probed ? t("proxies.nodes.latencyUnhealthy") : t("proxies.nodes.latencyUntested")}
		</span>
	);
}
/** Addresses are selectable, fully visible probe facts. Only failures need help. */
function ExitIPCell({ node }: { node: EgressNodeDTO }) {
	const { t } = useTranslation();
	const addresses = [
		{ family: "IPv4", probe: node.ipv4Probe },
		{ family: "IPv6", probe: node.ipv6Probe },
	];
	return (
		<span className="nres-addresses">
			{addresses.map(({ family, probe }) => (
				<span key={family} className="nres-address-line">
					<span className="nres-address-family">{family}</span>
					<span
						className={cn("nres-address-value", probe.status === "unhealthy" && "text-destructive")}
					>
						{probe.exitIp ||
							t(
								probe.status === "unknown"
									? "settings.egress.notTested"
									: probe.status === "healthy"
									? "settings.egress.healthy"
									: "settings.egress.unhealthy",
							)}
					</span>
					{probe.error && <NetworkError message={`${family} · ${probe.error}`} />}
				</span>
			))}
		</span>
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
		<OperationsField label={label} controlId={controlId} description={description ?? help}>
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
		return <span className="text-xs tabular-nums text-muted-foreground">0</span>;
	}
	return (
		<button
			type="button"
			onClick={() => onOpenLedger(nodeID)}
			aria-label={`${t("quality.egressView.history")} · ${total}`}
			className="shrink-0 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
		>
			<Badge
				variant="outline"
				className="border-transparent text-[11px] text-muted-foreground hover:border-border hover:text-primary"
			>
				{t("ops.historyCount", { count: total })}
				<ArrowUpRight className="size-3" aria-hidden="true" />
			</Badge>
		</button>
	);
}
