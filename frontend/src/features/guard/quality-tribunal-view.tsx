import { CaseProofEvidence } from "./case-proof-evidence";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
	Activity,
	AlertCircle,
	ArrowRight,
	Gavel,
	Info,
	RefreshCw,
	Scale,
	Search,
	Server,
	ShieldCheck,
	Timer,
	Unlock,
	User,
	UserX,
	Waypoints,
} from "lucide-react";
import { memo, useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useSearchParams } from "react-router-dom";

import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import {
	Dialog,
	DialogHeader,
	DialogTitle,
} from "@/shared/ui/dialog";
import { Input } from "@/shared/ui/input";
import { Spinner } from "@/shared/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";

import {
	OperationsDialogContent as DialogContent,
	OperationsError,
	StatusPill,
} from "@/shared/ui/operations";
import { Pagination } from "@/shared/components/pagination";
import { cn } from "@/shared/lib/cn";

import {
	fetchQualityNodes,
	fetchQualityProbesForCase,
	releaseQualityCase,
	triggerQualityReview,
	type ExperimentReport,
	type QualityCase,
} from "@/entities/guard/quality-api";
import { caseDispositionKey, partyDispositionKey } from "./quality-case-presentation";
import { QualityAccountReference, QualityExitReference } from "./quality-identity";
import { getProbeFinding } from "./quality-view";
import { useAccountDirectory, useRestrictedAccountCount } from "@/entities/account/account-queries";
import { useEgressNodes } from "@/entities/egress/egress-queries";
import { useQualityCases } from "@/entities/guard/guard-queries";
import {
	buildQualityExitIPIndex,
	qualityAccountDisplay,
	qualityExitDisplay,
	type QualityAccountIdentity,
	type QualityExitIPIndex,
	type QualityNodeIdentity,
} from "./quality-view";

type Identities = {
	accounts: Map<number, QualityAccountIdentity>;
	nodes: Map<number, QualityNodeIdentity>;
	ipByNode: QualityExitIPIndex;
};

function reportOf(item: QualityCase): ExperimentReport | undefined {
	return (
		item.live?.assessment ??
		(item.evidence?.assessment as ExperimentReport | undefined)
	);
}

function verdictKey(item: QualityCase) {
	return item.status === "investigating"
		? "pending"
		: item.verdict === "both_guilty"
			? "bothFinding"
		: item.verdict === "account_guilty"
			? "accountFinding"
			: item.verdict === "exit_guilty"
				? "exitFinding"
				: "inconclusive";
}

function verdictTone(item: QualityCase) {
	return item.status === "investigating"
		? "warn"
		: ["account_guilty", "exit_guilty", "both_guilty"].includes(item.verdict)
			? "bad"
			: "neutral";
}

export function LoadFailed({ message, onRetry, retry }: { message?: string; onRetry?: () => void; retry?: () => void }) {
	return <OperationsError message={message} retry={onRetry || retry} />;
}

export const QualityTribunalView = memo(function QualityTribunalView() {
	const { t, i18n } = useTranslation();
	const isZh = i18n.language.startsWith("zh");
	const cache = useQueryClient();
	const [params] = useSearchParams();
	const navigate = useNavigate();

	const [selected, setSelected] = useState<number | null>(null);
	const [inspecting, setInspecting] = useState(false);
	const linkedCase = Number(params.get("case"));
	const selectedId = linkedCase || selected;
	const dialogOpen = Boolean(linkedCase) || inspecting;

	const [filter, setFilter] = useState("all");
	const [search, setSearch] = useState("");
	const [page, setPage] = useState(1);

	const cases = useQualityCases();
	const accounts = useAccountDirectory((cases.data ?? []).flatMap((item) => item.parties.map((party) => party.account_id)));
	const restricted = useRestrictedAccountCount();
	const nodes = useEgressNodes();

	const ips = useQuery({
		queryKey: ["quality", "nodes"],
		queryFn: ({ signal }) => fetchQualityNodes(signal),
		staleTime: 30_000,
		refetchInterval: 30_000,
	});

	const reviewMutation = useMutation({
		mutationFn: () => triggerQualityReview(),
		onSuccess: () => {
			void cache.invalidateQueries({ queryKey: ["quality"] });
			void cache.invalidateQueries({ queryKey: ["accounts"] });
			void cache.invalidateQueries({ queryKey: ["egress-nodes"] });
		},
	});

	const identities = useMemo<Identities>(
		() => ({
			accounts: new Map(
				(accounts.data?.items ?? []).map((a) => [
					Number(a.id),
					{ name: a.name, email: a.email },
				])
			),
			nodes: new Map(
				(nodes.data?.items ?? []).map((n) => [Number(n.id), { name: n.name }])
			),
			ipByNode: buildQualityExitIPIndex(ips.data ?? []),
		}),
		[accounts.data, nodes.data, ips.data]
	);

	const heldAccountCount = restricted.isError ? undefined : restricted.data?.total;
	const hasHeldAccounts = (heldAccountCount ?? 0) > 0;
	const heldExits = useMemo(
		() => (nodes.data?.items ?? []).filter((n) => n.quality),
		[nodes.data]
	);

	const openCases = useMemo(
		() => (cases.data ?? []).filter((c) => c.status === "investigating"),
		[cases.data]
	);

	// Filtered cases
	const filteredCases = useMemo(() => {
		const raw = cases.data ?? [];
		const needle = search.trim().toLowerCase();

		return raw
			.filter((item) => {
				if (filter === "open" && item.status !== "investigating") return false;
				if (filter === "closed" && item.status === "investigating") return false;
				if (filter === "exit_guilty" && !["exit_guilty", "both_guilty"].includes(item.verdict)) return false;
				if (filter === "account_guilty" && !["account_guilty", "both_guilty"].includes(item.verdict)) return false;
				if (filter === "cleared" && item.status === "investigating") return false;
				if (
					filter === "cleared" &&
					item.verdict !== "inconclusive" && item.verdict !== "insufficient" &&
					item.verdict !== "dismissed"
				)
					return false;

				if (!needle) return true;
				return [
					String(item.id),
					...item.parties.map((p) =>
						p.kind === "account"
							? qualityAccountDisplay(p.account_id, identities.accounts.get(p.account_id)).title
							: qualityExitDisplay(p.node_id, p.epoch, identities.nodes.get(p.node_id), identities.ipByNode).title
					),
				]
					.join(" ")
					.toLowerCase()
					.includes(needle);
			})
			.sort(
				(a, b) =>
					Number(b.status === "investigating") - Number(a.status === "investigating") ||
					b.id - a.id
			);
	}, [cases.data, filter, search, identities]);

	const pageSize = 6;
	const totalPages = Math.max(1, Math.ceil(filteredCases.length / pageSize));
	const pageIndex = Math.min(page, totalPages);
	const paginatedCases = useMemo(
		() => filteredCases.slice((pageIndex - 1) * pageSize, pageIndex * pageSize),
		[filteredCases, pageIndex]
	);

	const chosen = useMemo(
		() => cases.data?.find((c) => c.id === selectedId),
		[cases.data, selectedId]
	);

	const close = useCallback(() => {
		setSelected(selectedId);
		setInspecting(false);
		if (params.has("case")) {
			const next = new URLSearchParams(params);
			next.delete("case");
			navigate(
				{ pathname: "/guard", search: next.toString(), hash: "tribunal" },
				{ replace: true }
			);
		}
	}, [selectedId, params, navigate]);

	return (
		<div className="flex flex-col gap-5 min-w-0 pb-2">
			{(accounts.isError || restricted.isError) && <LoadFailed retry={() => { void cache.invalidateQueries({ queryKey: ["accounts"] }); }} />}
			{/* Top 4 KPI Velocity Cards */}
			<div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
				{/* Card 1: Active In-Flight Investigations */}
				<div
					onClick={() => {
						setFilter("open");
						setPage(1);
					}}
					className={cn(
						"group relative overflow-hidden rounded-xl border p-4 shadow-sm backdrop-blur-sm transition-all cursor-pointer hover:shadow-md",
						openCases.length > 0
							? "border-amber-500/40 bg-amber-500/5 ring-1 ring-amber-500/20"
							: "border-border/80 bg-card/75 hover:border-border"
					)}
				>
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Scale className="size-4 text-amber-500" />
							<span>{isZh ? "在审归因案件" : "Active Cases"}</span>
						</span>
						<Badge
							variant="outline"
							className={cn(
								"text-[10px] font-semibold",
								openCases.length > 0
									? "text-amber-600 dark:text-amber-400 border-amber-500/30 bg-amber-500/10"
									: "text-muted-foreground"
							)}
						>
							{openCases.length > 0 ? (isZh ? "取证进行中" : "Attribution") : (isZh ? "全量健康" : "Clean")}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{openCases.length}
						</span>
						<span className="text-xs text-muted-foreground font-medium">
							{isZh ? "起异常调查中" : "in flight"}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80 truncate">
						{openCases.length > 0
							? isZh ? "正在执行双向变量隔离与交叉测试" : "Running cross-validation probes"
							: isZh ? "未检测到新的降智或响应异常" : "No active degradation cases"}
					</p>
				</div>

				{/* Card 2: Restricted Suspect Accounts (Click to manage accounts) */}
				<div
					onClick={() => navigate("/accounts?quality=restricted")}
					className={cn(
						"group relative overflow-hidden rounded-xl border p-4 shadow-sm backdrop-blur-sm transition-all cursor-pointer hover:shadow-md",
						hasHeldAccounts
							? "border-purple-500/40 bg-purple-500/5 ring-1 ring-purple-500/20 hover:border-purple-500/60"
							: "border-border/80 bg-card/75 hover:border-border"
					)}
					title={isZh ? "点击前往账号列表查看与管理" : "View and manage accounts"}
				>
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<UserX className="size-4 text-purple-500" />
							<span>{isZh ? "涉案受限账号" : "Held Accounts"}</span>
						</span>
						<Badge
							variant="outline"
							className={cn(
								"text-[10px] font-semibold",
								hasHeldAccounts
									? "text-purple-600 dark:text-purple-400 border-purple-500/30 bg-purple-500/10"
									: "text-muted-foreground"
							)}
						>
							{heldAccountCount === undefined ? t("ops.unknown") : hasHeldAccounts ? (isZh ? "受限隔离" : "Restricted") : (isZh ? "无质量限制" : "No quality holds")}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{heldAccountCount ?? "—"}
						</span>
						<span className="text-xs text-muted-foreground font-medium">
							{isZh ? "个账号暂停调度" : "held"}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80 truncate">
						{heldAccountCount === undefined ? t("ops.unknown") : hasHeldAccounts
							? isZh ? "点击前往账号列表管理与解禁 →" : "Click to manage in accounts"
							: isZh ? "当前没有被质量案件限制的账号" : "No current accounts are held by quality cases"}
					</p>
				</div>

				{/* Card 3: Quarantined Exits (Click to manage proxy network) */}
				<div
					onClick={() => navigate("/proxies#nodes")}
					className={cn(
						"group relative overflow-hidden rounded-xl border p-4 shadow-sm backdrop-blur-sm transition-all cursor-pointer hover:shadow-md",
						heldExits.length > 0
							? "border-rose-500/40 bg-rose-500/5 ring-1 ring-rose-500/20 hover:border-rose-500/60"
							: "border-border/80 bg-card/75 hover:border-border"
					)}
					title={isZh ? "点击前往代理网络更换 IP 或解禁" : "View and rotate exits in proxies"}
				>
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Waypoints className="size-4 text-rose-500" />
							<span>{isZh ? "受限隔离出口" : "Quarantined Exits"}</span>
						</span>
						<Badge
							variant="outline"
							className={cn(
								"text-[10px] font-semibold",
								heldExits.length > 0
									? "text-rose-600 dark:text-rose-400 border-rose-500/30 bg-rose-500/10"
									: "text-muted-foreground"
							)}
						>
							{heldExits.length > 0 ? (isZh ? "IP禁用/冻结" : "Banned") : (isZh ? "网络就绪" : "All Clean")}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{heldExits.length}
						</span>
						<span className="text-xs text-muted-foreground font-medium">
							{isZh ? "个出口暂不接单" : "quarantined"}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80 truncate">
						{heldExits.length > 0
							? isZh ? "点击前往代理网络更换 IP →" : "Click to rotate IP in proxies"
							: isZh ? "未发现降智或污染出口 IP" : "All egress IPs unpolluted"}
					</p>
				</div>

				{/* Card 4: Historical Intercepted Fallback Protection */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<ShieldCheck className="size-4 text-emerald-500" />
							<span>{isZh ? "累计结案归因" : "Resolved Attribution"}</span>
						</span>
						<Badge variant="outline" className="text-[10px] font-semibold text-emerald-600 dark:text-emerald-400 border-emerald-500/30 bg-emerald-500/5">
							{isZh ? "历史复盘" : "Archive"}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{(cases.data?.length ?? 0) - openCases.length}
						</span>
						<span className="text-xs text-muted-foreground font-medium">
							{isZh ? `起已完成判定 (总共 ${cases.data?.length ?? 0})` : `cases resolved`}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80 truncate">
						{isZh ? "证据完备归因闭环，保全全站可用率" : "Full scientific forensic history preserved"}
					</p>
				</div>
			</div>

			{/* Filter Toolbar, Search & Review Evaluation Action */}
			<div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
				{/* Scope Filter Pills */}
				<div className="flex items-center gap-1.5 overflow-x-auto whitespace-nowrap scrollbar-none py-0.5">
					{[
						{ id: "all", label: isZh ? "全部案件" : "All Cases", count: cases.data?.length ?? 0 },
						{ id: "open", label: isZh ? "调查中" : "In Flight", count: openCases.length, dot: "bg-amber-500" },
						{
							id: "exit_guilty",
							label: isZh ? "出口有罪" : "Exit Guilty",
							count: (cases.data ?? []).filter((c) => ["exit_guilty", "both_guilty"].includes(c.verdict)).length,
							dot: "bg-rose-500",
						},
						{
							id: "account_guilty",
							label: isZh ? "账号有罪" : "Account Guilty",
							count: (cases.data ?? []).filter((c) => ["account_guilty", "both_guilty"].includes(c.verdict)).length,
							dot: "bg-purple-500",
						},
						{
							id: "cleared",
							label: isZh ? "已免罚结案" : "Cleared",
							count: (cases.data ?? []).filter((c) => c.status !== "investigating" && (c.verdict === "inconclusive" || c.verdict === "insufficient" || c.verdict === "dismissed")).length,
						},
					].map((item) => (
						<button
							type="button"
							key={item.id}
							onClick={() => {
								setFilter(item.id);
								setPage(1);
							}}
							className={cn(
								"flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-all cursor-pointer",
								filter === item.id
									? "bg-primary text-primary-foreground shadow-sm"
									: "bg-card/70 border border-border/80 text-muted-foreground hover:border-border hover:text-foreground"
							)}
						>
							{item.dot && (
								<span
									className={cn(
										"size-1.5 rounded-full",
										item.dot,
										item.id === "open" && "animate-ping"
									)}
								/>
							)}
							<span>{item.label}</span>
							<span
								className={cn(
									"rounded px-1 text-[10px] tabular-nums font-mono",
									filter === item.id
										? "bg-primary-foreground/20 text-primary-foreground"
										: "bg-muted text-muted-foreground"
								)}
							>
								{item.count}
							</span>
						</button>
					))}
				</div>

				{/* Search & Immediate Evaluation Action */}
				<div className="flex items-center gap-2">
					<div className="relative min-w-[220px]">
						<Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
						<Input
							value={search}
							onChange={(e) => {
								setSearch(e.target.value);
								setPage(1);
							}}
							placeholder={isZh ? "搜索案件 #ID、账号、出口或 IP..." : "Search case ID, account, exit..."}
							className="h-8 pl-8 text-xs"
						/>
					</div>

					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="outline"
								size="sm"
								className="h-8 gap-1.5 text-xs font-medium"
								disabled={reviewMutation.isPending}
								onClick={() => reviewMutation.mutate()}
							>
								{reviewMutation.isPending ? (
									<Spinner className="size-3.5" />
								) : (
									<RefreshCw className="size-3.5 text-primary" />
								)}
								<span>{isZh ? "立即复核评估" : "Review Now"}</span>
							</Button>
						</TooltipTrigger>
						<TooltipContent>{isZh ? "立即触发规则引擎执行交叉证据评估" : "Trigger cross-evidence evaluation"}</TooltipContent>
					</Tooltip>
				</div>
			</div>

			{/* Main Forensic Cases Presentation Deck */}
			{cases.isPending ? (
				<div className="flex h-64 flex-col items-center justify-center gap-3 rounded-xl border border-border/80 bg-card/40">
					<Spinner className="size-6 text-primary" />
					<p className="text-xs text-muted-foreground">{t("common.loading")}</p>
				</div>
			) : filteredCases.length === 0 ? (
				<div className="flex h-56 flex-col items-center justify-center rounded-xl border border-dashed border-border/80 bg-card/40 p-6 text-center">
					<Gavel className="size-8 text-muted-foreground/40 mb-2" />
					<p className="text-sm font-semibold text-foreground">
						{search ? (isZh ? "未匹配到相关案件" : "No Matching Cases") : (isZh ? "暂无质量归因案件" : "No Cases Recorded")}
					</p>
					<p className="text-xs text-muted-foreground mt-1 max-w-md">
						{search
							? (isZh ? "请尝试调整搜索关键词或清空筛选条件。" : "Try adjusting your search criteria.")
							: (isZh ? "当系统在上游交互中捕获到降智或协议异常时，两组对照实验与仲裁流会自动呈现在这里。" : "Attribution cases will appear when quality anomalies are detected.")}
					</p>
				</div>
			) : (
				<div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
					{paginatedCases.map((item) => {
						const report = reportOf(item);
						const account = item.parties.find((p) => p.kind === "account");
						const exit = item.parties.find((p) => p.kind === "exit");
						const isInvestigating = item.status === "investigating";
						const dispKey = caseDispositionKey(item);

						const accDisplay = account
							? qualityAccountDisplay(account.account_id, identities.accounts.get(account.account_id))
							: null;
						const exitDisplay = exit
							? qualityExitDisplay(exit.node_id, exit.epoch, identities.nodes.get(exit.node_id), identities.ipByNode)
							: null;

						return (
							<div
								key={item.id}
								className={cn(
									"group relative flex flex-col justify-between rounded-xl border p-4 shadow-xs transition-all hover:shadow-md",
									isInvestigating
										? "border-amber-500/40 bg-amber-500/[0.03] ring-1 ring-amber-500/20"
										: "border-border/80 bg-card/70 hover:border-border"
								)}
							>
								<div>
									{/* Card Header: Case ID, Time, and Verdict Status */}
									<div className="flex items-center justify-between gap-2 border-b border-border/60 pb-3">
										<div className="flex items-center gap-2">
											<span className="font-mono text-sm font-black text-foreground">
												#{item.id}
											</span>
											<span className="text-[11px] text-muted-foreground">
												{new Date(item.opened_at).toLocaleTimeString([], {
													hour: "2-digit",
													minute: "2-digit",
													month: "2-digit",
													day: "2-digit",
												})}
											</span>
										</div>

										<StatusPill tone={verdictTone(item)}>
											{t(`ops.${verdictKey(item)}`)}
										</StatusPill>
									</div>

									{/* Accused Parties Confrontation Section */}
									<div className="mt-3 rounded-lg border border-border/60 bg-muted/20 p-3 space-y-2.5">
										{/* Suspect Account */}
										<div className="flex items-center justify-between text-xs">
											<div className="flex items-center gap-2 min-w-0">
												<User className="size-3.5 shrink-0 text-purple-500" />
												<div className="min-w-0">
													<p className="truncate font-semibold text-foreground" title={accDisplay?.title}>
														{accDisplay?.primary ?? (isZh ? "未绑定账号" : "Unknown")}
													</p>
													{accDisplay?.secondary && (
														<p className="truncate text-[10px] text-muted-foreground font-mono">
															{accDisplay.secondary}
														</p>
													)}
												</div>
											</div>

											{account && (
												<Badge
													variant={["remanded", "sentenced"].includes(account.disposition) ? "destructive" : "secondary"}
													className="text-[10px] px-1.5 py-0 shrink-0 font-mono"
												>
													{t(partyDispositionKey(account.disposition))}
												</Badge>
											)}
										</div>

										{/* VS Confrontation Divider */}
										<div className="relative flex items-center justify-center my-1">
											<div className="absolute inset-0 flex items-center">
												<span className="w-full border-t border-border/70 border-dashed" />
											</div>
											<span className="relative bg-card px-2 text-[9px] font-bold uppercase tracking-wider text-muted-foreground">
												VS 对照
											</span>
										</div>

										{/* Suspect Exit */}
										<div className="flex items-center justify-between text-xs">
											<div className="flex items-center gap-2 min-w-0">
												<Server className="size-3.5 shrink-0 text-rose-500" />
												<div className="min-w-0">
													<p className="truncate font-semibold text-foreground" title={exitDisplay?.title}>
														{exitDisplay?.primary ?? (isZh ? "直连/未知出口" : "Unknown")}
													</p>
													{exitDisplay?.secondary && (
														<p className="truncate text-[10px] text-muted-foreground font-mono">
															{exitDisplay.secondary}
														</p>
													)}
												</div>
											</div>

											{exit && (
												<Badge
													variant={exit.disposition === "remanded" || exit.disposition === "sentenced" ? "destructive" : "secondary"}
													className="text-[10px] px-1.5 py-0 shrink-0 font-mono"
												>
													{t(partyDispositionKey(exit.disposition))}
												</Badge>
											)}
										</div>
									</div>

									{/* Cross-Evidence Progress Gauge */}
									<div className="mt-3 space-y-2 text-xs">
										{report?.policy.version === "resource-proof-case-v1" ? (<p className="text-xs">{isZh ? "对照检测完成后归因" : "Attribution by comparison"} · {report.proof?.generations ?? 0} {isZh ? "次生成" : "generations"}</p>) : report ? (
											<div className="space-y-1.5">
												{/* Group 1: Exit Jury (固定出口 · 换正常账号) */}
												<div className="flex items-center justify-between text-[11px]">
													<span className="text-muted-foreground flex items-center gap-1">
														<Waypoints className="size-3 text-primary" />
														<span>{isZh ? "出口陪审组 (换正常账号)" : "Exit Jury"}</span>
													</span>
													<div className="flex items-center gap-1 font-mono text-[10px] tabular-nums">
														{report.exit.clean > 0 && <span className="text-emerald-500 font-bold">{report.exit.clean} 通过</span>}
														{report.exit.degraded > 0 && <span className="text-rose-500 font-bold">{report.exit.degraded} 降智</span>}
														{report.exit.pending > 0 && <span className="text-muted-foreground">{report.exit.pending} 待跑</span>}
													</div>
												</div>

												{/* Group 2: Account Differential (固定账号 · 换纯净出口) */}
												<div className="flex items-center justify-between text-[11px]">
													<span className="text-muted-foreground flex items-center gap-1">
														<User className="size-3 text-purple-500" />
														<span>{isZh ? "账号差分组 (换纯净出口)" : "Account Differential"}</span>
													</span>
													<div className="flex items-center gap-1 font-mono text-[10px] tabular-nums">
														{report.account.clean > 0 && <span className="text-emerald-500 font-bold">{report.account.clean} 通过</span>}
														{report.account.degraded > 0 && <span className="text-rose-500 font-bold">{report.account.degraded} 降智</span>}
														{report.account.pending > 0 && <span className="text-muted-foreground">{report.account.pending} 待跑</span>}
													</div>
												</div>
											</div>
										) : (
											<p className="text-[11px] text-muted-foreground">
												{t("ops.legacy")}
											</p>
										)}
									</div>
								</div>

								{/* Card Footer: Disposition Explanation & Inspect Button */}
								<div className="mt-4 border-t border-border/60 pt-3 flex items-center justify-between gap-2">
									<p className="text-[11px] text-muted-foreground line-clamp-1 flex-1">
										{t(`ops.${dispKey}`)}
									</p>

									<Button
										size="sm"
										variant="outline"
										className="h-7 text-xs gap-1 shrink-0 font-medium hover:bg-primary hover:text-primary-foreground"
										onClick={() => {
											setSelected(item.id);
											setInspecting(true);
										}}
									>
										<Gavel className="size-3" />
										<span>{isZh ? "案情审讯" : "Inspect"}</span>
									</Button>
								</div>
							</div>
						);
					})}
				</div>
			)}

			{/* Pagination */}
			{filteredCases.length > pageSize && (
				<div className="flex justify-center pt-2">
					<Pagination
						page={pageIndex}
						pageSize={pageSize}
						total={filteredCases.length}
						onPageChange={setPage}
					/>
				</div>
			)}

			{/* Detailed Forensic Case Modal */}
			{chosen && (
				<CaseExperiment
					key={chosen.id}
					item={chosen}
					open={dialogOpen}
					accountsMap={identities.accounts}
					nodesMap={identities.nodes}
					ipByNode={identities.ipByNode}
					onClose={close}
				/>
			)}
		</div>
	);
});

/** Detailed Case Experiment Modal - Aligned with Forensic Probe Report Design System */
function CaseExperiment({
	item,
	open,
	accountsMap,
	nodesMap,
	ipByNode,
	onClose,
}: {
	item: QualityCase;
	open: boolean;
	accountsMap: Map<number, QualityAccountIdentity>;
	nodesMap: Map<number, QualityNodeIdentity>;
	ipByNode: QualityExitIPIndex;
	onClose: () => void;
}) {
	return (
		<Dialog open={open} onOpenChange={(next) => { if (!next) onClose(); }}>
			<DialogContent className="sm:max-w-4xl max-h-[90vh] grid-cols-1 overflow-y-auto [overflow-wrap:anywhere]">
				<CaseExperimentContent
					item={item}
					accountsMap={accountsMap}
					nodesMap={nodesMap}
					ipByNode={ipByNode}
				/>
			</DialogContent>
		</Dialog>
	);
}

function CaseExperimentContent({
	item,
	accountsMap: caseAccountsMap,
	nodesMap,
	ipByNode,
}: {
	item: QualityCase;
	accountsMap: Map<number, QualityAccountIdentity>;
	nodesMap: Map<number, QualityNodeIdentity>;
	ipByNode: QualityExitIPIndex;
}) {
	const { t, i18n } = useTranslation();
	const isZh = i18n.language.startsWith("zh");
	const cache = useQueryClient();
	const [reviewReason, setReviewReason] = useState("");

	const manual = item.evidence?.manual_review as { reason?: string; at?: string } | undefined;
	const release = useMutation({
		mutationFn: () => releaseQualityCase(item.id, reviewReason.trim()),
		onSuccess: () => {
			void cache.invalidateQueries({ queryKey: ["quality"] });
			void cache.invalidateQueries({ queryKey: ["accounts"] });
			void cache.invalidateQueries({ queryKey: ["egress-nodes"] });
			setReviewReason("");
		},
	});

	const report = reportOf(item);
	const probes = useQuery({
		queryKey: ["quality", "probes", "case", item.id],
		queryFn: ({ signal }) => fetchQualityProbesForCase(signal, item.id),
		staleTime: 5000,
		refetchInterval: item.status === "investigating" ? 5000 : false,
	});

	const probeAccounts = useAccountDirectory((probes.data ?? []).flatMap((probe) => [probe.defendant, probe.juror, probe.control_account_id ?? 0, ...(probe.proof?.observations ?? []).map(o => Number(o.account_id))]));
	const accountsMap = useMemo(() => new Map([
		...caseAccountsMap,
		...(probeAccounts.data?.items ?? []).map((value) => [Number(value.id), { name: value.name, email: value.email }] as const),
	]), [caseAccountsMap, probeAccounts.data]);

	const account = item.parties.find((p) => p.kind === "account");
	const exit = item.parties.find((p) => p.kind === "exit");

	// Extract unique control accounts and exits
	const controlAccountsList = useMemo(() => {
		if (!probes.data) return [];
		const accMap = new Map<number, string>();
		for (const p of probes.data) {
			const cId = p.control_account_id ?? (p.direction === "exit" || p.direction === "exit_jury" ? p.juror : undefined);
			if (cId && !accMap.has(cId)) {
				const info = accountsMap.get(cId);
				accMap.set(cId, info?.name || info?.email || `#${cId}`);
			}
		}
		return Array.from(accMap.entries()).map(([id, name]) => ({ id, name }));
	}, [probes.data, accountsMap]);

	const controlExitsList = useMemo(() => {
		if (!probes.data) return [];
		const exitMap = new Map<number, string>();
		for (const p of probes.data) {
			const nId = p.control_node_id ?? p.baseline_node_id ?? (p.direction === "account" || p.direction === "account_differential" ? p.node_id : undefined);
			if (nId && !exitMap.has(nId)) {
				const info = nodesMap.get(nId);
				exitMap.set(nId, info?.name || `节点 #${nId}`);
			}
		}
		return Array.from(exitMap.entries()).map(([id, name]) => ({ id, name }));
	}, [probes.data, nodesMap]);

	const proofProtocol = report?.policy.version === "resource-proof-case-v1" || Boolean(item.proof) || (item.evidence?.policy as { version?: string } | undefined)?.version === "resource-proof-case-v1";

	const tone = verdictTone(item);

	return (
		<>
			{/* Dialog Header - 100% Matching Probe Report Header */}
			<DialogHeader>
				<div className="flex flex-wrap items-center justify-between gap-2 pr-6">
					<DialogTitle className="flex min-w-0 items-start gap-2 text-base font-bold">
						<Gavel className="size-5 shrink-0 text-primary" />
						<span>{t("experiment.case", { id: item.id })} · {isZh ? "调查报告" : "Investigation report"}</span>
					</DialogTitle>
					{!proofProtocol && <StatusPill tone={tone}>
						{t(`ops.${verdictKey(item)}`)}
					</StatusPill>}
				</div>
				<p className="text-xs text-muted-foreground">
					{isZh ? "立案时间：" : "Opened: "}{new Date(item.opened_at).toLocaleString(i18n.language)}{item.closed_at && <> · {isZh ? "结案时间：" : "Closed: "}{new Date(item.closed_at).toLocaleString(i18n.language)}</>}
				</p>
			</DialogHeader>

			<div className="min-w-0 space-y-4 py-2 text-xs">
				{proofProtocol && <CaseProofEvidence item={item} tasks={probes.data ?? []} loading={probes.isPending} error={probes.isError} retry={() => void probes.refetch()} accounts={accountsMap} nodes={nodesMap} />}
				{!proofProtocol && <>

				{/* Section 1: Methodology & Accused Parties (100% Matching Section 1 in Probe Report) */}
				<div className="rounded-lg border border-border/80 bg-muted/20 p-3">
					<h4 className="font-semibold text-foreground mb-1.5 flex items-center gap-1.5">
						<Scale className="size-4 text-indigo-500" />
						<span>{isZh ? "实验设计与涉案当事双方" : "Methodology & Accused Parties"}</span>
					</h4>
					<p className="text-[11px] text-muted-foreground leading-relaxed">
						{isZh
							? "固定涉案出口换正常账号（陪审组），固定嫌疑账号换纯净出口（差分组）。通过两组交替对照测试隔离单点变量，查明降智归因。"
							: t("experiment.intro")}
					</p>

					{/* 2-Column Parties Grid */}
					<div className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-3 border-t border-border/50 pt-2.5">
						<div>
							<span className="text-[10px] text-muted-foreground block mb-0.5">
								{isZh ? "涉案嫌疑账号 (Defendant Account)" : t("guardProbes.defendantAccount")}
							</span>
							{account ? (
								<div className="flex min-w-0 items-center justify-between gap-2 pr-2">
									<QualityAccountReference id={account.account_id} accounts={accountsMap} />
									<Badge variant={["remanded", "sentenced"].includes(account.disposition) ? "destructive" : "secondary"} className="text-[9px] px-1 py-0">
										{t(partyDispositionKey(account.disposition))}
									</Badge>
								</div>
							) : (
								<span className="text-muted-foreground italic">未指定</span>
							)}
						</div>

						<div>
							<span className="text-[10px] text-muted-foreground block mb-0.5">
								{isZh ? "涉案异常出口 (Incident Exit)" : t("guardProbes.suspectExit")}
							</span>
							{exit ? (
								<div className="flex min-w-0 items-center justify-between gap-2 pr-2">
									<QualityExitReference node={exit.node_id} epoch={exit.epoch} nodes={nodesMap} ipByNode={ipByNode} />
									<Badge variant={exit.disposition === "remanded" || exit.disposition === "sentenced" ? "destructive" : "secondary"} className="text-[9px] px-1 py-0">
										{t(partyDispositionKey(exit.disposition))}
									</Badge>
								</div>
							) : (
								<span className="text-muted-foreground italic">直连/未知出口</span>
							)}
						</div>
					</div>

					{/* Control Benchmarks List */}
					<div className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-3 border-t border-border/50 pt-2.5">
						<div>
							<span className="text-[10px] text-muted-foreground block mb-1">
								{isZh ? "匹配对照账号 (Control Accounts)" : "Control Benchmark Accounts"}
							</span>
							{controlAccountsList.length > 0 ? (
								<div className="flex flex-wrap gap-1">
									{controlAccountsList.map((acc) => (
										<span key={acc.id} className="inline-flex items-center gap-1 rounded bg-card/80 border border-border/60 px-1.5 py-0.5 text-[10px] font-mono text-foreground font-semibold">
											<User className="size-2.5 text-primary" />
											<span>{acc.name}</span>
										</span>
									))}
								</div>
							) : (
								<span className="text-[10px] text-muted-foreground italic">{isZh ? "账号池随机对照正常账号" : "Clean accounts"}</span>
							)}
						</div>

						<div>
							<span className="text-[10px] text-muted-foreground block mb-1">
								{isZh ? "匹配对照出口 (Control Exits)" : "Matched Control Exits"}
							</span>
							{controlExitsList.length > 0 ? (
								<div className="flex flex-wrap gap-1">
									{controlExitsList.map((ex) => (
										<span key={ex.id} className="inline-flex items-center gap-1 rounded bg-card/80 border border-border/60 px-1.5 py-0.5 text-[10px] font-mono text-foreground font-semibold">
											<Server className="size-2.5 text-emerald-500" />
											<span>{ex.name}</span>
										</span>
									))}
								</div>
							) : (
								<span className="text-[10px] text-muted-foreground italic">{isZh ? "已知纯净候选出口" : "Clean exits"}</span>
							)}
						</div>
					</div>
				</div>

				{/* Section 2: Findings & Evidentiary Support (100% Matching Section 2 in Probe Report) */}
				<div className="rounded-lg border border-border/80 bg-card p-3 space-y-2.5">
					<h4 className="font-semibold text-foreground mb-1.5 flex items-center gap-1.5">
						<AlertCircle className="size-4 text-primary" />
						<span>{isZh ? "审讯裁决推论与事实支撑 (Attribution Findings)" : t("guardProbes.evidenceTitle")}</span>
					</h4>

					{/* Highlight Finding Box matching Probe Report Finding Box */}
					<div
						className={cn(
							"rounded-lg border p-3 leading-relaxed",
							tone === "neutral"
								? "border-emerald-500/30 bg-emerald-500/5 text-emerald-950 dark:text-emerald-200"
								: tone === "bad"
									? "border-rose-500/30 bg-rose-500/5 text-rose-950 dark:text-rose-200"
									: "border-amber-500/30 bg-amber-500/5 text-amber-950 dark:text-amber-200"
						)}
					>
						<p className="font-bold text-xs mb-1">
							{t(`ops.${verdictKey(item)}`)} · {t(`ops.${caseDispositionKey(item)}`)}
						</p>
						{report?.reason && (
							<p className="text-[11px] opacity-90 mt-0.5">{t(`experiment.facts.${report.reason}`, { defaultValue: report.reason })}</p>
						)}
					</div>

					{/* Specific Support Facts */}
					{report && (
						<div className="grid grid-cols-1 sm:grid-cols-2 gap-3 pt-1">
							<div className="rounded-md border border-border/60 bg-muted/20 p-2.5 space-y-1">
								<p className="font-semibold text-[11px] text-foreground flex items-center gap-1">
									<Server className="size-3 text-rose-500" />
									<span>{t("experiment.exitSupport")}</span>
								</p>
								{report.exit_support && report.exit_support.length > 0 ? (
									<ul className="space-y-0.5 text-[10px]">
										{report.exit_support.map((fact, idx) => (
											<li key={idx} className="flex items-start gap-1 text-rose-600 dark:text-rose-400">
												<span className="size-1 rounded-full bg-rose-500 mt-1.5 shrink-0" />
												<span>{t(`experiment.facts.${fact}`, { defaultValue: fact })}</span>
											</li>
										))}
									</ul>
								) : (
									<p className="text-[10px] text-muted-foreground/70 italic">{t("experiment.noSupport")}</p>
								)}
							</div>

							<div className="rounded-md border border-border/60 bg-muted/20 p-2.5 space-y-1">
								<p className="font-semibold text-[11px] text-foreground flex items-center gap-1">
									<User className="size-3 text-purple-500" />
									<span>{t("experiment.accountSupport")}</span>
								</p>
								{report.account_support && report.account_support.length > 0 ? (
									<ul className="space-y-0.5 text-[10px]">
										{report.account_support.map((fact, idx) => (
											<li key={idx} className="flex items-start gap-1 text-purple-600 dark:text-purple-400">
												<span className="size-1 rounded-full bg-purple-500 mt-1.5 shrink-0" />
												<span>{t(`experiment.facts.${fact}`, { defaultValue: fact })}</span>
											</li>
										))}
									</ul>
								) : (
									<p className="text-[10px] text-muted-foreground/70 italic">{t("experiment.noSupport")}</p>
								)}
							</div>
						</div>
					)}

					{/* Limitations if any */}
					{report?.limitations && report.limitations.length > 0 && (
						<div className="rounded-md border border-amber-500/30 bg-amber-500/10 p-2.5 text-[11px] text-amber-800 dark:text-amber-200">
							<p className="font-bold mb-0.5 flex items-center gap-1">
								<Info className="size-3" />
								<span>{t("experiment.limitations")}：</span>
							</p>
							<ul className="list-disc list-inside space-y-0.5 pl-1 opacity-90 text-[10px]">
								{report.limitations.map((lim, idx) => (
									<li key={idx}>{t(`experiment.facts.${lim}`, { defaultValue: lim })}</li>
								))}
							</ul>
						</div>
					)}
				</div>

				{/* Section 3: Telemetry (100% Matching Section 3 in Probe Report) */}
				{report && (
					<div className="rounded-lg border border-border/80 bg-muted/10 p-3">
						<h4 className="font-semibold text-foreground mb-1.5 flex items-center gap-1.5">
							<Timer className="size-4 text-muted-foreground" />
							<span>{isZh ? "两组对照测试度量遥测" : t("guardProbes.telemetryTitle")}</span>
						</h4>

						<div className="grid grid-cols-3 gap-2 font-mono text-[11px]">
							<div className="rounded-md border border-border/40 bg-card/60 p-2">
								<span className="text-[10px] text-muted-foreground block">{isZh ? "出口陪审组 (Jury)" : "Exit Jury"}</span>
								<span className="font-bold text-foreground">
									{report.exit.clean} 通过 / {report.exit.degraded} 降智
								</span>
							</div>

							<div className="rounded-md border border-border/40 bg-card/60 p-2">
								<span className="text-[10px] text-muted-foreground block">{isZh ? "账号差分组 (Diff)" : "Differential"}</span>
								<span className="font-bold text-foreground">
									{report.account.clean} 通过 / {report.account.degraded} 降智
								</span>
							</div>

							<div className="rounded-md border border-border/40 bg-card/60 p-2">
								<span className="text-[10px] text-muted-foreground block">{isZh ? "关联探针任务" : "Total Probes"}</span>
								<span className="font-bold text-foreground">
									{probes.data ? `${probes.data.length} 条测试` : "计算中"}
								</span>
							</div>
						</div>
					</div>
				)}

				{/* Section 4: Probe Tasks Log */}
				<p className="text-xs text-muted-foreground">{t("guardProbes.replacementHelp")}</p>
				{probeAccounts.isError && <LoadFailed retry={() => { void probeAccounts.refetch(); }} />}
				<div className="rounded-lg border border-border/80 bg-card p-3 space-y-2">
					<div className="flex items-center justify-between">
						<h4 className="font-semibold text-foreground flex items-center gap-1.5">
							<Activity className="size-4 text-primary" />
							<span>{isZh ? "关联取证探针记录流水" : "Case Probes Log"}</span>
						</h4>
						<span className="text-[11px] font-mono text-muted-foreground">
							{probes.data ? `${probes.data.length} 条探针` : "加载中..."}
						</span>
					</div>

					{probes.isLoading ? (
						<div className="flex justify-center py-4"><Spinner className="size-4" /></div>
					) : !probes.data || probes.data.length === 0 ? (
						<p className="text-[11px] text-muted-foreground py-3 text-center">{isZh ? "暂无关联探针流水记录" : "No probes recorded"}</p>
					) : (
						<div className="max-h-48 overflow-y-auto rounded-md border border-border/60 bg-muted/10 divide-y divide-border/50">
							{probes.data.map((p) => {
								const isExitJury = p.direction === "exit" || p.direction === "exit_jury";
								const finding = getProbeFinding(p, t);
								const targetAccountID = isExitJury ? p.juror : p.defendant;
								const controlOutcome = p.control_outcome === "clean" ? "controlClean" : p.control_outcome === "degraded" ? "controlDegraded" : p.control_outcome ? "controlFailed" : "controlNotRun";

								return (
									<div key={p.id} className="flex flex-col sm:flex-row sm:items-center justify-between gap-1.5 p-2 text-xs">
										<div className="flex flex-wrap items-center gap-2 min-w-0">
											<span className="font-mono text-[11px] font-bold text-foreground">#{p.id}</span>
											<Badge variant="outline" className={cn(
												"text-[9px] px-1 py-0 font-mono",
												isExitJury ? "text-primary border-primary/30 bg-primary/5" : "text-purple-600 border-purple-500/30 bg-purple-500/5"
											)}>
												{isExitJury ? (isZh ? "出口陪审" : "Jury") : (isZh ? "账号差分" : "Diff")}
											</Badge>

											<div className="flex flex-wrap min-w-0 items-center gap-1 text-[10px] text-muted-foreground">
												<QualityAccountReference id={targetAccountID} accounts={accountsMap} className="max-w-[160px]" />
												<ArrowRight className="size-2.5 shrink-0" />
												<QualityExitReference node={p.node_id} epoch={p.epoch} nodes={nodesMap} ipByNode={ipByNode} className="max-w-[120px]" />
												{p.control_account_id ? (
													<span className="text-muted-foreground">
														{t("guardProbes.controlOutcome", { outcome: t(`guardProbes.${controlOutcome}`) })}
														<QualityAccountReference id={p.control_account_id} accounts={accountsMap} className="max-w-[150px]" />
													</span>
												) : null}
											</div>
										</div>

										<div className="flex flex-wrap items-center gap-2">
											<span className="text-[10px] text-muted-foreground truncate max-w-[150px]" title={finding.text}>
												{finding.text}
											</span>

											<Badge
												variant={p.result === "clean" ? "default" : p.result === "degraded" ? "destructive" : "secondary"}
												className="text-[9px] px-1 py-0 font-mono shrink-0"
											>
												{finding.badge}
											</Badge>
										</div>
									</div>
								);
							})}
						</div>
					)}
				</div>

				</>}

				{/* Section 5: Manual Review / Release Action */}
				<div className="rounded-lg border border-border/80 bg-muted/20 p-3 space-y-2">
					<h4 className="font-semibold text-foreground flex items-center gap-1.5">
						<Unlock className="size-4 text-primary" />
						<span>{t("experiment.manualReview")}</span>
					</h4>
					<p className="text-[11px] text-muted-foreground leading-relaxed">
						{t("experiment.manualHelp")}
					</p>

					{manual ? (
						<div className="rounded-lg border border-emerald-500/30 bg-emerald-500/10 p-2.5 text-emerald-700 dark:text-emerald-300">
							<p className="font-bold text-xs">{t("experiment.manualReleased")}</p>
							<p className="text-[11px] mt-0.5">{manual.reason}</p>
							{manual.at && <p className="text-[10px] opacity-75 mt-0.5 font-mono">{new Date(manual.at).toLocaleString()}</p>}
						</div>
					) : (
						<div className="flex flex-col gap-2 sm:flex-row">
							<Input
								value={reviewReason}
								onChange={(e) => setReviewReason(e.target.value)}
								placeholder={t("experiment.manualReason")}
								className="h-8 text-xs bg-background"
							/>
							<Button
								size="sm"
								className="shrink-0 h-8 gap-1.5 font-medium"
								disabled={reviewReason.trim().length < 3 || release.isPending}
								onClick={() => release.mutate()}
							>
								{release.isPending && <Spinner className="size-3.5" />}
								<span>{t("experiment.release")}</span>
							</Button>
						</div>
					)}
				</div>
			</div>
		</>
	);
}
