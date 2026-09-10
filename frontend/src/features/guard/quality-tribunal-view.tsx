import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
	ArrowRight,
	Check,
	CheckCircle2,
	Clock3,
	RefreshCw,
	Search,
} from "lucide-react";
import { memo, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams, useNavigate } from "react-router-dom";
import { OperationsButton as Button } from "@/features/operations/operations-ui";
import {
	Dialog,
	DialogDescription,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Pagination } from "@/shared/components/pagination";
import {
	OperationsDialogContent as DialogContent,
	OperationsError,
	OperationsHelp,
	StatusPill,
} from "@/features/operations/operations-ui";
import {
	useOperationsAccounts,
	useOperationsCases,
	useOperationsNodes,
} from "@/features/operations/operations-queries";
import {
	QualityAccountReference,
	QualityExitReference,
} from "./quality-identity";
import {
	buildQualityExitIPIndex,
	qualityAccountDisplay,
	qualityExitDisplay,
	type QualityAccountIdentity,
	type QualityNodeIdentity,
	type QualityExitIPIndex,
} from "./quality-view";
import {
	fetchQualityNodes,
	fetchQualityProbesForCase,
	triggerQualityReview,
	releaseQualityCase,
	type QualityCase,
	type QualityProbeTask,
	type ExperimentReport,
	type ExperimentGroup,
} from "./quality-api";
import {
	caseDispositionKey,
	caseEarlyRelease,
} from "./quality-case-presentation";
import { useNow } from "./quality-hooks";

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
		: item.verdict === "account_guilty"
			? "accountFinding"
			: item.verdict === "exit_guilty"
				? "exitFinding"
				: "inconclusive";
}
function verdictTone(item: QualityCase) {
	return item.status === "investigating"
		? "warn"
		: item.verdict === "account_guilty" || item.verdict === "exit_guilty"
			? "bad"
			: "neutral";
}

export const QualityTribunalView = memo(function QualityTribunalView() {
	const { t } = useTranslation();
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
	const cases = useOperationsCases(),
		accounts = useOperationsAccounts(),
		nodes = useOperationsNodes();
	const ips = useQuery({
		queryKey: ["quality", "nodes"],
		queryFn: ({ signal }) => fetchQualityNodes(signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
	const identities = useMemo<Identities>(
		() => ({
			accounts: new Map(
				(accounts.data?.items ?? []).map((a) => [
					Number(a.id),
					{ name: a.name, email: a.email },
				]),
			),
			nodes: new Map(
				(nodes.data?.items ?? []).map((n) => [Number(n.id), { name: n.name }]),
			),
			ipByNode: buildQualityExitIPIndex(ips.data ?? []),
		}),
		[accounts.data, nodes.data, ips.data],
	);
	const review = useMutation({
		mutationFn: triggerQualityReview,
		onSuccess: () => {
			void cache.invalidateQueries({ queryKey: ["quality"] });
			void cache.invalidateQueries({ queryKey: ["egress-nodes"] });
		},
	});
	const rows = useMemo(() => {
		const needle = search.trim().toLowerCase();
		return (cases.data ?? [])
			.filter((item) => {
				if (filter === "open" && item.status !== "investigating") return false;
				if (filter === "closed" && item.status === "investigating") return false;
				if (!needle) return true;
				return [
					String(item.id),
					...item.parties.map((p) =>
						p.kind === "account"
							? qualityAccountDisplay(
									p.account_id,
									identities.accounts.get(p.account_id),
								).title
							: qualityExitDisplay(
									p.node_id,
									p.epoch,
									identities.nodes.get(p.node_id),
									identities.ipByNode,
								).title,
					),
				]
					.join(" ")
					.toLowerCase()
					.includes(needle);
			})
			.sort(
				(a, b) =>
					Number(b.status === "investigating") -
						Number(a.status === "investigating") || b.id - a.id,
			);
	}, [cases.data, filter, search, identities]);
	const pageIndex = Math.min(page, Math.max(1, Math.ceil(rows.length / 8)));
	const chosen = cases.data?.find((c) => c.id === selectedId);
	const close = () => {
		setSelected(selectedId);
		setInspecting(false);
		if (params.has("case")) {
			const next = new URLSearchParams(params);
			next.delete("case");
			navigate(
				{ pathname: "/guard", search: next.toString(), hash: "tribunal" },
				{ replace: true },
			);
		}
	};
	const caseCards = useMemo(() => rows.slice((pageIndex - 1) * 8, pageIndex * 8).map((item) => {
			const report = reportOf(item),
				account = item.parties.find((p) => p.kind === "account"),
				exit = item.parties.find((p) => p.kind === "exit");
			return (
				<button
					type="button"
					key={item.id}
					className="ops-case-row"
					data-open={item.status === "investigating"}
					onClick={() => { setSelected(item.id); setInspecting(true); }}
					aria-label={`${t("ops.inspectCase")} #${item.id}`}
				>
					<div className="min-w-0">
						<div className="ops-case-meta">
							<span className="font-medium">#{item.id}</span>
							<span>
								{new Date(item.opened_at).toLocaleString([], {
									month: "2-digit",
									day: "2-digit",
									hour: "2-digit",
									minute: "2-digit",
								})}
							</span>
						</div>
						{account && (
							<span
								className="block truncate text-[13px] font-medium"
								title={
									qualityAccountDisplay(
										account.account_id,
										identities.accounts.get(account.account_id),
									).title
								}
							>
								{
									qualityAccountDisplay(
										account.account_id,
										identities.accounts.get(account.account_id),
									).primary
								}
							</span>
						)}
						{exit && (
							<div className="mt-2 flex items-center gap-1.5 text-xs text-muted-foreground">
								<ArrowRight className="size-3 shrink-0" />
								<span className="truncate">
									{
										qualityExitDisplay(
											exit.node_id,
											exit.epoch,
											identities.nodes.get(exit.node_id),
											identities.ipByNode,
										).primary
									}
								</span>
							</div>
						)}
					</div>
					<div className="ops-case-evidence min-w-0">
						{report ? (
							(
								[
									["groupExit", report.exit],
									["groupAccount", report.account],
								] as const
							).map(([key, group]) => (
								<div className="ops-case-progress" key={key}>
									<span className="shrink-0">{t(`ops.${key}`)}</span>
									<span className="tabular-nums">
										{group.clean > 0 && (
											<span className="ops-text-good">
												{group.clean} {t("experiment.count.clean")}{" "}
											</span>
										)}
										{group.degraded > 0 && (
											<span className="ops-text-bad">
												{group.degraded}{" "}
												{t("experiment.count.degraded")}{" "}
											</span>
										)}
										{group.transport > 0 && (
											<span className="ops-text-warn">
												{group.transport}{" "}
												{t("experiment.count.transport")}{" "}
											</span>
										)}
										{group.pending > 0 && (
											<span>
												{group.pending}{" "}
												{t("experiment.count.pending")}{" "}
											</span>
										)}
										{group.unavailable + group.cancelled > 0 && (
											<span>
												{group.unavailable + group.cancelled}{" "}
												{t("experiment.count.unavailable")}
											</span>
										)}
										{group.attempts === 0 && "—"}
									</span>
								</div>
							))
						) : (
							<span className="text-xs text-muted-foreground">
								{t("ops.legacy")}
							</span>
						)}
					</div>
					<div className="min-w-0 text-right">
						<StatusPill tone={verdictTone(item)}>
							{t(`ops.${verdictKey(item)}`)}
						</StatusPill>
						<p className="ops-row-secondary max-w-44">
							{t(`ops.${caseDispositionKey(item)}`)}
						</p>
					</div>
				</button>
			);
		}), [rows, pageIndex, identities, t]);

	return (
		<div className="min-w-0">
			{review.isError && <OperationsError message={String(review.error)} />}
			{(accounts.isError || nodes.isError || ips.isError) && (
				<OperationsError message={t("experiment.identitiesUnavailable")} />
			)}
			<section className="ops-panel">
				<div className="ops-toolbar">
					<div className="ops-segment">
						{["all", "open", "closed"].map((value) => (
							<button
								type="button"
								key={value}
								aria-pressed={filter === value}
								onClick={() => {
									setFilter(value);
									setPage(1);
								}}
							>
								{t(`experiment.filter.${value}`)}
							</button>
						))}
					</div>
					<Button
						size="icon"
						variant="ghost"
						className="size-7"
						aria-label={t("experiment.review")}
						title={t("experiment.review")}
						disabled={review.isPending}
						onClick={() => review.mutate()}
					>
						{review.isPending ? (
							<Spinner />
						) : (
							<RefreshCw className="size-3.5" />
						)}
					</Button>
					<div className="relative w-full">
						<Search className="absolute left-2.5 top-2.5 size-3.5 text-muted-foreground" />
						<Input
							className="h-9 pl-8 text-xs"
							value={search}
							onChange={(e) => {
								setSearch(e.target.value);
								setPage(1);
							}}
							placeholder={t("experiment.search")}
							aria-label={t("experiment.search")}
						/>
					</div>
				</div>
				{cases.isError ? (
					<div className="p-4">
						<OperationsError
							message={String(cases.error)}
							retry={() => void cases.refetch()}
						/>
					</div>
				) : cases.isPending ? (
					<div className="ops-empty">
						<Spinner />
					</div>
				) : !rows.length ? (
					<p className="ops-empty">{t("experiment.empty")}</p>
				) : (
					<div>
						{caseCards}
					</div>
				)}
				<div className="px-4 py-2">
					<Pagination
						page={pageIndex}
						pageSize={8}
						total={rows.length}
						onPageChange={setPage}
					/>
				</div>
			</section>
			{dialogOpen && selectedId && !chosen && !cases.isPending && (
				<OperationsError message={t("ops.caseUnavailable")} />
			)}
			{chosen && (
				<CaseExperiment
					key={chosen.id}
					item={chosen}
					open={dialogOpen}
					identities={identities}
					onClose={close}
				/>
			)}
		</div>
	);
});

function CaseExperiment({
	item,
	open,
	identities,
	onClose,
}: {
	item: QualityCase;
	open: boolean;
	identities: Identities;
	onClose: () => void;
}) {
	return (
		<Dialog open={open} onOpenChange={(next) => { if (!next) onClose(); }}>
			<DialogContent className="ops-case-dialog">
				<CaseExperimentContent item={item} identities={identities} />
			</DialogContent>
		</Dialog>
	);
}

function CaseExperimentContent({ item, identities }: { item: QualityCase; identities: Identities }) {
	const { t } = useTranslation();
	const cache = useQueryClient();
	const [reviewReason, setReviewReason] = useState("");
	const manual = item.evidence?.manual_review as
		| { reason?: string; at?: string }
		| undefined;
	const release = useMutation({
		mutationFn: () => releaseQualityCase(item.id, reviewReason.trim()),
		onSuccess: () => {
			void cache.invalidateQueries({ queryKey: ["quality"] });
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
	return (
		<>
			<DialogHeader className="border-b px-5 py-4 pr-12 text-left">
				<div className="flex flex-wrap items-center gap-3">
					<DialogTitle className="text-base">
						{t("experiment.case", { id: item.id })}
					</DialogTitle>
					<StatusPill tone={verdictTone(item)}>
						{t(`ops.${verdictKey(item)}`)}
					</StatusPill>
				</div>
				<DialogDescription className="flex flex-wrap items-center gap-3 text-xs">
					<span>{new Date(item.opened_at).toLocaleString()}</span>
					{item.status === "investigating" && report ? (
						<CaseCountdown deadline={report.policy.deadline_at} />
					) : item.closed_at ? (
						<span>
							{t("ops.caseTime", {
								seconds: Math.round(
									(Date.parse(item.closed_at) - Date.parse(item.opened_at)) /
										1000,
								),
							})}
						</span>
					) : null}
				</DialogDescription>
			</DialogHeader>
			<div className="ops-inspector">
				<div className="ops-inspector-main">
					<div className="mb-5 grid gap-3 sm:grid-cols-2">
						{item.parties.map((p, i) => (
							<div
								key={i}
								className="flex min-w-0 items-center justify-between gap-3 rounded-lg bg-card p-3"
							>
								<div className="min-w-0">
									<p className="mb-1.5 text-[11px] text-muted-foreground">
										{t(`ops.${p.kind}`)}
									</p>
									{p.kind === "account" ? (
										<QualityAccountReference
											id={p.account_id}
											accounts={identities.accounts}
											className="text-sm"
										/>
									) : (
										<QualityExitReference
											node={p.node_id}
											epoch={p.epoch}
											nodes={identities.nodes}
											ipByNode={identities.ipByNode}
											className="text-sm"
										/>
									)}
								</div>
								<StatusPill
									tone={
										p.disposition === "released"
											? "good"
											: p.disposition === "sentenced"
												? "bad"
												: p.disposition === "remanded"
													? "warn"
													: "neutral"
									}
								>
									{t(
										`quality.court.disposition.${p.disposition}`,
										p.disposition,
									)}
								</StatusPill>
							</div>
						))}
					</div>
					{probes.isError ? (
						<OperationsError
							message={String(probes.error)}
							retry={() => void probes.refetch()}
						/>
					) : probes.isPending ? (
						<div className="ops-empty">
							<Spinner />
						</div>
					) : (
						<div className="ops-experiment-grid">
							{(["exit_jury", "account_differential"] as const).map(
								(direction) => {
									const kind = direction === "exit_jury" ? "exit" : "account",
										group = report?.[kind],
										early = caseEarlyRelease(item, kind);
									return (
										<section className="ops-experiment" key={direction}>
											<header className="ops-experiment-header">
												<div className="flex items-center justify-between gap-2">
													<h3>
														{t(
															direction === "exit_jury"
																? "ops.groupExit"
																: "ops.groupAccount",
														)}
													</h3>
													<OperationsHelp>
														{t(`experiment.question.${direction}`)}
													</OperationsHelp>
												</div>
												<p>{t(`experiment.group.${direction}`)}</p>
												{group && <GroupSummary group={group} />}
											</header>
											{early && (
												<div className="ops-clearance">
													<CheckCircle2 className="mt-0.5 size-3.5 shrink-0" />
													<div>
														{t(
															kind === "exit"
																? "ops.earlyReleaseExit"
																: "ops.earlyReleaseAccount",
														)}
														{early.at && (
															<p className="mt-1 text-[11px]">
																{t("ops.clearedAt", {
																	time: new Date(
																		early.at,
																	).toLocaleTimeString(),
																})}
															</p>
														)}
													</div>
												</div>
											)}
											{probes.data
												?.filter((task) => task.direction === direction)
												.map((task) => (
													<Measurement
														key={task.id}
														task={task}
														identities={identities}
													/>
												))}
											{!probes.data?.some(
												(task) => task.direction === direction,
											) && (
												<p className="ops-empty">
													{t("experiment.noCandidates")}
												</p>
											)}
										</section>
									);
								},
							)}
						</div>
					)}
				</div>
				<div className="ops-inspector-rail">
					<section className="ops-inspector-section">
						<h3 className="mb-3">{t("ops.decisionBasis")}</h3>
						{report ? (
							<>
								<p className="text-sm font-medium leading-6">
									{t(`experiment.reason.${report.reason}`, {
										defaultValue: report.reason,
									})}
								</p>
								<p className="mt-2 text-xs leading-6 text-muted-foreground">
									{t(`experiment.explanation.${report.reason}`, {
										defaultValue: t("experiment.explanation.default"),
									})}
								</p>
							</>
						) : (
							<p className="text-xs leading-6 text-muted-foreground">
								{manual
									? t("experiment.manualReleased")
									: t("experiment.legacy")}
							</p>
						)}
					</section>
					{report && (
						<>
							{report.policy.experiment?.version && (
								<section className="ops-inspector-section">
									<p className="text-xs font-medium">{t("experiment.scope", {
										model: report.policy.experiment.baseline.model || "—",
										effort: report.policy.experiment.baseline.profile.reasoning_effort || "—",
									})}</p>
									<p className="mt-2 text-xs leading-6 text-muted-foreground">{t("experiment.scopeHelp")}</p>
								</section>
							)}
							<Reasons
								title={t("experiment.accountSupport")}
								reasons={report.account_support}
							/>
							<Reasons
								title={t("experiment.exitSupport")}
								reasons={report.exit_support}
							/>
							{report.limitations.length > 0 && (
								<Reasons
									title={t("experiment.limitations")}
									reasons={report.limitations}
								/>
							)}
						</>
					)}
					<section className="ops-inspector-section">
						<h3>{t("ops.caseResources")}</h3>
						<p className="mt-2 text-xs font-medium leading-6">
							{t(`ops.${caseDispositionKey(item)}`)}
						</p>
						<p className="mt-2 text-xs leading-5 text-muted-foreground">
							{t("ops.independentReleaseHelp")}
						</p>
						{item.closed_at && (
							<p className="mt-3 text-xs text-muted-foreground">
								{t(
									`experiment.closure.${String(item.evidence?.closure_reason ?? "evidence_complete")}`,
									{ defaultValue: "" },
								)}
							</p>
						)}
					</section>
					{report && (
						<details className="ops-disclosure ops-inspector-section">
							<summary>{t("ops.caseRule")}</summary>
							<dl>
								{[
									["accountThreshold", report.policy.account_paths],
									["exitThreshold", report.policy.jury_size],
									["opsTransport", report.policy.transport_paths],
								].map(([key, value]) => (
									<div className="ops-config-row" key={key}>
										<dt>
											{key === "opsTransport"
												? t("experiment.count.transport")
												: t(`ops.${key}`)}
										</dt>
										<dd>{value}</dd>
									</div>
								))}
							</dl>
							<p className="mt-3 text-[11px] text-muted-foreground">
								{t("experiment.thresholds", {
									account: report.policy.account_paths,
									transport: report.policy.transport_paths,
									jury: report.policy.jury_size,
									degraded: report.policy.jury_degraded,
								})}
							</p>
							<p className="mt-2 text-[11px] text-muted-foreground">
								{report.policy.version}
							</p>
						</details>
					)}
					{manual ? (
						<section className="ops-inspector-section">
							<h3>{t("experiment.manualReleased")}</h3>
							<p className="mt-2 text-xs leading-6">{manual.reason}</p>
							<p className="mt-2 text-[11px] text-muted-foreground">
								{manual.at ? new Date(manual.at).toLocaleString() : ""}
							</p>
						</section>
					) : (
						["investigating", "account_guilty", "exit_guilty"].includes(
							item.status,
						) && (
							<details className="ops-disclosure">
								<summary>{t("experiment.manualReview")}</summary>
								<p className="mb-3 text-xs text-muted-foreground">
									{t("experiment.manualHelp")}
								</p>
								<Input
									value={reviewReason}
									onChange={(e) => setReviewReason(e.target.value)}
									maxLength={160}
									placeholder={t("experiment.manualReason")}
									aria-label={t("experiment.manualReason")}
								/>
								<Button
									className="mt-3 w-full"
									variant="outline"
									size="sm"
									disabled={
										release.isPending || reviewReason.trim().length < 3
									}
									onClick={() => release.mutate()}
								>
									{release.isPending && <Spinner />}
									{t("experiment.release")}
								</Button>
								{release.isError && (
									<OperationsError message={String(release.error)} />
								)}
							</details>
						)
					)}
					{!report && <LegacyEvidence evidence={item.evidence} />}
				</div>
			</div>
		</>
	);
}
function CaseCountdown({ deadline }: { deadline: string }) {
	const { t } = useTranslation();
	const now = useNow(1000);
	const remaining = Math.max(0, Math.ceil((Date.parse(deadline) - now) / 1000));
	return (
		<span className="inline-flex items-center gap-1.5">
			<Clock3 className="size-3" />
			{remaining
				? t("experiment.remaining", { minutes: Math.floor(remaining / 60), seconds: remaining % 60 })
				: t("experiment.deadlineReached")}
		</span>
	);
}

function LegacyEvidence({ evidence }: { evidence: QualityCase["evidence"] }) {
	const { t } = useTranslation();
	const [expanded, setExpanded] = useState(false);
	const formatted = useMemo(() => expanded ? JSON.stringify(evidence, null, 2) : "", [evidence, expanded]);
	return (
		<details className="ops-disclosure mt-4" onToggle={(event) => setExpanded(event.currentTarget.open)}>
			<summary>{t("experiment.legacyData")}</summary>
			{expanded && <pre className="whitespace-pre-wrap break-all text-[11px]">{formatted}</pre>}
		</details>
	);
}

function Reasons({ title, reasons }: { title: string; reasons: string[] }) {
	const { t } = useTranslation();
	return (
		<section className="ops-inspector-section">
			<h3>{title}</h3>
			<ul className="mt-2 space-y-2 text-xs leading-5 text-muted-foreground">
				{reasons.length ? (
					reasons.map((reason) => (
						<li key={reason} className="flex gap-2">
							<span className="mt-2 size-1 shrink-0 rounded-full bg-muted-foreground" />
							<span>
								{t(`experiment.signals.${reason}`, { defaultValue: reason })}
							</span>
						</li>
					))
				) : (
					<li>{t("experiment.noSupport")}</li>
				)}
			</ul>
		</section>
	);
}
function GroupSummary({ group }: { group: ExperimentGroup }) {
	const { t } = useTranslation();
	return (
		<div className="mt-3 flex flex-wrap gap-x-3 gap-y-1 text-[11px]">
			{(
				[
					"clean",
					"degraded",
					"transport",
					"unavailable",
					"pending",
					"cancelled",
				] as const
			)
				.filter((k) => group[k] > 0)
				.map((k) => (
					<span
						key={k}
						className={
							k === "clean"
								? "ops-text-good"
								: k === "degraded"
									? "ops-text-bad"
									: k === "pending"
										? "ops-text-warn"
										: "text-muted-foreground"
						}
					>
						{t(`experiment.count.${k}`)}{" "}
						<strong className="font-medium tabular-nums">{group[k]}</strong>
					</span>
				))}
		</div>
	);
}
function Measurement({
	task,
	identities,
}: {
	task: QualityProbeTask;
	identities: Identities;
}) {
	const { t } = useTranslation();
	const status = ["pending", "running", "cancelled"].includes(task.state)
		? task.state
		: task.result === "error"
			? task.failure_kind || "unavailable"
			: task.result;
	const subject = task.direction === "exit_jury" ? task.juror : task.defendant;
	return (
		<article className="ops-measurement">
			<div className="flex items-center justify-between gap-2">
				<span className="text-[11px] text-muted-foreground">
					{new Date(task.finished_at ?? task.created_at).toLocaleTimeString(
						[],
						{ hour: "2-digit", minute: "2-digit", second: "2-digit" },
					)}
				</span>
				<StatusPill
					tone={
						status === "clean"
							? "good"
							: status === "degraded"
								? "bad"
								: task.state === "running" || task.result === "error"
									? "warn"
									: "neutral"
					}
				>
					{t(`experiment.result.${status}`, { defaultValue: status })}
				</StatusPill>
			</div>
			<div className="ops-measurement-path">
				<QualityAccountReference
					id={subject}
					accounts={identities.accounts}
					className="flex-1"
				/>
				<ArrowRight className="size-3 shrink-0 text-muted-foreground" />
				<QualityExitReference
					node={task.node_id}
					epoch={task.epoch}
					nodes={identities.nodes}
					ipByNode={identities.ipByNode}
					className="flex-1"
				/>
			</div>
			{task.control_outcome && (
				<div className="ops-control">
					<div className="flex items-center gap-2 text-muted-foreground">
						{task.control_outcome === "clean" && (
							<Check className="size-3 ops-text-good" />
						)}
						{t("experiment.matchedControl")} ·{" "}
						{t(`experiment.result.${task.control_outcome}`)}
					</div>
					<div className="ops-measurement-path">
						<QualityAccountReference
							id={task.control_account_id ?? 0}
							accounts={identities.accounts}
							className="flex-1"
						/>
						<ArrowRight className="size-3 shrink-0 text-muted-foreground" />
						<QualityExitReference
							node={task.control_node_id ?? 0}
							epoch={task.control_epoch ?? 0}
							nodes={identities.nodes}
							ipByNode={identities.ipByNode}
							className="flex-1"
						/>
					</div>
					{!task.control_verified && (
						<p className="ops-text-warn">{t("experiment.controlUnverified")}</p>
					)}
				</div>
			)}
			<details className="ops-disclosure text-[11px]">
				<summary>
					{t("experiment.diagnostics")}{" "}
					<span className="opacity-60">#{task.id}</span>
				</summary>
				<p className="break-words text-muted-foreground">
					{task.detail || "—"}
				</p>
				{task.control_detail && (
					<p className="mt-1 break-words text-muted-foreground">
						{task.control_detail}
					</p>
				)}
			</details>
		</article>
	);
}
export function LoadFailed({
	message,
	onRetry,
}: {
	message: string;
	onRetry: () => void;
}) {
	return <OperationsError message={message} retry={onRetry} />;
}
