import {
	ArrowUpRight,
	Check,
	Settings2,
	UserRound,
	Waypoints,
} from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import {
	useOperationsAccounts,
	useOperationsCases,
	useOperationsGuard,
	useOperationsNodes,
} from "@/features/operations/operations-queries";
import {
	OperationsHelp,
	StatusPill,
} from "@/features/operations/operations-ui";
import { fetchQualitySettings } from "./quality-api";

export function QualityOverviewPanel() {
	const { t } = useTranslation();
	const accounts = useOperationsAccounts();
	const nodes = useOperationsNodes();
	const cases = useOperationsCases();
	const guard = useOperationsGuard();
	const policy = useQuery({
		queryKey: ["quality", "settings"],
		queryFn: ({ signal }) => fetchQualitySettings(signal),
		staleTime: 30000,
	});
	const rows = [
		...(accounts.data?.items ?? [])
			.filter((a) => a.quality && a.quality.state !== "active")
			.map((a) => ({
				id: `a${a.id}`,
				name: a.name,
				kind: "account",
				caseId: a.quality?.caseId,
				held: a.quality?.state === "remanded",
			})),
		...(nodes.data?.items ?? [])
			.filter((n) => n.quality)
			.map((n) => ({
				id: `n${n.id}`,
				name: n.name,
				kind: "exit",
				caseId: n.quality?.caseId,
				held: n.quality?.state === "remanded",
			})),
	];
	const unknown =
		accounts.isError || nodes.isError || !accounts.data || !nodes.data;
	const models = guard.data?.effective?.guardedModels ?? [];
	return (
		<div className="ops-rail">
			<section className="ops-rail-section">
				<header>
					<span>{t("ops.restrictions")}</span>
					<StatusPill tone={rows.length ? "warn" : "neutral"}>
						{unknown ? "—" : rows.length}
					</StatusPill>
				</header>
				{unknown ? (
					<p className="ops-empty">{t("ops.unknown")}</p>
				) : rows.length ? (
					<div className="max-h-80 overflow-y-auto">
						{rows.map((row) => (
							<Link
								key={row.id}
								className="ops-resource"
								to={
									row.caseId
										? `/guard?case=${row.caseId}#tribunal`
										: row.kind === "exit"
											? "/proxies"
											: "/accounts"
								}
							>
								{row.kind === "account" ? (
									<UserRound className="size-3.5 shrink-0 text-muted-foreground" />
								) : (
									<Waypoints className="size-3.5 shrink-0 text-muted-foreground" />
								)}
								<div className="min-w-0 flex-1">
									<p className="ops-resource-name" title={row.name}>
										{row.name}
									</p>
									<p className="ops-resource-detail">
										{t(`ops.${row.kind}`)} ·{" "}
										{t(
											row.held
												? "ops.condition.held"
												: row.kind === "exit"
													? "ops.condition.banned"
													: "ops.accountFinding",
										)}
									</p>
								</div>
								<ArrowUpRight className="size-3 shrink-0 text-muted-foreground" />
							</Link>
						))}
					</div>
				) : (
					<p className="ops-empty">
						<Check className="size-4" />
						{t("ops.noHeldResources")}
					</p>
				)}
			</section>
			<section className="ops-rail-section">
				<header>
					<span>{t("ops.currentPolicy")}</span>
					<Link aria-label={t("ops.configure")} to="/guard/settings">
						<Settings2 className="size-3.5 text-muted-foreground" />
					</Link>
				</header>
				<dl className="ops-config-list">
					{(
						[
							["accountThreshold", policy.data?.account_need_exits],
							["exitThreshold", policy.data?.exit_need_n],
							["investigationLimit", policy.data?.investigation_timeout],
						] as const
					).map(([key, value]) => (
						<div className="ops-config-row" key={key}>
							<dt>{t(`ops.${key}`)}</dt>
							<dd>{policy.isError ? "—" : (value ?? "—")}</dd>
						</div>
					))}
					<div className="ops-config-row">
						<dt className="flex items-center">
							{t("ops.releasePolicy")}
							<OperationsHelp>{t("ops.independentReleaseHelp")}</OperationsHelp>
						</dt>
						<dd>{t("ops.clearWhenReady")}</dd>
					</div>
				</dl>
				<div className="border-t px-3.5 py-3">
					<p className="mb-2 text-xs text-muted-foreground">
						{t("ops.guardModels")}
					</p>
					<div className="flex flex-wrap gap-1.5">
						{models.map((m) => (
							<StatusPill key={m}>{m.replace(/^grok_build:/, "")}</StatusPill>
						))}
						{!models.length && (
							<span className="text-xs text-muted-foreground">—</span>
						)}
					</div>
				</div>
			</section>
			<section className="ops-rail-section">
				<header>
					<span>{t("ops.caseDistribution")}</span>
					<OperationsHelp>
						{t("ops.caseHistoryScope", { count: cases.data?.length ?? 0 })}
					</OperationsHelp>
				</header>
				<dl className="ops-config-list">
					{[
						["accountFinding", "account_guilty"],
						["exitFinding", "exit_guilty"],
						["inconclusive", "insufficient"],
					].map(([key, value]) => (
						<div className="ops-config-row" key={key}>
							<dt>{t(`ops.${key}`)}</dt>
							<dd>
								{cases.isError || !cases.data
									? "—"
									: cases.data.filter(
											(c) =>
												c.verdict === value ||
												(value === "insufficient" && c.verdict === "dismissed"),
										).length}
							</dd>
						</div>
					))}
				</dl>
			</section>
		</div>
	);
}
