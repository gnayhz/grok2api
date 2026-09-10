import { useQuery } from "@tanstack/react-query";
import { ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { getGuardStats } from "./guard-stats-api";
import {
	OperationsError,
	OperationsSection,
	StatusPill,
} from "@/features/operations/operations-ui";
import { Spinner } from "@/components/ui/spinner";

export function QualityHitStats() {
	const { t } = useTranslation();
	const query = useQuery({
		queryKey: ["guard-stats"],
		queryFn: getGuardStats,
		refetchInterval: 10000,
	});
	if (query.isPending)
		return (
			<div className="p-12">
				<Spinner />
			</div>
		);
	if (query.isError)
		return (
			<OperationsError
				message={query.error.message}
				retry={() => void query.refetch()}
			/>
		);
	const stats = query.data;
	const active = stats.signals.some((s) => s.triggered > 0);
	const exemptions = (stats.exempts ?? []).filter((e) => e.count > 0);
	return (
		<div className="space-y-5">
			<OperationsSection
				title={t("ops.guardEffect")}
				description={t("ops.guardEffectHelp")}
				action={<StatusPill>{t("ops.processScope")}</StatusPill>}
			>
				{!active && (
					<div className="mb-6 flex items-center gap-4 rounded-lg bg-muted/60 p-5">
						<div className="grid size-11 shrink-0 place-items-center rounded-xl border bg-card">
							<ShieldCheck className="size-5 text-primary" />
						</div>
						<div>
							<p className="text-sm font-medium">{t("ops.noSignals")}</p>
							<p className="mt-1 text-xs leading-6 text-muted-foreground">
								{t("ops.noSignalsHelp")}
							</p>
						</div>
					</div>
				)}
				<div className="-mx-4 overflow-x-auto">
					<table className="ops-table min-w-[650px]">
						<thead>
							<tr>
								{["signal", "triggered", "outcome", "ratio"].map((k) => (
									<th key={k}>{t(`ops.${k}`)}</th>
								))}
							</tr>
						</thead>
						<tbody>
							{stats.signals.map((signal) => {
								const settled = signal.rescued + signal.failed;
								const ratio = settled ? signal.rescued / settled : null;
								return (
									<tr key={signal.signal}>
										<td>
											<p className="ops-row-primary">
												{t(`settings.guardStats.signals.${signal.signal}`, {
													defaultValue: signal.signal,
												})}
											</p>
											<p className="ops-row-secondary">
												{signal.lastSeen
													? new Date(signal.lastSeen).toLocaleString()
													: t("ops.noData")}
											</p>
										</td>
										<td className="font-medium tabular-nums">
											{signal.triggered}
										</td>
										<td>
											<div className="mb-2 flex max-w-64 justify-between gap-8 text-[11px]">
												<span className="text-muted-foreground">
													{t("ops.recovered")}{" "}
													<strong className="font-medium text-foreground">
														{signal.rescued}
													</strong>
												</span>
												<span className="text-muted-foreground">
													{t("ops.unrecovered")}{" "}
													<strong className="font-medium text-foreground">
														{signal.failed}
													</strong>
												</span>
											</div>
											<div className="flex h-1.5 max-w-64 overflow-hidden rounded-full bg-muted">
												{ratio !== null && (
													<>
														<span
															style={{
																width: `${ratio * 100}%`,
																background: "var(--ops-good)",
															}}
														/>
														<span
															style={{
																width: `${(1 - ratio) * 100}%`,
																background: "var(--ops-bad)",
															}}
														/>
													</>
												)}
											</div>
										</td>
										<td className="font-medium tabular-nums">
											{ratio === null ? "—" : `${Math.round(ratio * 100)}%`}
										</td>
									</tr>
								);
							})}
						</tbody>
					</table>
				</div>
			</OperationsSection>
			<div className="grid gap-5 lg:grid-cols-[1.3fr_1fr]">
				<OperationsSection
					title={t("ops.coverage")}
					description={t("ops.coverageHelp")}
				>
					{exemptions.length ? (
						<div className="divide-y">
							{exemptions.map((e) => (
								<div
									key={e.reason}
									className="flex items-center justify-between py-3 text-xs"
								>
									<span className="text-muted-foreground">
										{t(`settings.guardStats.exempts.${e.reason}`, {
											defaultValue: e.reason,
										})}
									</span>
									<strong className="font-medium tabular-nums">
										{e.count}
									</strong>
								</div>
							))}
						</div>
					) : (
						<p className="py-8 text-center text-sm text-muted-foreground">
							{t("ops.coverageNone")}
						</p>
					)}
				</OperationsSection>
				<OperationsSection
					title={t("ops.scope")}
					description={
						stats.since
							? t("ops.sinceTime", {
									time: new Date(stats.since).toLocaleString(),
								})
							: t("ops.processReset")
					}
				>
					<p className="text-xs leading-7 text-muted-foreground">
						{t("ops.guardEffectHelp")}
					</p>
					<div className="mt-4 border-t pt-4">
						<p className="mb-2 text-xs font-medium">{t("ops.guardModels")}</p>
						<div className="flex flex-wrap gap-2">
							{stats.effective?.guardedModels?.map((model) => (
								<StatusPill key={model}>
									{model.replace(/^grok_build:/, "")}
								</StatusPill>
							))}
						</div>
					</div>
				</OperationsSection>
			</div>
		</div>
	);
}
