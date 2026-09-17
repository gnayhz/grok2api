import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
	Activity,
	AlertTriangle,
	CheckCircle2,
	Layers,
	Plus,
	Radio,
	RefreshCw,
	Server,
	Settings2,
	Trash2,
	Zap,
} from "lucide-react";
import { useState } from "react";
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
} from "@/shared/ui/alert-dialog";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Dialog, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { Label } from "@/shared/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/shared/ui/select";
import { Spinner } from "@/shared/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { useNow } from "@/shared/lib/use-now";
import { networkSummary } from "@/entities/egress/node-condition";

import { OperationsDialogContent, OperationsAlertDialogContent } from "@/shared/ui/operations";
import {
	cleanupUnhealthyEgressNodes,
	previewUnhealthyEgressNodes,
	testEgressNodes,
} from "@/entities/egress/egress-api";
import { cn } from "@/shared/lib/cn";

import { getNetworkRuntime } from "@/entities/egress/network-runtime-api";
import { IntervalInput } from "./operations-context";
import { useEgressOperations } from "./operations-shared";
import { getLatencyTone } from "./proxy-format";
import { useEgressNodes, useEgressPools } from "@/entities/egress/egress-queries";

export function ProxyCommandBar({
	onOpenAddModal,
}: {
	onOpenAddModal: (tab?: "manual" | "bulk" | "subscriptions") => void;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const operations = useEgressOperations();
	const nodes = useEgressNodes();
	const pools = useEgressPools();
	const now = useNow(15_000);

	// Runtime connections telemetry query
	const runtimeQuery = useQuery({
		queryKey: ["egress-runtime"],
		queryFn: ({ signal }) => getNetworkRuntime(signal),
		refetchInterval: 10_000,
	});

	// Probe all nodes mutation
	const [probingAll, setProbingAll] = useState(false);
	const probeAllMutation = useMutation({
		mutationFn: () => testEgressNodes([]),
		onMutate: () => setProbingAll(true),
		onSuccess: (result) => {
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			toast.success(
				t("network.testAllDone", {
					healthy: result.healthy,
					unhealthy: result.unhealthy,
				})
			);
		},
		onError: (err) => {
			toast.error(err instanceof Error ? err.message : t("network.probeTestError"));
		},
		onSettled: () => setProbingAll(false),
	});

	// Foolproof Secondary Cleanup Confirmation Dialog state
	const [cleanupDialogOpen, setCleanupDialogOpen] = useState(false);
	const cleanupPreviewQuery = useQuery({
		queryKey: ["egress-cleanup-preview"],
		queryFn: () => previewUnhealthyEgressNodes(),
		enabled: cleanupDialogOpen,
	});

	const cleanupMutation = useMutation({
		mutationFn: () => cleanupUnhealthyEgressNodes(),
		onSuccess: (res) => {
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			toast.success(t("network.cleanUnhealthySuccess", { count: res.deleted }));
			setCleanupDialogOpen(false);
		},
		onError: (err) => {
			toast.error(err instanceof Error ? err.message : t("network.cleanupError"));
		},
	});

	// Probe engine settings dialog
	const [settingsOpen, setSettingsOpen] = useState(false);
	const [probeProvider, setProbeProvider] = useState(operations.form.probeProvider);
	const [probeInterval, setProbeInterval] = useState(String(operations.form.probeIntervalSeconds));

	const items = nodes.data?.items ?? [];
	const summary = nodes.data ? networkSummary(items, now) : undefined;
	const runtime = runtimeQuery.data?.network;

	// Dual-Stack IPv4 / IPv6 counts
	const ipv6Count = items.filter(
		(n) => n.enabled && n.ipv6Probe && n.ipv6Probe.status === "healthy"
	).length;

	const latency = summary?.median ?? null;
	const tone = getLatencyTone(latency);
	const readyPercent = summary && summary.total > 0 ? Math.round((summary.ready / summary.total) * 100) : 0;

	const handleRefresh = () => {
		for (const key of [
			"egress-nodes",
			"egress-pools",
			"egress-sources",
			"egress-operations",
			"egress-runtime",
			"egress-routing-stats",
		]) {
			void queryClient.invalidateQueries({ queryKey: [key] });
		}
		toast.info(t("network.refreshed", { time: new Date().toLocaleTimeString() }));
	};

	return (
		<div className="flex flex-col gap-4">
			{/* Top Bar: Brand, Status, and Action Hub */}
			<div className="flex flex-wrap items-center justify-between gap-4 py-1">
				{/* Title and Live status */}
				<div className="flex items-center gap-3">
					<div className="relative flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/20">
						<Radio className="size-5 animate-pulse text-primary" />
					</div>
					<div>
						<div className="flex items-center gap-2">
							<h1 className="text-xl font-bold tracking-tight text-foreground">
								{t("ops.network")}
							</h1>
							<span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/10 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
								<span className="size-1.5 rounded-full bg-emerald-500 animate-ping" />
								{t("network.pulseLive")}
							</span>
						</div>
						<p className="text-xs text-muted-foreground mt-0.5">
							{t("network.total", { count: nodes.data?.total ?? 0 })} · {pools.data?.length ?? 0} {t("ops.poolTab")} · {ipv6Count} {t("network.ipv6Supported")}
						</p>
					</div>
				</div>

				{/* Quick Action Center */}
				<div className="flex flex-wrap items-center gap-2">
					{/* 1. Add Node / Import Hub (Guaranteed working click!) */}
					<Button
						size="sm"
						className="gap-1.5 shadow-sm bg-primary text-primary-foreground hover:bg-primary/90 font-medium"
						onClick={() => onOpenAddModal("manual")}
					>
						<Plus className="size-4" />
						<span>{t("network.addNodeOrImport")}</span>
					</Button>

					{/* 2. One-click Test All */}
					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="outline"
								size="sm"
								className="gap-1.5"
								disabled={probingAll || nodes.isFetching}
								onClick={() => probeAllMutation.mutate()}
							>
								{probingAll ? <Spinner className="size-3.5" /> : <Zap className="size-3.5 text-amber-500" />}
								<span>{probingAll ? t("network.testingAll") : t("network.testAll")}</span>
							</Button>
						</TooltipTrigger>
						<TooltipContent>{t("networkResources.probeLatency")}</TooltipContent>
					</Tooltip>

					{/* 3. Clean Dead Nodes (Foolproof secondary confirmation) */}
					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="outline"
								size="sm"
								className="gap-1.5 text-muted-foreground hover:text-destructive hover:border-destructive/40"
								onClick={() => setCleanupDialogOpen(true)}
							>
								<Trash2 className="size-3.5" />
								<span>{t("network.cleanUnhealthy")}</span>
							</Button>
						</TooltipTrigger>
						<TooltipContent>{t("network.cleanUnhealthyTitle")}</TooltipContent>
					</Tooltip>

					{/* 4. Probe Engine Config */}
					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="outline"
								size="sm"
								className="gap-1.5"
								onClick={() => {
									setProbeProvider(operations.form.probeProvider);
									setProbeInterval(String(operations.form.probeIntervalSeconds));
									setSettingsOpen(true);
								}}
							>
								<Settings2 className="size-3.5" />
								<span>{t("network.probeConfig")}</span>
							</Button>
						</TooltipTrigger>
						<TooltipContent>{t("proxies.automation.title")}</TooltipContent>
					</Tooltip>

					{/* 5. Refresh State */}
					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="ghost"
								size="icon"
								className="size-8"
								disabled={nodes.isFetching || pools.isFetching}
								onClick={handleRefresh}
							>
								<RefreshCw
									className={cn(
										"size-4 text-muted-foreground",
										(nodes.isFetching || pools.isFetching) && "animate-spin text-primary"
									)}
								/>
							</Button>
						</TooltipTrigger>
						<TooltipContent>{t("network.refresh")}</TooltipContent>
					</Tooltip>
				</div>
			</div>

			{/* Telemetry Metric Cards Grid */}
			<div className="grid grid-cols-2 gap-3 sm:grid-cols-4 lg:grid-cols-4">
				{/* Card 1: Fleet Readiness */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/70 p-3.5 shadow-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Server className="size-3.5 text-primary" />
							{t("network.fleetHealth")}
						</span>
						<Badge variant="outline" className="text-[10px] font-semibold tabular-nums">
							{readyPercent}%
						</Badge>
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span className="text-2xl font-black tracking-tight tabular-nums text-foreground">
							{summary?.ready ?? 0}
						</span>
						<span className="text-xs text-muted-foreground">
							/ {summary?.total ?? 0} {t("network.ready")}
						</span>
					</div>
					<div className="mt-2.5 h-1.5 w-full overflow-hidden rounded-full bg-muted">
						<div
							className="h-full rounded-full bg-emerald-500 transition-all duration-500"
							style={{ width: `${readyPercent}%` }}
						/>
					</div>
				</div>

				{/* Card 2: Median Latency */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/70 p-3.5 shadow-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Activity className="size-3.5 text-emerald-500" />
							{t("network.median")}
						</span>
						<Badge variant="outline" className={cn("text-[9px] px-1 py-0", tone.badgeClass)}>
							{tone.label}
						</Badge>
					</div>
					<div className="mt-2 flex items-baseline gap-1.5">
						<span className={cn("text-2xl font-black tracking-tight tabular-nums", tone.textClass)}>
							{latency !== null ? `${latency}` : "--"}
						</span>
						<span className="text-xs text-muted-foreground">ms</span>
					</div>
					<div className="mt-2 flex items-center gap-1.5 text-[11px] text-muted-foreground">
						{summary && summary.attention > 0 ? (
							<span className="flex items-center gap-1 text-amber-500 font-medium">
								<AlertTriangle className="size-3" />
								{summary.attention} {t("network.attention")}
							</span>
						) : (
							<span className="flex items-center gap-1 text-emerald-600 dark:text-emerald-400">
								<CheckCircle2 className="size-3" />
								{t("network.noIssues")}
							</span>
						)}
					</div>
				</div>

				{/* Card 3: Physical Connections & Limits */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/70 p-3.5 shadow-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Radio className="size-3.5 text-blue-500" />
							{t("network.activeConns")}
						</span>
						{runtime?.limits?.Connections ? (
							<span className="text-[10px] text-muted-foreground font-mono">
								{t("network.connsMax", { count: runtime.limits.Connections })}
							</span>
						) : null}
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span className="text-2xl font-black tracking-tight tabular-nums text-foreground">
							{runtime?.activeConnections ?? 0}
						</span>
						<span className="text-xs text-muted-foreground">
							({runtime?.connections ?? 0} {t("network.connections")})
						</span>
					</div>
					<div className="mt-2 flex items-center justify-between text-[11px] text-muted-foreground">
						<span>{t("network.idleConns")}: {runtime?.idleConnections ?? 0}</span>
						<span>{t("network.establishingConns")}: {runtime?.establishingConnections ?? 0}</span>
					</div>
				</div>

				{/* Card 4: Requests & Queue Saturation */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/70 p-3.5 shadow-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Layers className="size-3.5 text-violet-500" />
							{t("network.requests")}
						</span>
						{(runtime?.waiters ?? 0) > 0 || (runtime?.rejected ?? 0) > 0 ? (
							<span className="rounded bg-rose-500/10 px-1.5 py-0.5 text-[10px] font-semibold text-rose-500">
								{t("network.overloaded")}
							</span>
						) : (
							<span className="text-[10px] text-emerald-500 font-medium">
								{t("network.normal")}
							</span>
						)}
					</div>
					<div className="mt-2 flex items-baseline gap-2">
						<span className="text-2xl font-black tracking-tight tabular-nums text-foreground">
							{runtime?.requests ?? 0}
						</span>
						<span className="text-xs text-muted-foreground">{t("network.inFlight")}</span>
					</div>
					<div className="mt-2 flex items-center justify-between text-[11px]">
						<span className={cn((runtime?.waiters ?? 0) > 0 ? "font-bold text-amber-500" : "text-muted-foreground")}>
							{t("network.waiters")}: {runtime?.waiters ?? 0}
						</span>
						<span className={cn((runtime?.rejected ?? 0) > 0 ? "font-bold text-rose-500" : "text-muted-foreground")}>
							{t("network.rejectedCount", { count: runtime?.rejected ?? 0 })}
						</span>
					</div>
				</div>
			</div>

			{/* Secondary Cleanup Confirmation Dialog */}
			<AlertDialog open={cleanupDialogOpen} onOpenChange={setCleanupDialogOpen}>
				<OperationsAlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle className="flex items-center gap-2">
							{cleanupPreviewQuery.data?.nodes === 0 ? (
								<CheckCircle2 className="size-5 text-emerald-500" />
							) : (
								<AlertTriangle className="size-5 text-destructive" />
							)}
							<span>{t("network.cleanUnhealthyTitle")}</span>
						</AlertDialogTitle>
						<AlertDialogDescription className="text-sm pt-2">
							{cleanupPreviewQuery.isPending ? (
								<div className="flex items-center gap-2 py-4 text-muted-foreground">
									<Spinner className="size-4" />
									<span>{t("network.scanningOfflineNodes")}</span>
								</div>
							) : cleanupPreviewQuery.data?.nodes === 0 ? (
								<div className="py-2 text-foreground font-medium">
									{t("network.cleanUnhealthyEmpty")}
								</div>
							) : (
								<div className="space-y-2 text-foreground">
									<p>
										{t("network.cleanUnhealthyDesc", {
											count: cleanupPreviewQuery.data?.nodes ?? 0,
											subCount: cleanupPreviewQuery.data?.subscriptionManaged ?? 0,
										})}
									</p>
								</div>
							)}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
						{cleanupPreviewQuery.data && cleanupPreviewQuery.data.nodes > 0 && (
							<AlertDialogAction
								className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
								disabled={cleanupMutation.isPending}
								onClick={() => cleanupMutation.mutate()}
							>
								{cleanupMutation.isPending && <Spinner className="mr-1.5 size-3.5" />}
								<span>{t("network.cleanUnhealthy")} ({cleanupPreviewQuery.data.nodes})</span>
							</AlertDialogAction>
						)}
					</AlertDialogFooter>
				</OperationsAlertDialogContent>
			</AlertDialog>

			{/* Probe Engine Config Dialog */}
			<Dialog open={settingsOpen} onOpenChange={setSettingsOpen}>
				<OperationsDialogContent className="max-w-md">
					<DialogHeader>
						<DialogTitle className="flex items-center gap-2">
							<Settings2 className="size-5 text-primary" />
							{t("proxies.automation.title")}
						</DialogTitle>
					</DialogHeader>
					<div className="space-y-4 py-2">
						<div className="space-y-1.5">
							<label className="text-xs font-semibold text-foreground">
								{t("settings.egress.probeProvider")}
							</label>
							<p className="text-[11px] text-muted-foreground">
								{t("settings.egress.probeProviderHelp")}
							</p>
							<Select
								value={probeProvider}
								onValueChange={(val: "ipinfo" | "cloudflare") => setProbeProvider(val)}
							>
								<SelectTrigger className="w-full">
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									<SelectItem value="cloudflare">Cloudflare Trace API</SelectItem>
									<SelectItem value="ipinfo">IPinfo API</SelectItem>
								</SelectContent>
							</Select>
						</div>

						<div className="space-y-1.5">
							<div className="flex items-center justify-between">
								<Label htmlFor="probe-interval-input" className="text-xs font-semibold text-foreground">
									{t("network.probeIntervalLabel")}
								</Label>
								<span className="text-[11px] text-muted-foreground font-medium">
									{t("network.probeIntervalRange")}
								</span>
							</div>
							<p className="text-[11px] text-muted-foreground">
								{t("settings.egress.probeIntervalHelp")}
							</p>
							<IntervalInput
								id="probe-interval-input"
								value={probeInterval}
								onChange={setProbeInterval}
							/>
						</div>
					</div>
					<DialogFooter>
						<Button variant="outline" size="sm" onClick={() => setSettingsOpen(false)}>
							{t("common.cancel")}
						</Button>
						<Button
							size="sm"
							onClick={() => {
								const sec = Number(probeInterval);
								if (Number.isInteger(sec) && sec >= 60 && sec <= 86400) {
									operations.update((cur) => ({
										...cur,
										probeProvider,
										probeIntervalSeconds: sec,
									}));
									setSettingsOpen(false);
									toast.success(t("ops.applyDraft"));
								} else {
									toast.error(t("ops.probeIntervalInvalid"));
								}
							}}
						>
							{t("ops.applyDraft")}
						</Button>
					</DialogFooter>
				</OperationsDialogContent>
			</Dialog>
		</div>
	);
}

/**
 * Floating Save / Discard Dock for Pending Routing Changes
 */
export function ProxyFloatingSaveDock() {
	const { t } = useTranslation();
	const operations = useEgressOperations();

	if (!operations.isDirty) return null;

	return (
		<div className="fixed bottom-6 left-1/2 z-50 -translate-x-1/2 animate-in fade-in slide-in-from-bottom-5 duration-200">
			<div className="flex items-center gap-3 rounded-full border border-amber-500/40 bg-card/95 px-5 py-2.5 shadow-2xl backdrop-blur-xl ring-1 ring-amber-500/20">
				<span className="flex size-2 rounded-full bg-amber-500 animate-ping" />
				<span className="text-xs font-medium text-foreground">
					{t("network.hasUnsavedChanges")}
				</span>
				<div className="flex items-center gap-2">
					<Button
						size="sm"
						variant="ghost"
						className="h-7 text-xs text-muted-foreground hover:text-foreground"
						disabled={operations.savePending}
						onClick={operations.discard}
					>
						{t("network.discardDraft")}
					</Button>
					<Button
						size="sm"
						className="h-7 gap-1.5 bg-primary text-primary-foreground hover:bg-primary/90 text-xs shadow-sm font-semibold"
						disabled={operations.savePending}
						onClick={operations.save}
					>
						{operations.savePending && <Spinner className="size-3" />}
						{t("network.saveDraft")}
					</Button>
				</div>
			</div>
		</div>
	);
}