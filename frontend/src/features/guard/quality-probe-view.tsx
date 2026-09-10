import { Pagination } from "@/shared/components/pagination";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Spinner } from "@/components/ui/spinner";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import { listAllAccounts } from "@/features/accounts/accounts-api";
import { listAllEgressNodes } from "@/features/settings/settings-api";
import {
	buildQualityExitIPIndex,
	parseGoDurationMs,
	probeDurationMs,
	probeSummary,
	type QualityAccountIdentity,
	type QualityExitIPIndex,
	type QualityNodeIdentity,
} from "@/features/guard/quality-view";
import { QualitySection, ToneBadge } from "@/features/guard/quality-ui";
import { useNow } from "./quality-hooks";
import {
	QualityAccountReference,
	QualityExitReference,
} from "./quality-identity";
import {
	fetchQualityNodes,
	fetchQualityProbes,
	fetchQualitySettings,
	type QualityProbeTask,
} from "./quality-api";
import { LoadFailed } from "./quality-tribunal-view";

// 调查局:取证探针队列。双向取证(账号差分/陪审员探测)是裁决证据的
// 主动来源——这里能直接看到"法院此刻在等哪些取证结果"。
// 字号规范同监控台其余分区:表格 text-xs,次级 meta text-[11px]。

const PROBE_ROWS = 10;

export function QualityProbeView() {
	const { t, i18n } = useTranslation();
	const [page, setPage] = useState(1);
	const probesQuery = useQuery({
		queryKey: ["quality", "probes"],
		queryFn: ({ signal }) => fetchQualityProbes(signal),
		refetchInterval: 15_000,
	});

	// 近期结论窗口跟随证据窗口(可调参):硬编码 30m 会在管理员调整
	// evidence_window 后静默漂移(批10 修复);设置未加载时退默认窗。
	const settingsQuery = useQuery({
		queryKey: ["quality", "settings"],
		queryFn: ({ signal }) => fetchQualitySettings(signal),
		staleTime: 60_000,
	});
	const accountsQuery = useQuery({
		queryKey: ["quality", "account-names"],
		queryFn: ({ signal }) => listAllAccounts({}, signal),
		staleTime: 5 * 60_000,
		refetchInterval: 5 * 60_000,
	});
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
	const now = useNow(30_000); // 渲染纯净度:时间源经 hook,不裸调 Date.now
	const probes = probesQuery.data ?? [];
	const currentPage = Math.min(
		page,
		Math.max(1, Math.ceil(probes.length / PROBE_ROWS)),
	);
	const summary = probeSummary(probes, now, windowMs);
	const timeFormatter = new Intl.DateTimeFormat(i18n.language, {
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
	});

	return (
		<QualitySection
			id="q-bureau"
			icon={Search}
			title={t("ops.measurementTitle")}
			help={t("ops.measurementHelp")}
		>
			{probesQuery.isError ? (
				<LoadFailed
					message={String(probesQuery.error)}
					onRetry={() => void probesQuery.refetch()}
				/>
			) : probesQuery.isLoading ? (
				<div className="flex min-h-16 items-center justify-center">
					<Spinner />
				</div>
			) : probes.length === 0 ? (
				<p className="rounded-md border border-dashed px-4 py-6 text-center text-sm text-muted-foreground">
					{t("quality.bureau.empty")}
				</p>
			) : (
				<div className="space-y-2.5">
					<div className="flex flex-wrap items-center gap-1.5">
						<SummaryChip
							label={t("quality.bureau.pending")}
							value={summary.pending}
							tone="muted"
							pulse={summary.pending > 0}
						/>
						<SummaryChip
							label={t("quality.bureau.running")}
							value={summary.running}
							tone="warning"
							pulse={summary.running > 0}
						/>
						<SummaryChip
							label={t("quality.bureau.clean")}
							value={summary.clean}
							tone="ok"
						/>
						<SummaryChip
							label={t("quality.bureau.degraded")}
							value={summary.degraded}
							tone="destructive"
						/>
						<SummaryChip
							label={t("quality.bureau.error")}
							value={summary.error}
							tone="muted"
						/>
						<SummaryChip
							label={t("quality.bureau.cancelled")}
							value={summary.cancelled}
							tone="muted"
						/>
					</div>
					<div className="overflow-hidden rounded-lg border">
						<Table className="ops-compact-table">
							<TableHeader>
								<TableRow className="hover:bg-transparent">
									<TableHead className="h-9 text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colId")}
									</TableHead>
									<TableHead className="h-9 text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colDirection")}
									</TableHead>
									<TableHead className="h-9 text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colCase")}
									</TableHead>
									<TableHead className="h-9 text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colParties")}
									</TableHead>
									<TableHead className="h-9 text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colState")}
									</TableHead>
									<TableHead className="h-9 text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colResult")}
									</TableHead>
									<TableHead className="h-9 text-right text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colDuration")}
									</TableHead>
									<TableHead className="h-9 text-right text-xs font-medium text-muted-foreground">
										{t("quality.bureau.colCreated")}
									</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{probes
									.slice(
										(currentPage - 1) * PROBE_ROWS,
										currentPage * PROBE_ROWS,
									)
									.map((task) => {
										const duration = probeDurationMs(task);
										return (
											<TableRow key={task.id} className="text-xs">
												<TableCell className="py-2 font-mono text-muted-foreground">
													#{task.id}
												</TableCell>
												<TableCell className="py-2">
													{t(
														`quality.court.probeDirection.${task.direction}`,
														task.direction,
													)}
												</TableCell>
												<TableCell className="py-2 font-mono text-muted-foreground">
													<Link
														className="underline underline-offset-4"
														to={`/guard?case=${task.case_id}#tribunal`}
													>
														#{task.case_id}
													</Link>
												</TableCell>
												<TableCell className="min-w-[280px] py-2">
													<ProbePartiesCell
														task={task}
														accounts={accountIdentity}
														nodes={nodeIdentity}
														ipByNode={ipByNode}
													/>
												</TableCell>
												<TableCell className="py-2">
													<ToneBadge
														tone={
															task.state === "failed"
																? "muted"
																: task.state === "running"
																	? "warning"
																	: "muted"
														}
													>
														{task.state === "running" ? (
															<span className="size-1.5 animate-pulse rounded-full bg-current" />
														) : null}
														{t(
															`quality.court.probeState.${task.state}`,
															task.state,
														)}
													</ToneBadge>
												</TableCell>
												<TableCell className="max-w-64 py-2">
													<div className="flex min-w-0 items-center gap-1.5">
														{task.result ? (
															<ToneBadge
																tone={
																	task.result === "clean"
																		? "ok"
																		: task.result === "degraded"
																			? "destructive"
																			: "muted"
																}
															>
																{t(
																	`quality.bureau.results.${task.result}`,
																	task.result,
																)}
															</ToneBadge>
														) : null}
														{task.detail ? (
															<span
																className="truncate font-mono text-[11px] text-muted-foreground"
																title={task.detail}
															>
																{task.detail}
															</span>
														) : null}
													</div>
												</TableCell>
												<TableCell className="py-2 text-right tabular-nums text-muted-foreground">
													{duration === null
														? "—"
														: `${Math.round(duration / 1000)}s`}
												</TableCell>
												<TableCell className="py-2 text-right text-muted-foreground">
													{timeFormatter.format(new Date(task.created_at))}
												</TableCell>
											</TableRow>
										);
									})}
							</TableBody>
						</Table>
						{probes.length > PROBE_ROWS ? (
							<Pagination
								className="border-t px-3 py-2"
								page={currentPage}
								pageSize={PROBE_ROWS}
								total={probes.length}
								onPageChange={setPage}
							/>
						) : null}
					</div>
				</div>
			)}
		</QualitySection>
	);
}

function SummaryChip({
	label,
	value,
	tone,
	pulse,
}: {
	label: string;
	value: number;
	tone: "ok" | "destructive" | "warning" | "muted";
	pulse?: boolean;
}) {
	return (
		<ToneBadge tone={tone} className="gap-1 tabular-nums">
			{pulse ? (
				<span className="size-1.5 animate-pulse rounded-full bg-current" />
			) : null}
			{label}
			<span className="font-medium">{value}</span>
		</ToneBadge>
	);
}

function ProbePartiesCell({
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
	const { t } = useTranslation();
	if (task.direction === "account_differential") {
		return (
			<div className="min-w-0 space-y-1.5">
				<div className="flex min-w-0 items-start gap-1.5">
					<span className="shrink-0 text-[10px] text-muted-foreground">
						{t("quality.bureau.defendant")}
					</span>
					<QualityAccountReference
						id={task.defendant}
						accounts={accounts}
						className="max-w-56"
					/>
				</div>
				<div className="flex min-w-0 flex-wrap items-start gap-x-1.5 gap-y-1">
					<span className="shrink-0 text-[10px] text-muted-foreground">
						{t("quality.bureau.baseline")}
					</span>
					{task.baseline_node_id && task.baseline_node_id > 0 ? (
						<QualityExitReference
							node={task.baseline_node_id}
							epoch={task.baseline_epoch ?? 0}
							nodes={nodes}
							ipByNode={ipByNode}
							className="max-w-48"
						/>
					) : (
						<span className="text-[10px] text-muted-foreground">
							{t("quality.tribunal.baselineUnknown")}
						</span>
					)}
					<span className="self-center text-muted-foreground">→</span>
					<span className="shrink-0 text-[10px] text-muted-foreground">
						{t("quality.bureau.comparison")}
					</span>
					<QualityExitReference
						node={task.node_id}
						epoch={task.epoch}
						nodes={nodes}
						ipByNode={ipByNode}
						className="max-w-48"
					/>
				</div>
			</div>
		);
	}
	return (
		<div className="flex min-w-0 flex-wrap items-start gap-x-1.5 gap-y-1">
			<span className="shrink-0 text-[10px] text-muted-foreground">
				{t("quality.bureau.juror")}
			</span>
			<QualityAccountReference
				id={task.juror}
				accounts={accounts}
				className="max-w-56"
			/>
			<span className="self-center text-muted-foreground">→</span>
			<span className="shrink-0 text-[10px] text-muted-foreground">
				{t("quality.bureau.exit")}
			</span>
			<QualityExitReference
				node={task.node_id}
				epoch={task.epoch}
				nodes={nodes}
				ipByNode={ipByNode}
				className="max-w-48"
			/>
		</div>
	);
}
