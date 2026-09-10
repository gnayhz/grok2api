import { Settings2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { OperationsButton as Button } from "@/features/operations/operations-ui";
import { Tabs, TabsContent } from "@/components/ui/tabs";
import {
	MetricRail,
	OperationalMetric,
	OperationsError,
	OperationsHeader,
	OperationsTabs,
	StatusPill,
} from "@/features/operations/operations-ui";
import {
	useOperationsAccounts,
	useOperationsCases,
	useOperationsGuard,
	useOperationsNodes,
	useOperationsSelfCheck,
} from "@/features/operations/operations-queries";
import { QualityHitStats } from "./quality-hit-stats";
import { QualityProbeView } from "./quality-probe-view";
import { QualityTribunalView } from "./quality-tribunal-view";
import { QualityOverviewPanel } from "./quality-overview";

export function QualityConsole() {
	const { t } = useTranslation();
	const location = useLocation();
	const navigate = useNavigate();
	const requested = location.hash.slice(1);
	const view = ["stats", "bureau"].includes(requested) ? requested : "tribunal";
	const guard = useOperationsGuard();
	const check = useOperationsSelfCheck();
	const cases = useOperationsCases();
	const accounts = useOperationsAccounts();
	const nodes = useOperationsNodes();
	const enabled = guard.data?.effective?.enabled;
	const checkBad = check.data?.self_check.outcome === "error";
	const status =
		guard.isError || check.isError || !check.data || enabled === undefined
			? "unknown"
			: checkBad
				? "selfCheckFailed"
				: enabled
					? "active"
					: "inactive";
	const open = cases.data?.filter((c) => c.status === "investigating").length;
	const accountCount = accounts.data?.items.filter(
		(a) => a.quality && a.quality.state !== "active",
	).length;
	const exitCount = nodes.data?.items.filter((n) => n.quality).length;
	const change = (next: string) => navigate({ pathname: "/guard", hash: next });
	return (
		<div className="ops-workspace">
			<OperationsHeader
				title={t("ops.quality")}
				description={t("ops.qualityDescription")}
				status={
					<StatusPill
						tone={
							status === "active"
								? "good"
								: status === "unknown"
									? "neutral"
									: "bad"
						}
					>
						{t(`ops.${status}`)}
					</StatusPill>
				}
				action={
					<Button variant="outline" size="sm" asChild>
						<Link to="/guard/settings">
							<Settings2 className="size-3.5" />
							{t("ops.settings")}
						</Link>
					</Button>
				}
			/>
			{(guard.isError ||
				check.isError ||
				cases.isError ||
				accounts.isError ||
				nodes.isError) && (
				<div className="mb-4">
					<OperationsError />
				</div>
			)}
			{status === "inactive" || status === "selfCheckFailed" ? (
				<div className="mb-4">
					<OperationsError
						message={t(
							status === "inactive"
								? "settings.guardStats.bannerDisabledDescription"
								: "ops.selfCheckFailed",
						)}
					/>
				</div>
			) : null}

			<Tabs activationMode="manual" value={view} onValueChange={change}>
				<OperationsTabs
					items={[
						{ value: "tribunal", label: t("ops.caseWorkspace"), count: open },
						{ value: "stats", label: t("ops.signals") },
						{ value: "bureau", label: t("ops.tests") },
					]}
					end={t("ops.freshness")}
				/>
				<TabsContent value="tribunal" className="mt-0">
					<MetricRail>
						<OperationalMetric
							label={t("ops.openCases")}
							value={cases.isError ? "—" : (open ?? "—")}
							detail={t("ops.openCasesHelp")}
							tone={open ? "warn" : undefined}
							onClick={() => change("tribunal")}
						/>
						<OperationalMetric
							label={t("ops.heldAccounts")}
							value={accounts.isError ? "—" : (accountCount ?? "—")}
							detail={t("ops.heldAccountsHelp")}
							tone={accountCount ? "warn" : undefined}
						/>
						<OperationalMetric
							label={t("ops.heldExits")}
							value={nodes.isError ? "—" : (exitCount ?? "—")}
							detail={t("ops.heldExitsHelp")}
							tone={exitCount ? "bad" : undefined}
							onClick={() => navigate("/proxies#nodes")}
						/>
						<OperationalMetric
							label={t("ops.rejected")}
							value={
								guard.isError
									? "—"
									: (guard.data?.retrial.exhaustedRejected ?? "—")
							}
							detail={t("ops.processScope")}
							onClick={() => change("stats")}
						/>
					</MetricRail>
					<div className="ops-workbench">
						<QualityTribunalView />
						<QualityOverviewPanel />
					</div>
				</TabsContent>
				<TabsContent value="stats" className="mt-0">
					<QualityHitStats />
				</TabsContent>
				<TabsContent value="bureau" className="mt-0">
					<QualityProbeView />
				</TabsContent>
			</Tabs>
		</div>
	);
}
