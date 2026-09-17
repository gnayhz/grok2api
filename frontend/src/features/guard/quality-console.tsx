import { useQueryClient } from "@tanstack/react-query";
import { RefreshCw, Settings2, ShieldCheck } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { cn } from "@/shared/lib/cn";
import { OperationsButton as Button } from "@/shared/ui/operations";
import { Tabs, TabsContent } from "@/shared/ui/tabs";
import {
	OperationsError,
	OperationsTabs,
} from "@/shared/ui/operations";

import { QualityHitStats } from "./quality-hit-stats";
import { QualityProbeView } from "./quality-probe-view";
import { QualityTribunalView } from "./quality-tribunal-view";
import { useRestrictedAccountCount } from "@/entities/account/account-queries";
import { useEgressNodes } from "@/entities/egress/egress-queries";
import { useGuardStats, useQualityCases, useQualityGuardSelfCheck } from "@/entities/guard/guard-queries";

export function QualityConsole() {
	const { t } = useTranslation();
	const cache = useQueryClient();
	const location = useLocation();
	const navigate = useNavigate();
	const requested = location.hash.slice(1);
	const view = ["stats", "bureau"].includes(requested) ? requested : "tribunal";
	const guard = useGuardStats();
	const check = useQualityGuardSelfCheck();
	const cases = useQualityCases();
	const accounts = useRestrictedAccountCount();
	const nodes = useEgressNodes();
	const enabled = guard.data?.effective?.enabled;
	const checkBad = check.data?.self_check.outcome === "error";
	const status = useMemo(() => {
		if (guard.isError || check.isError || !check.data || enabled === undefined) return "unknown";
		if (checkBad) return "selfCheckFailed";
		return enabled ? "active" : "inactive";
	}, [guard.isError, check.isError, check.data, enabled, checkBad]);

	const open = useMemo(() => cases.data?.filter((c) => c.status === "investigating").length ?? 0, [cases.data]);
	const accountCount = accounts.isError ? undefined : accounts.data?.total;
	const exitCount = useMemo(() => nodes.data?.items.filter((n) => n.quality).length ?? 0, [nodes.data?.items]);

	// Keep visited tabs in DOM for instant 0ms switching without remounting lag
	const [visitedTabs, setVisitedTabs] = useState<Record<string, boolean>>(() => ({ [view]: true }));
	if (!visitedTabs[view]) {
		setVisitedTabs((prev) => ({ ...prev, [view]: true }));
	}

	const change = (next: string) => navigate({ pathname: "/guard", hash: next });
	return (
		<div className="ops-workspace">
			{/* Top Bar: Brand, Status, and Action Hub (Command Deck Style) */}
			<div className="flex flex-wrap items-center justify-between gap-4 py-1 mb-1">
				{/* Title and Live status */}
				<div className="flex items-center gap-3">
					<div className="relative flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/20">
						<ShieldCheck className="size-5 animate-pulse text-primary" />
					</div>
					<div>
						<div className="flex items-center gap-2">
							<h1 className="text-xl font-bold tracking-tight text-foreground">
								{t("ops.quality")}
							</h1>
							<span
								className={cn(
									"inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
									status === "active"
										? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
										: status === "unknown"
											? "bg-muted text-muted-foreground"
											: "bg-rose-500/10 text-rose-600 dark:text-rose-400"
								)}
							>
								<span
									className={cn(
										"size-1.5 rounded-full",
										status === "active"
											? "bg-emerald-500 animate-ping"
											: status === "unknown"
												? "bg-muted-foreground"
												: "bg-rose-500 animate-ping"
									)}
								/>
								{t(`ops.${status}`)}
							</span>
						</div>
						<p className="text-xs text-muted-foreground mt-0.5">
							{open ?? 0} {t("ops.cases")} · {accountCount ?? "—"} {t("ops.heldAccounts")} · {exitCount ?? 0} {t("ops.heldExits")}
						</p>
					</div>
				</div>

				{/* Quick Action Center */}
				<div className="flex flex-wrap items-center gap-2">
					<Button variant="outline" size="sm" className="gap-1.5 font-medium shadow-xs" asChild>
						<Link to="/guard/settings">
							<Settings2 className="size-3.5" />
							<span>{t("ops.settings")}</span>
						</Link>
					</Button>

					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="ghost"
								size="icon"
								className="size-8"
								disabled={guard.isFetching || check.isFetching || cases.isFetching}
								onClick={() => {
									void guard.refetch();
									void check.refetch();
									void cases.refetch();
									void cache.invalidateQueries({ queryKey: ["accounts"] });
									void nodes.refetch();
								}}
							>
								<RefreshCw
									className={cn(
										"size-4 text-muted-foreground",
										(guard.isFetching || check.isFetching || cases.isFetching) && "animate-spin text-primary"
									)}
								/>
							</Button>
						</TooltipTrigger>
						<TooltipContent>{t("network.refresh")}</TooltipContent>
					</Tooltip>
				</div>
			</div>
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
				{visitedTabs.tribunal && (
					<TabsContent forceMount value="tribunal" className="data-[state=inactive]:hidden mt-0 focus-visible:outline-none">
						<QualityTribunalView />
					</TabsContent>
				)}
				{visitedTabs.stats && (
					<TabsContent forceMount value="stats" className="data-[state=inactive]:hidden mt-0">
						<QualityHitStats />
					</TabsContent>
				)}
				{visitedTabs.bureau && (
					<TabsContent forceMount value="bureau" className="data-[state=inactive]:hidden mt-0">
						<QualityProbeView />
					</TabsContent>
				)}
			</Tabs>
		</div>
	);
}
