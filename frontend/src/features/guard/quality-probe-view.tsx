import { useQuery } from "@tanstack/react-query";
import {
	Activity,
	AlertCircle,
	AlertTriangle,
	ArrowRight,
	CheckCircle2,
	Clock,
	ExternalLink,
	Eye,
	Gavel,
	LayoutGrid,
	RefreshCw,
	Scale,
	Search,
	ShieldCheck,
	Table as TableIcon,
	Timer,
	User,
	Users,
	X,
} from "lucide-react";
import { memo, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import {
	Dialog,
	DialogHeader,
	DialogTitle,
} from "@/shared/ui/dialog";
import { OperationsDialogContent } from "@/shared/ui/operations";
import { Input } from "@/shared/ui/input";
import { Spinner } from "@/shared/ui/spinner";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/shared/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { useAccountDirectory } from "@/entities/account/account-queries";
import { listAllEgressNodes } from "@/entities/egress/egress-api";
import { Pagination } from "@/shared/components/pagination";
import { cn } from "@/shared/lib/cn";

import {
	fetchQualityNodes,
	fetchQualityProbes,
	fetchQualitySettings,
	type QualityProbeTask,
} from "@/entities/guard/quality-api";
import { useNow } from "@/shared/lib/use-now";
import {
	QualityAccountReference,
	QualityExitReference,
} from "./quality-identity";
import { LoadFailed } from "./quality-tribunal-view";
import {
	buildQualityExitIPIndex,
	getProbeFinding,
	parseGoDurationMs,
	probeDurationMs,
	probeSummary,
	type QualityAccountIdentity,
	type QualityExitIPIndex,
	type QualityNodeIdentity,
} from "./quality-view";

const PROBE_PAGE_SIZE = 12;

export function QualityProbeView() {
	const { t, i18n } = useTranslation();
	const [page, setPage] = useState(1);
	const [viewMode, setViewMode] = useState<"cards" | "table">("cards");
	const [search, setSearch] = useState("");
	const [filterDirection, setFilterDirection] = useState<string>("all");
	const [filterResult, setFilterResult] = useState<string>("all");

	// Active inspection modal
	const [inspectTask, setInspectTask] = useState<QualityProbeTask | null>(null);

	const probesQuery = useQuery({
		queryKey: ["quality", "probes"],
		queryFn: ({ signal }) => fetchQualityProbes(signal),
		refetchInterval: 15_000,
	});

	const settingsQuery = useQuery({
		queryKey: ["quality", "settings"],
		queryFn: ({ signal }) => fetchQualitySettings(signal),
		staleTime: 60_000,
	});

	const accountsQuery = useAccountDirectory((probesQuery.data ?? []).flatMap((probe) => [probe.defendant, probe.juror, probe.control_account_id ?? 0]));

	const egressNamesQuery = useQuery({
		queryKey: ["quality", "egress-names"],
		queryFn: () => listAllEgressNodes(),
		staleTime: 60_000,
		refetchInterval: 5 * 60_000,
	});

	const nodesQuery = useQuery({
		queryKey: ["quality", "nodes"],
		queryFn: ({ signal }) => fetchQualityNodes(signal),
		staleTime: 30_000,
		refetchInterval: 30_000,
	});

	const accountIdentity = useMemo(() => {
		const map = new Map<number, QualityAccountIdentity>();
		for (const account of accountsQuery.data?.items ?? []) {
			map.set(Number(account.id), { name: account.name, email: account.email });
		}
		return map;
	}, [accountsQuery.data]);

	const nodeIdentity = useMemo(() => {
		const map = new Map<number, QualityNodeIdentity>();
		for (const node of egressNamesQuery.data?.items ?? []) {
			if (node.name) {
				map.set(Number(node.id), { name: node.name });
			}
		}
		return map;
	}, [egressNamesQuery.data]);

	const ipByNode = useMemo(
		() => buildQualityExitIPIndex(nodesQuery.data ?? []),
		[nodesQuery.data],
	);

	const windowMs =
		parseGoDurationMs(settingsQuery.data?.evidence_window) ?? 30 * 60 * 1000;
	const windowMinutes = Math.round(windowMs / 60_000);
	const now = useNow(15_000);
	const probes = useMemo(() => probesQuery.data ?? [], [probesQuery.data]);
	const summary = probeSummary(probes, now, windowMs);

	// Filtering
	const filteredProbes = useMemo(() => {
		let list = probes;

		if (filterDirection !== "all") {
			list = list.filter((p) => p.direction === filterDirection);
		}

		if (filterResult !== "all") {
			if (filterResult === "running") {
				list = list.filter((p) => p.state === "running" || p.state === "pending");
			} else {
				list = list.filter((p) => p.result === filterResult);
			}
		}

		if (search.trim()) {
			const q = search.trim().toLowerCase();
			list = list.filter((p) => {
				const idMatch = String(p.id).includes(q) || `#${p.id}`.includes(q);
				const caseMatch = String(p.case_id).includes(q) || `#${p.case_id}`.includes(q);
				const detailMatch = p.detail?.toLowerCase().includes(q);
				const failMatch = p.failure_kind?.toLowerCase().includes(q);

				// Account match
				const defAcc = accountIdentity.get(p.defendant);
				const jurAcc = p.juror ? accountIdentity.get(p.juror) : null;
				const accMatch = Boolean(
					(defAcc?.name && defAcc.name.toLowerCase().includes(q)) ||
					(defAcc?.email && defAcc.email.toLowerCase().includes(q)) ||
					(jurAcc?.name && jurAcc.name.toLowerCase().includes(q)) ||
					(jurAcc?.email && jurAcc.email.toLowerCase().includes(q)),
				);

				// Node match
				const targetNode = nodeIdentity.get(p.node_id);
				const baseNode = p.baseline_node_id ? nodeIdentity.get(p.baseline_node_id) : null;
				const nodeMatch = Boolean(
					(targetNode?.name && targetNode.name.toLowerCase().includes(q)) ||
					(baseNode?.name && baseNode.name.toLowerCase().includes(q)),
				);

				return idMatch || caseMatch || detailMatch || failMatch || accMatch || nodeMatch;
			});
		}

		return list;
	}, [probes, filterDirection, filterResult, search, accountIdentity, nodeIdentity]);

	const totalPages = Math.max(1, Math.ceil(filteredProbes.length / PROBE_PAGE_SIZE));
	const currentPage = Math.min(page, totalPages);
	const pageItems = filteredProbes.slice(
		(currentPage - 1) * PROBE_PAGE_SIZE,
		currentPage * PROBE_PAGE_SIZE,
	);

	const timeFormatter = useMemo(
		() =>
			new Intl.DateTimeFormat(i18n.language, {
				hour: "2-digit",
				minute: "2-digit",
				second: "2-digit",
			}),
		[i18n.language],
	);

	return (
		<div className="flex flex-col gap-5">
			{/* Telemetry Velocity Deck (4 Top Cards) */}
			<div className="grid grid-cols-2 gap-3 md:grid-cols-4">
				{/* 1. In-Flight Probes */}
				<div className="relative overflow-hidden rounded-xl border border-border/80 bg-card/60 p-4 shadow-xs">
					<div className="flex items-center justify-between">
						<span className="text-xs font-semibold text-muted-foreground">
							{t("guardProbes.telemetryInFlight")}
						</span>
						{summary.inFlight > 0 ? (
							<span className="flex items-center gap-1 rounded-full bg-blue-500/10 px-2 py-0.5 text-[10px] font-bold text-blue-500 animate-pulse">
								<Activity className="size-3 animate-spin" />
								{summary.running} RUN
							</span>
						) : (
							<span className="flex items-center gap-1 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
								<CheckCircle2 className="size-3 text-emerald-500" />
								IDLE
							</span>
						)}
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span className="text-2xl font-black tracking-tight tabular-nums text-foreground">
							{summary.inFlight}
						</span>
						<span className="text-xs text-muted-foreground">
							({t("guardProbes.telemetryPending", { count: summary.pending })})
						</span>
					</div>
					<p className="mt-1 truncate text-[11px] text-muted-foreground/80">
						{summary.inFlight > 0
							? `${summary.running} ${t("guardProbes.stateRunning")}, ${summary.pending} ${t("guardProbes.statePending")}`
							: t("guardProbes.emptyDesc")}
					</p>
				</div>

				{/* 2. Confirmed Degraded */}
				<div
					className={cn(
						"relative overflow-hidden rounded-xl border p-4 shadow-xs transition-all",
						summary.degraded > 0
							? "border-rose-500/30 bg-rose-500/5 dark:bg-rose-950/10"
							: "border-border/80 bg-card/60",
					)}
				>
					<div className="flex items-center justify-between">
						<span className="text-xs font-semibold text-muted-foreground">
							{t("guardProbes.telemetryDegraded")}
						</span>
						<AlertTriangle
							className={cn("size-4", summary.degraded > 0 ? "text-rose-500 animate-pulse" : "text-muted-foreground/40")}
						/>
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span
							className={cn(
								"text-2xl font-black tracking-tight tabular-nums",
								summary.degraded > 0 ? "text-rose-600 dark:text-rose-400" : "text-foreground",
							)}
						>
							{summary.degraded}
						</span>
					</div>
					<p className="mt-1 truncate text-[11px] text-muted-foreground/80">
						{t("guardProbes.telemetryDegradedHelp")}
					</p>
				</div>

				{/* 3. Clean Passes */}
				<div className="relative overflow-hidden rounded-xl border border-border/80 bg-card/60 p-4 shadow-xs">
					<div className="flex items-center justify-between">
						<span className="text-xs font-semibold text-muted-foreground">
							{t("guardProbes.telemetryClean")}
						</span>
						<CheckCircle2 className="size-4 text-emerald-500" />
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span className="text-2xl font-black tracking-tight tabular-nums text-emerald-600 dark:text-emerald-400">
							{summary.clean}
						</span>
					</div>
					<p className="mt-1 truncate text-[11px] text-muted-foreground/80">
						{t("guardProbes.telemetryCleanHelp")}
					</p>
				</div>

				{/* 4. Transport Errors */}
				<div className="relative overflow-hidden rounded-xl border border-border/80 bg-card/60 p-4 shadow-xs">
					<div className="flex items-center justify-between">
						<span className="text-xs font-semibold text-muted-foreground">
							{t("guardProbes.telemetryErrors")}
						</span>
						<Timer className="size-4 text-amber-500" />
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span className="text-2xl font-black tracking-tight tabular-nums text-foreground">
							{summary.error}
						</span>
						{summary.cancelled > 0 && (
							<span className="text-xs text-muted-foreground">
								(+{summary.cancelled} {t("guardProbes.stateCancelled")})
							</span>
						)}
					</div>
					<p className="mt-1 truncate text-[11px] text-muted-foreground/80">
						{t("guardProbes.telemetryWindow", { window: windowMinutes })}
					</p>
				</div>
			</div>

			{/* Filter & Command Capsule */}
			<div className="flex flex-col gap-3 rounded-xl border border-border/80 bg-card/60 p-3.5 shadow-xs">
				<div className="flex flex-col gap-3 xl:flex-row xl:items-center xl:justify-between">
					{/* Direction & Result Filter Pills */}
					<div className="flex flex-wrap items-center gap-1.5 overflow-x-auto whitespace-nowrap scrollbar-none">
						{/* Direction */}
						{[
							{ id: "all", label: t("guardProbes.directionAll"), icon: null },
							{ id: "account_differential", label: t("guardProbes.directionBadgeAccount"), icon: Users },
							{ id: "exit_jury", label: t("guardProbes.directionBadgeJury"), icon: Gavel },
						].map((item) => (
							<button
								type="button"
								key={item.id}
								onClick={() => {
									setFilterDirection(item.id);
									setPage(1);
								}}
								className={cn(
									"flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-all cursor-pointer",
									filterDirection === item.id
										? "bg-primary text-primary-foreground shadow-xs"
										: "bg-muted/50 border border-border/60 text-muted-foreground hover:border-border hover:text-foreground",
								)}
							>
								<span>{item.label}</span>
							</button>
						))}

						<div className="h-4 w-px bg-border/80 mx-1 shrink-0" />

						{/* Results */}
						{[
							{ id: "all", label: t("guardProbes.resultAll"), dot: "" },
							{ id: "clean", label: t("guardProbes.resultClean"), dot: "bg-emerald-500" },
							{ id: "degraded", label: t("guardProbes.resultDegraded"), dot: "bg-rose-500" },
							{ id: "error", label: t("guardProbes.resultError"), dot: "bg-amber-500" },
							{ id: "running", label: t("guardProbes.stateRunning"), dot: "bg-blue-500" },
						].map((item) => (
							<button
								type="button"
								key={item.id}
								onClick={() => {
									setFilterResult(item.id);
									setPage(1);
								}}
								className={cn(
									"flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-all cursor-pointer",
									filterResult === item.id
										? "bg-foreground text-background shadow-xs font-semibold"
										: "bg-muted/50 border border-border/60 text-muted-foreground hover:border-border hover:text-foreground",
								)}
							>
								{item.dot && <span className={cn("size-1.5 rounded-full", item.dot)} />}
								<span>{item.label}</span>
							</button>
						))}
					</div>

					{/* Search & View Controls */}
					<div className="flex items-center gap-2 shrink-0">
						{/* Search Input */}
						<div className="relative min-w-[240px] sm:min-w-[280px]">
							<Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
							<Input
								placeholder={t("guardProbes.searchPlaceholder")}
								className="h-8 pl-8 pr-7 text-xs font-mono"
								value={search}
								onChange={(e) => {
									setSearch(e.target.value);
									setPage(1);
								}}
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

						{/* View Mode Toggle */}
						<div className="flex items-center rounded-lg border border-border/80 bg-muted/40 p-0.5">
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
								<TooltipContent>{t("guardProbes.viewCards")}</TooltipContent>
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
								<TooltipContent>{t("guardProbes.viewTable")}</TooltipContent>
							</Tooltip>
						</div>

						{/* Refresh */}
						<Tooltip>
							<TooltipTrigger asChild>
								<Button
									variant="outline"
									size="icon"
									className="size-8"
									onClick={() => void probesQuery.refetch()}
									disabled={probesQuery.isFetching}
								>
									<RefreshCw className={cn("size-3.5", probesQuery.isFetching && "animate-spin")} />
								</Button>
							</TooltipTrigger>
							<TooltipContent>{t("common.refresh")}</TooltipContent>
						</Tooltip>
					</div>
				</div>

				{/* Summary Meta Line */}
				<div className="flex items-center justify-between text-[11px] text-muted-foreground border-t border-border/50 pt-2 px-0.5">
					<div className="flex items-center gap-2">
						<span>{t("guardProbes.totalRecords", { count: probes.length })}</span>
						{(filterDirection !== "all" || filterResult !== "all" || search.trim()) && (
							<span className="font-semibold text-primary">
								· {t("guardProbes.filteredRecords", { count: filteredProbes.length })}
							</span>
						)}
					</div>
					<span className="font-mono text-[10px]">
						{t("guardProbes.telemetryWindow", { window: windowMinutes })}
					</span>
				</div>
			</div>

			{/* Main Content: Cards vs Table */}
			{accountsQuery.isError && <LoadFailed onRetry={() => void accountsQuery.refetch()} />}
			{probesQuery.isError ? (
				<LoadFailed
					message={String(probesQuery.error)}
					onRetry={() => void probesQuery.refetch()}
				/>
			) : probesQuery.isLoading ? (
				<div className="flex min-h-48 items-center justify-center rounded-xl border border-dashed border-border/80 p-8">
					<div className="flex flex-col items-center gap-2 text-muted-foreground">
						<Spinner className="size-6 text-primary" />
						<span className="text-xs">{t("common.loading")}</span>
					</div>
				</div>
			) : filteredProbes.length === 0 ? (
				<div className="flex min-h-48 flex-col items-center justify-center rounded-xl border border-dashed border-border/80 bg-card/40 p-8 text-center">
					<Search className="size-8 text-muted-foreground/30 mb-2" />
					<p className="text-sm font-semibold text-foreground">
						{t("guardProbes.emptyTitle")}
					</p>
					<p className="text-xs text-muted-foreground mt-1 max-w-md">
						{t("guardProbes.emptyDesc")}
					</p>
				</div>
			) : viewMode === "cards" ? (
				/* Cards Timeline View */
				<div className="grid grid-cols-1 gap-3.5 lg:grid-cols-2">
					{pageItems.map((task) => (
						<ProbeTaskCard
							key={task.id}
							task={task}
							accounts={accountIdentity}
							nodes={nodeIdentity}
							ipByNode={ipByNode}
							timeFormatter={timeFormatter}
							onInspect={() => setInspectTask(task)}
						/>
					))}
				</div>
			) : (
				/* High-Density Data Table View */
				<div className="overflow-hidden rounded-xl border border-border/80 bg-card/60 shadow-xs">
					<Table>
						<TableHeader>
							<TableRow className="hover:bg-transparent">
								<TableHead className="w-16 font-mono text-xs">{t("quality.bureau.colId")}</TableHead>
								<TableHead className="w-28 text-xs">{t("quality.bureau.colDirection")}</TableHead>
								<TableHead className="w-24 font-mono text-xs">{t("quality.bureau.colCase")}</TableHead>
								<TableHead className="text-xs">{t("quality.bureau.colParties")}</TableHead>
								<TableHead className="w-24 text-xs">{t("quality.bureau.colResult")}</TableHead>
								<TableHead className="text-xs">诊断分析</TableHead>
								<TableHead className="w-20 text-right text-xs">{t("quality.bureau.colDuration")}</TableHead>
								<TableHead className="w-24 text-right text-xs">{t("quality.bureau.colCreated")}</TableHead>
								<TableHead className="w-16 text-right text-xs">{t("common.actions")}</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{pageItems.map((task) => {
								const duration = probeDurationMs(task);
								const finding = getProbeFinding(task, t);
								return (
									<TableRow
										key={task.id}
										className="h-12 text-xs transition-colors hover:bg-muted/40 cursor-pointer"
										onClick={() => setInspectTask(task)}
									>
										<TableCell className="font-mono font-bold text-foreground">
											#{task.id}
										</TableCell>
										<TableCell>
											{task.direction === "account_differential" ? (
												<Badge variant="outline" className="text-[10px] gap-1 px-1.5 py-0 border-indigo-500/30 text-indigo-600 dark:text-indigo-400 bg-indigo-500/5">
													<Users className="size-3 shrink-0" />
													<span>{t("guardProbes.directionAccount")}</span>
												</Badge>
											) : (
												<Badge variant="outline" className="text-[10px] gap-1 px-1.5 py-0 border-amber-500/30 text-amber-600 dark:text-amber-400 bg-amber-500/5">
													<Gavel className="size-3 shrink-0" />
													<span>{t("guardProbes.directionJury")}</span>
												</Badge>
											)}
										</TableCell>
										<TableCell className="font-mono">
											<Link
												to={`/guard?case=${task.case_id}#tribunal`}
												onClick={(e) => e.stopPropagation()}
												className="inline-flex items-center gap-1 font-semibold text-primary hover:underline"
											>
												<span>#{task.case_id}</span>
												<ExternalLink className="size-2.5 opacity-60" />
											</Link>
										</TableCell>
										<TableCell className="min-w-[240px]">
											<ProbePartiesSummary
												task={task}
												accounts={accountIdentity}
												nodes={nodeIdentity}
												ipByNode={ipByNode}
											/>
										</TableCell>
										<TableCell>
											<ResultBadge result={task.result} state={task.state} t={t} />
										</TableCell>
										<TableCell className="max-w-[280px]">
											<p className="truncate text-[11px] text-muted-foreground" title={finding.text}>
												{finding.text}
											</p>
										</TableCell>
										<TableCell className="text-right font-mono tabular-nums text-muted-foreground">
											{duration === null ? "—" : `${(duration / 1000).toFixed(1)}s`}
										</TableCell>
										<TableCell className="text-right font-mono text-[11px] text-muted-foreground">
											{timeFormatter.format(new Date(task.created_at))}
										</TableCell>
										<TableCell className="text-right" onClick={(e) => e.stopPropagation()}>
											<Button
												size="sm"
												variant="ghost"
												className="size-7 p-0 text-muted-foreground hover:text-foreground"
												onClick={() => setInspectTask(task)}
											>
												<Eye className="size-3.5" />
											</Button>
										</TableCell>
									</TableRow>
								);
							})}
						</TableBody>
					</Table>
				</div>
			)}

			{/* Pagination */}
			{filteredProbes.length > PROBE_PAGE_SIZE && (
				<Pagination
					page={currentPage}
					pageSize={PROBE_PAGE_SIZE}
					total={filteredProbes.length}
					onPageChange={setPage}
					className="justify-end border-t border-border/60 pt-3"
				/>
			)}

			{/* Forensic Inspection Modal */}
			{inspectTask && (
				<ForensicProbeModal
					task={inspectTask}
					accounts={accountIdentity}
					nodes={nodeIdentity}
					ipByNode={ipByNode}
					open={Boolean(inspectTask)}
					onClose={() => setInspectTask(null)}
				/>
			)}
		</div>
	);
}

/**
 * Visual Probe Pipeline Card (Card Timeline View)
 */
const ProbeTaskCard = memo(function ProbeTaskCard({
	task,
	accounts,
	nodes,
	ipByNode,
	timeFormatter,
	onInspect,
}: {
	task: QualityProbeTask;
	accounts: Map<number, QualityAccountIdentity>;
	nodes: Map<number, QualityNodeIdentity>;
	ipByNode: QualityExitIPIndex;
	timeFormatter: Intl.DateTimeFormat;
	onInspect: () => void;
}) {
	const { t } = useTranslation();
	const duration = probeDurationMs(task);
	const finding = getProbeFinding(task, t);

	return (
		<div
			className={cn(
				"group relative flex flex-col justify-between rounded-xl border border-border/80 bg-card/60 p-4 transition-all duration-200 hover:border-primary/40 hover:bg-card hover:shadow-md cursor-pointer",
				task.result === "degraded" && "border-rose-500/30 bg-rose-500/[0.02]",
			)}
			onClick={onInspect}
		>
			{/* Header Row */}
			<div>
				<div className="flex items-center justify-between gap-2 border-b border-border/50 pb-2.5">
					<div className="flex items-center gap-2 min-w-0">
						<span className="font-mono text-xs font-bold text-foreground">
							#{task.id}
						</span>
						{task.direction === "account_differential" ? (
							<Badge variant="outline" className="text-[10px] gap-1 px-2 py-0 border-indigo-500/30 text-indigo-600 dark:text-indigo-400 bg-indigo-500/5">
								<Users className="size-3 shrink-0" />
								<span>{t("guardProbes.directionAccount")}</span>
							</Badge>
						) : (
							<Badge variant="outline" className="text-[10px] gap-1 px-2 py-0 border-amber-500/30 text-amber-600 dark:text-amber-400 bg-amber-500/5">
								<Gavel className="size-3 shrink-0" />
								<span>{t("guardProbes.directionJury")}</span>
							</Badge>
						)}
						<Link
							to={`/guard?case=${task.case_id}#tribunal`}
							onClick={(e) => e.stopPropagation()}
							className="inline-flex items-center gap-0.5 text-[11px] font-mono font-medium text-primary hover:underline ml-1"
						>
							<span>#{task.case_id}</span>
							<ExternalLink className="size-2.5 opacity-60" />
						</Link>
					</div>

					<ResultBadge result={task.result} state={task.state} t={t} />
				</div>

				{/* Visual Variable Pipeline Flow */}
				<div className="mt-3 rounded-lg border border-border/60 bg-muted/20 p-3">
					{task.direction === "account_differential" ? (
						<div className="flex flex-col gap-2">
							{/* Target Account */}
							<div className="flex items-center justify-between text-xs">
								<div className="flex items-center gap-1.5 min-w-0">
									<User className="size-3.5 text-indigo-500 shrink-0" />
									<span className="text-[11px] font-semibold text-muted-foreground shrink-0">
										{t("guardProbes.defendantAccount")}:
									</span>
									<QualityAccountReference
										id={task.defendant}
										accounts={accounts}
										className="max-w-[200px]"
									/>
								</div>
								{task.verified_ip_change !== undefined && (
									<span
										className={cn(
											"rounded px-1.5 py-0.5 text-[9px] font-mono font-medium",
											task.verified_ip_change
												? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
												: "bg-muted text-muted-foreground",
										)}
									>
										{task.verified_ip_change ? t("guardProbes.ipChangeVerified") : t("guardProbes.ipChangeUnverified")}
									</span>
								)}
							</div>

							{/* Exit Comparison Route */}
							<div className="flex items-center gap-2 text-xs border-t border-border/40 pt-2">
								<div className="flex items-center gap-1.5 min-w-0 flex-1">
									<span className="text-[10px] text-muted-foreground shrink-0">{t("guardProbes.baselineExit")}:</span>
									{task.baseline_node_id && task.baseline_node_id > 0 ? (
										<QualityExitReference
											node={task.baseline_node_id}
											epoch={task.baseline_epoch ?? 0}
											nodes={nodes}
											ipByNode={ipByNode}
											className="max-w-[130px]"
										/>
									) : (
										<span className="text-[11px] text-muted-foreground/60">—</span>
									)}
								</div>

								<ArrowRight className="size-3.5 text-muted-foreground/60 shrink-0" />

								<div className="flex items-center gap-1.5 min-w-0 flex-1">
									<span className="text-[10px] text-muted-foreground shrink-0">{t("guardProbes.comparisonExit")}:</span>
									<QualityExitReference
										node={task.node_id}
										epoch={task.epoch}
										nodes={nodes}
										ipByNode={ipByNode}
										className="max-w-[130px]"
									/>
								</div>
							</div>
						</div>
					) : (
						/* Exit Jury Flow */
						<div className="flex flex-col gap-2">
							<div className="flex items-center gap-1.5 text-xs">
								<Scale className="size-3.5 text-amber-500 shrink-0" />
								<span className="text-[11px] font-semibold text-muted-foreground shrink-0">
									{t("guardProbes.jurorAccount")}:
								</span>
								<QualityAccountReference
									id={task.juror}
									accounts={accounts}
									className="max-w-[220px]"
								/>
							</div>

							<div className="flex items-center gap-1.5 text-xs border-t border-border/40 pt-2">
								<span className="text-[10px] text-muted-foreground shrink-0">{t("guardProbes.suspectExit")}:</span>
								<QualityExitReference
									node={task.node_id}
									epoch={task.epoch}
									nodes={nodes}
									ipByNode={ipByNode}
									className="max-w-[260px]"
								/>
							</div>
						</div>
					)}
				</div>

				{/* Findings & Verdict Summary (Clean Humanized Box) */}
				<div
					className={cn(
						"mt-2.5 rounded-lg border p-2.5 text-xs leading-relaxed",
						finding.tone === "ok"
							? "border-emerald-500/20 bg-emerald-500/5 text-emerald-950 dark:text-emerald-200"
							: finding.tone === "bad"
								? "border-rose-500/20 bg-rose-500/5 text-rose-950 dark:text-rose-200"
								: "border-amber-500/20 bg-amber-500/5 text-amber-950 dark:text-amber-200",
					)}
				>
					<div className="flex items-start gap-1.5">
						{finding.tone === "ok" ? (
							<CheckCircle2 className="size-3.5 text-emerald-500 shrink-0 mt-0.5" />
						) : finding.tone === "bad" ? (
							<AlertTriangle className="size-3.5 text-rose-500 shrink-0 mt-0.5" />
						) : (
							<AlertCircle className="size-3.5 text-amber-500 shrink-0 mt-0.5" />
						)}
						<span className="text-[11px] font-medium">{finding.text}</span>
					</div>
				</div>
			</div>

			{/* Card Footer: Metrics & Details Trigger */}
			<div className="mt-3 flex items-center justify-between border-t border-border/50 pt-2.5 text-[11px] text-muted-foreground">
				<div className="flex items-center gap-2 font-mono">
					<span className="flex items-center gap-1">
						<Timer className="size-3 text-muted-foreground/70" />
						{duration === null ? "—" : `${(duration / 1000).toFixed(1)}s`}
					</span>
					<span>·</span>
					<span className="flex items-center gap-1">
						<Clock className="size-3 text-muted-foreground/70" />
						{timeFormatter.format(new Date(task.created_at))}
					</span>
				</div>

				<Button
					size="sm"
					variant="ghost"
					className="h-6 gap-1 px-2 text-[11px] font-medium text-primary hover:text-primary/80 group-hover:bg-primary/10"
					onClick={(e) => {
						e.stopPropagation();
						onInspect();
					}}
				>
					<span>{t("guardProbes.viewReportBtn")}</span>
					<ArrowRight className="size-3" />
				</Button>
			</div>
		</div>
	);
});

/**
 * Result & State Badge
 */
function ResultBadge({
	result,
	state,
	t,
}: {
	result: string;
	state: string;
	t: (key: string) => string;
}) {
	if (state === "running") {
		return (
			<Badge variant="outline" className="text-[10px] gap-1 px-1.5 py-0 border-blue-500/40 text-blue-600 dark:text-blue-400 bg-blue-500/10 animate-pulse">
				<span className="size-1.5 rounded-full bg-blue-500" />
				<span>{t("guardProbes.stateRunning")}</span>
			</Badge>
		);
	}

	if (state === "pending") {
		return (
			<Badge variant="outline" className="text-[10px] px-1.5 py-0 text-muted-foreground bg-muted/40">
				<span>{t("guardProbes.statePending")}</span>
			</Badge>
		);
	}

	if (result === "clean") {
		return (
			<Badge variant="outline" className="text-[10px] gap-1 px-1.5 py-0 border-emerald-500/40 text-emerald-600 dark:text-emerald-400 bg-emerald-500/10">
				<CheckCircle2 className="size-3" />
				<span>{t("guardProbes.resultClean")}</span>
			</Badge>
		);
	}

	if (result === "degraded") {
		return (
			<Badge variant="destructive" className="text-[10px] gap-1 px-1.5 py-0 animate-pulse">
				<AlertTriangle className="size-3" />
				<span>{t("guardProbes.resultDegraded")}</span>
			</Badge>
		);
	}

	return (
		<Badge variant="outline" className="text-[10px] gap-1 px-1.5 py-0 border-amber-500/40 text-amber-600 dark:text-amber-400 bg-amber-500/10">
			<AlertCircle className="size-3" />
			<span>{t("guardProbes.resultError")}</span>
		</Badge>
	);
}

/**
 * Probe Parties Table Summary
 */
function ProbePartiesSummary({
	task,
	accounts,
	nodes,
	ipByNode,
}: {
	task: QualityProbeTask;
	accounts: Map<number, QualityAccountIdentity>;
	nodes: Map<number, QualityNodeIdentity>;
	ipByNode: QualityExitIPIndex;
}) {
	if (task.direction === "account_differential") {
		return (
			<div className="flex flex-col gap-0.5 leading-tight">
				<div className="flex items-center gap-1 font-medium">
					<User className="size-3 text-indigo-500" />
					<QualityAccountReference id={task.defendant} accounts={accounts} className="max-w-[180px]" />
				</div>
				<div className="flex items-center gap-1 text-[11px] text-muted-foreground font-mono">
					{task.baseline_node_id ? (
						<QualityExitReference
							node={task.baseline_node_id}
							epoch={task.baseline_epoch ?? 0}
							nodes={nodes}
							ipByNode={ipByNode}
							className="max-w-[100px]"
						/>
					) : (
						<span>—</span>
					)}
					<span>→</span>
					<QualityExitReference
						node={task.node_id}
						epoch={task.epoch}
						nodes={nodes}
						ipByNode={ipByNode}
						className="max-w-[100px]"
					/>
				</div>
			</div>
		);
	}

	return (
		<div className="flex flex-col gap-0.5 leading-tight">
			<div className="flex items-center gap-1 font-medium">
				<Scale className="size-3 text-amber-500" />
				<QualityAccountReference id={task.juror} accounts={accounts} className="max-w-[180px]" />
			</div>
			<div className="flex items-center gap-1 text-[11px] text-muted-foreground font-mono">
				<span>→</span>
				<QualityExitReference
					node={task.node_id}
					epoch={task.epoch}
					nodes={nodes}
					ipByNode={ipByNode}
					className="max-w-[180px]"
				/>
			</div>
		</div>
	);
}

/**
 * Detailed Forensic Modal
 */
function ForensicProbeModal({
	task,
	accounts,
	nodes,
	ipByNode,
	open,
	onClose,
}: {
	task: QualityProbeTask;
	accounts: Map<number, QualityAccountIdentity>;
	nodes: Map<number, QualityNodeIdentity>;
	ipByNode: QualityExitIPIndex;
	open: boolean;
	onClose: () => void;
}) {
	const { t } = useTranslation();
	const duration = probeDurationMs(task);
	const finding = getProbeFinding(task, t);

	return (
		<Dialog open={open} onOpenChange={(v) => !v && onClose()}>
			<OperationsDialogContent className="max-w-2xl max-h-[85vh] overflow-y-auto">
				<DialogHeader>
					<div className="flex items-center justify-between pr-6">
						<DialogTitle className="flex items-center gap-2 text-base font-bold">
							<ShieldCheck className="size-5 text-primary" />
							<span>{t("guardProbes.modalTitle", { id: task.id })}</span>
						</DialogTitle>
						<ResultBadge result={task.result} state={task.state} t={t} />
					</div>
					<p className="text-xs text-muted-foreground">
						<Link
							to={`/guard?case=${task.case_id}#tribunal`}
							className="text-primary underline underline-offset-4"
						>
							{t("guardProbes.modalCase", { caseId: task.case_id })}
						</Link>
					</p>
				</DialogHeader>

				<div className="space-y-4 py-2 text-xs">
					{/* Section 1: Methodology */}
					<div className="rounded-lg border border-border/80 bg-muted/20 p-3">
						<h4 className="font-semibold text-foreground mb-1.5 flex items-center gap-1.5">
							{task.direction === "account_differential" ? (
								<Users className="size-4 text-indigo-500" />
							) : (
								<Gavel className="size-4 text-amber-500" />
							)}
							<span>{t("guardProbes.methodologyTitle")}</span>
						</h4>
						<p className="text-[11px] text-muted-foreground leading-relaxed">
							{task.direction === "account_differential"
								? t("guardProbes.accountDifferentialDesc")
								: t("guardProbes.exitJuryDesc")}
						</p>

						<div className="mt-3 grid grid-cols-2 gap-3 border-t border-border/50 pt-2.5">
							<div>
								<span className="text-[10px] text-muted-foreground block mb-0.5">
									{task.direction === "account_differential"
										? t("guardProbes.defendantAccount")
										: t("guardProbes.jurorAccount")}
								</span>
								<QualityAccountReference
									id={task.direction === "account_differential" ? task.defendant : task.juror}
									accounts={accounts}
								/>
							</div>

							<div>
								<span className="text-[10px] text-muted-foreground block mb-0.5">
									{task.direction === "account_differential"
										? t("guardProbes.comparisonExit")
										: t("guardProbes.suspectExit")}
								</span>
								<QualityExitReference
									node={task.node_id}
									epoch={task.epoch}
									nodes={nodes}
									ipByNode={ipByNode}
								/>
							</div>
						</div>
					</div>

					{/* Section 2: Findings & Diagnostic */}
					<div className="rounded-lg border border-border/80 bg-card p-3">
						<h4 className="font-semibold text-foreground mb-1.5 flex items-center gap-1.5">
							<AlertCircle className="size-4 text-primary" />
							<span>{t("guardProbes.evidenceTitle")}</span>
						</h4>

						<div
							className={cn(
								"rounded-lg border p-3 leading-relaxed",
								finding.tone === "ok"
									? "border-emerald-500/30 bg-emerald-500/5 text-emerald-950 dark:text-emerald-200"
									: finding.tone === "bad"
										? "border-rose-500/30 bg-rose-500/5 text-rose-950 dark:text-rose-200"
										: "border-amber-500/30 bg-amber-500/5 text-amber-950 dark:text-amber-200",
							)}
						>
							<p className="font-medium text-xs mb-1">{finding.text}</p>
							{task.detail && (
								<p className="font-mono text-[10px] opacity-75 break-all mt-1">
									Raw Detail: {task.detail}
								</p>
							)}
						</div>

						{/* Control verification info */}
						{(task.control_outcome || task.control_detail) && (
							<div className="mt-2.5 flex items-center justify-between rounded-md bg-muted/40 px-2.5 py-1.5 text-[11px]">
								<span className="text-muted-foreground">
									{t("guardProbes.controlOutcome", { outcome: task.control_outcome || "clean" })}
								</span>
								<span className="font-mono text-[10px] text-muted-foreground">
									{task.control_verified ? t("guardProbes.controlVerified") : t("guardProbes.controlNotVerified")}
								</span>
							</div>
						)}
					</div>

					{/* Section 3: Telemetry */}
					<div className="rounded-lg border border-border/80 bg-muted/10 p-3">
						<h4 className="font-semibold text-foreground mb-1.5 flex items-center gap-1.5">
							<Timer className="size-4 text-muted-foreground" />
							<span>{t("guardProbes.telemetryTitle")}</span>
						</h4>

						<div className="grid grid-cols-3 gap-2 font-mono text-[11px]">
							<div className="rounded-md border border-border/40 bg-card/60 p-2">
								<span className="text-[10px] text-muted-foreground block">{t("guardProbes.durationLabel")}</span>
								<span className="font-bold text-foreground">
									{duration === null ? "—" : `${(duration / 1000).toFixed(2)}s`}
								</span>
							</div>

							<div className="rounded-md border border-border/40 bg-card/60 p-2">
								<span className="text-[10px] text-muted-foreground block">{t("guardProbes.createdAtLabel")}</span>
								<span className="text-foreground">
									{new Date(task.created_at).toLocaleTimeString()}
								</span>
							</div>

							<div className="rounded-md border border-border/40 bg-card/60 p-2">
								<span className="text-[10px] text-muted-foreground block">{t("guardProbes.finishedAtLabel")}</span>
								<span className="text-foreground">
									{task.finished_at ? new Date(task.finished_at).toLocaleTimeString() : "—"}
								</span>
							</div>
						</div>
					</div>
				</div>

				<div className="flex items-center gap-2 pt-3 border-t border-border/60">
					<Button variant="outline" size="sm" className="h-7 text-xs gap-1" asChild>
						<Link to={`/guard?case=${task.case_id}#tribunal`}>
							<Gavel className="size-3 text-primary" />
							<span>{t("guardProbes.actionGoCase")}</span>
						</Link>
					</Button>
					<Button variant="outline" size="sm" className="h-7 text-xs gap-1" asChild>
						<Link to="/proxies#nodes">
							<ExternalLink className="size-3 text-emerald-500" />
							<span>{t("guardProbes.actionGoNode")}</span>
						</Link>
					</Button>
				</div>
			</OperationsDialogContent>
		</Dialog>
	);
}


