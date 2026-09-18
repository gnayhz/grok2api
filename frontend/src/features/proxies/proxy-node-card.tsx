// 代理节点卡片(ui 层):节点行卡片及其徽章/工具栏展示。状态与命令
// 经命名 props 传入;列表/对话框/生命周期在 proxy-nodes-view.tsx。
import { memo } from "react";
import { useTranslation } from "react-i18next";
import { Copy, Layers, MoreHorizontal, Pencil, Power, PowerOff, RefreshCw, ShieldAlert, Trash2, Zap } from "lucide-react";
import type { EgressNodeDTO } from "@/entities/egress/egress-api";
import { cn } from "@/shared/lib/cn";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Checkbox } from "@/shared/ui/checkbox";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/shared/ui/dropdown-menu";
import { Spinner } from "@/shared/ui/spinner";
import { Switch } from "@/shared/ui/switch";
import { maskIP } from "@/shared/lib/mask-ip";
import { formatTimeAgo, getLatencyTone } from "./proxy-format";

export const ProxyNodeCardItem = memo(function ProxyNodeCardItem({
	node,
	cond,
	isSelected,
	isProbing,
	locale,
	onToggleSelect,
	onToggleEnabled,
	onTestProbe,
	onQualityCheck,
	onRotate,
	onRevealProxyURL,
	onRevealRotationURL,
	onEdit,
	onDelete,
	onUnban,
	onViewPools,
}: {
	node: EgressNodeDTO;
	cond: string;
	isSelected: boolean;
	isProbing: boolean;
	locale: string;
	onToggleSelect: (id: string) => void;
	onToggleEnabled: (id: string, enabled: boolean) => void;
	onTestProbe: (id: string) => void;
	onQualityCheck: () => void;
	onRotate: (id: string) => void;
	onRevealProxyURL: (id: string) => void;
	onRevealRotationURL: (id: string) => void;
	onEdit: (node: EgressNodeDTO) => void;
	onDelete: (node: EgressNodeDTO) => void;
	onUnban: (node: EgressNodeDTO) => void;
	onViewPools?: () => void;
}) {
	const { t } = useTranslation();

	const beaconTone =
		cond === "ready"
			? "bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.5)]"
			: cond === "dynamic"
				? "bg-blue-500 shadow-[0_0_8px_rgba(59,130,246,0.5)]"
				: cond === "cooling"
					? "bg-amber-500"
					: cond === "unhealthy"
						? "bg-rose-500"
						: cond === "held" || cond === "banned"
							? "bg-purple-500"
							: "bg-zinc-400";

	const ipv4 = node.ipv4Probe;
	const ipv6 = node.ipv6Probe;
	const latencyTone = getLatencyTone(node.probeLatencyMs);

	return (
		<div
			className={cn(
				"group relative flex flex-col justify-between rounded-xl border bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md h-full",
				isSelected ? "border-primary ring-1 ring-primary/30" : "border-border/80"
			)}
		>
			<div>
				{/* Top bar: Select checkbox, Name, Switch, and Menu */}
				<div className="flex items-start justify-between gap-2">
					<div className="flex items-center gap-2 min-w-0">
						<Checkbox checked={isSelected} onCheckedChange={() => onToggleSelect(node.id)} />
						<span className={cn("size-2 shrink-0 rounded-full", beaconTone)} />
						<span className="truncate text-sm font-bold text-foreground" title={node.name}>
							{node.name}
						</span>
					</div>

					<div className="flex items-center gap-1 shrink-0">
						<Switch
							checked={node.enabled}
							onCheckedChange={(val) => onToggleEnabled(node.id, val)}
							className="scale-75 origin-right"
							title={node.enabled ? t("common.disable") : t("common.enable")}
						/>
						<DropdownMenu>
							<DropdownMenuTrigger asChild>
								<Button size="icon" variant="ghost" className="size-7 opacity-70 group-hover:opacity-100">
									<MoreHorizontal className="size-3.5" />
								</Button>
							</DropdownMenuTrigger>
							<DropdownMenuContent align="end" className="w-44">
								<DropdownMenuItem onClick={onQualityCheck}><ShieldAlert className="mr-2 size-3.5" />{t("resourceChecks.action")}</DropdownMenuItem>
								<DropdownMenuItem onClick={() => onTestProbe(node.id)}>
									<Zap className="mr-2 size-3.5 text-amber-500" />
									<span>{t("network.testLatency")}</span>
								</DropdownMenuItem>
								<DropdownMenuItem onClick={() => onToggleEnabled(node.id, !node.enabled)}>
									{node.enabled ? (
										<PowerOff className="mr-2 size-3.5 text-zinc-500" />
									) : (
										<Power className="mr-2 size-3.5 text-emerald-500" />
									)}
									<span>{node.enabled ? t("common.disable") : t("common.enable")}</span>
								</DropdownMenuItem>
							{node.rotationConfigured && node.rotationEnabled && (
								<DropdownMenuItem onClick={() => onRotate(node.id)}>
									<RefreshCw className="mr-2 size-3.5 text-blue-500" />
									<span>{t("network.rotateAction")}</span>
								</DropdownMenuItem>
							)}
							{node.proxyConfigured && (
								<DropdownMenuItem onClick={() => onRevealProxyURL(node.id)}>
									<Copy className="mr-2 size-3.5" />
									<span>{t("network.copyProxyUrl")}</span>
								</DropdownMenuItem>
							)}
							{node.rotationConfigured && node.rotationEnabled && (
								<DropdownMenuItem onClick={() => onRevealRotationURL(node.id)}>
									<Copy className="mr-2 size-3.5 text-purple-500" />
									<span>{t("network.copyRotationUrl")}</span>
								</DropdownMenuItem>
							)}
							{node.quality?.state && (
								<DropdownMenuItem onClick={() => onUnban(node)}>
									<ShieldAlert className="mr-2 size-3.5 text-purple-500" />
									<span>{t("network.releaseUnban")}</span>
								</DropdownMenuItem>
							)}
							<DropdownMenuSeparator />
							<DropdownMenuItem onClick={() => onEdit(node)}>
								<Pencil className="mr-2 size-3.5" />
								<span>{t("common.edit")}</span>
							</DropdownMenuItem>
							<DropdownMenuItem onClick={() => onDelete(node)} className="text-destructive focus:text-destructive">
								<Trash2 className="mr-2 size-3.5" />
								<span>{t("common.delete")}</span>
							</DropdownMenuItem>
						</DropdownMenuContent>
					</DropdownMenu>
					</div>
				</div>

				{/* Feature Badges */}
				<div className="mt-2 flex flex-wrap items-center gap-1.5 min-h-[22px]">
					{node.rotatingEndpoint ? (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-blue-500/30 text-blue-600 dark:text-blue-400 bg-blue-500/5 shrink-0">
							{t("network.dynamicTunnel")}
						</Badge>
					) : node.rotationConfigured && node.rotationEnabled ? (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-purple-500/30 text-purple-600 dark:text-purple-400 bg-purple-500/5 shrink-0">
							{t("network.rotateWebhookLabel")}
						</Badge>
					) : (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-zinc-500/30 text-zinc-600 dark:text-zinc-400 bg-zinc-500/5 shrink-0">
							{t("network.fixedExitBadge")}
						</Badge>
					)}
					{node.accountBoundProxy && (
						<Badge variant="outline" className="text-[10px] px-1.5 py-0 border-purple-500/30 text-purple-600 dark:text-purple-400 bg-purple-500/5">
							{t("network.accountBound")}
						</Badge>
					)}
					{node.sourceName && (
						<Badge variant="secondary" className="text-[10px] px-1.5 py-0 truncate max-w-[130px]">
							{node.sourceName}
						</Badge>
					)}
					{node.quality?.state && (
						<Badge variant="destructive" className="text-[10px] px-1.5 py-0 animate-pulse">
							{t("network.courtStatus", { state: node.quality.state })}
						</Badge>
					)}
				</div>

				{/* Dual-Stack Telemetry Box (IPv4 & IPv6 explicitly shown!) */}
				<div className="mt-3 flex flex-col gap-1.5 rounded-lg border border-border/60 bg-muted/20 p-2.5">
					{/* IPv4 Status */}
					<div className="flex items-center justify-between font-mono text-xs">
						<div className="flex items-center gap-1.5 min-w-0">
							<span className="rounded bg-emerald-500/10 px-1 py-0.2 text-[9px] font-bold text-emerald-600 dark:text-emerald-400">
								IPv4
							</span>
							<span className="truncate text-[11px] text-foreground font-semibold">
								{ipv4?.exitIp ? maskIP(ipv4.exitIp) : node.exitIp ? maskIP(node.exitIp) : t("network.notConfigured")}
							</span>
						</div>
						<span className="text-[10px] text-muted-foreground tabular-nums shrink-0">
							{ipv4?.latencyMs ? `${ipv4.latencyMs}ms` : "--"}
						</span>
					</div>

					{/* IPv6 Status */}
					<div className="flex items-center justify-between font-mono text-xs border-t border-border/40 pt-1.5">
						<div className="flex items-center gap-1.5 min-w-0">
							<span className={cn(
								"rounded px-1 py-0.2 text-[9px] font-bold",
								ipv6?.status === "healthy"
									? "bg-cyan-500/10 text-cyan-600 dark:text-cyan-400"
									: "bg-muted text-muted-foreground"
							)}>
								IPv6
							</span>
							<span className="truncate text-[11px] text-muted-foreground">
								{ipv6?.exitIp ? maskIP(ipv6.exitIp) : t("network.ipv6None")}
							</span>
						</div>
						<span className="text-[10px] text-muted-foreground tabular-nums shrink-0">
							{ipv6?.latencyMs ? `${ipv6.latencyMs}ms` : "--"}
						</span>
					</div>
				</div>

				{/* Pool Membership Chips */}
				<div className="mt-2.5 flex items-center gap-1.5 text-xs text-muted-foreground min-h-[22px]">
					<Layers className="size-3.5 shrink-0" />
					<div className="flex flex-wrap gap-1 min-w-0">
						{node.pools && node.pools.length > 0 ? (
							node.pools.map((p) => (
								<Badge
									key={p.id}
									variant="outline"
									className="text-[9px] px-1 py-0 cursor-pointer hover:bg-accent"
									onClick={onViewPools}
								>
									{p.name}
								</Badge>
							))
						) : (
							<span className="text-[11px] text-muted-foreground/60">{t("networkResources.noPool")}</span>
						)}
					</div>
				</div>
			</div>

			{/* Card Footer: Latency Summary & Inline Actions */}
			<div className="mt-3 flex items-center justify-between border-t border-border/60 pt-2.5">
				<div className="flex items-center gap-1.5">
					{isProbing ? (
						<Spinner className="size-3.5" />
					) : node.probeStatus === "healthy" ? (
						<span className={cn("text-xs font-mono tabular-nums", latencyTone.textClass)}>
							{node.probeLatencyMs}ms
						</span>
					) : node.probeStatus === "unhealthy" ? (
						<span className="text-xs font-semibold text-rose-500 font-mono">
							{t("network.unhealthy")}
						</span>
					) : (
						<span className="text-xs text-muted-foreground font-mono">
							{t("network.neverProbed")}
						</span>
					)}
					<span className="text-[10px] text-muted-foreground">
						· {node.lastProbedAt ? formatTimeAgo(node.lastProbedAt, locale) : t("network.neverProbed")}
					</span>
				</div>

				<div className="flex items-center gap-1">
					<Button
						size="icon"
						variant="ghost"
						className="size-6 text-muted-foreground hover:text-amber-500"
						disabled={isProbing}
						onClick={() => onTestProbe(node.id)}
						title={t("network.testLatency")}
					>
						<Zap className="size-3" />
					</Button>

					{node.rotationConfigured && node.rotationEnabled && (
						<Button
							size="icon"
							variant="ghost"
							className="size-6 text-muted-foreground hover:text-blue-500"
							onClick={() => onRotate(node.id)}
							title={t("network.rotateAction")}
						>
							<RefreshCw className="size-3" />
						</Button>
					)}

					{node.proxyConfigured && (
						<Button
							size="icon"
							variant="ghost"
							className="size-6 text-muted-foreground hover:text-foreground"
							onClick={() => onRevealProxyURL(node.id)}
							title={t("network.copyProxyUrl")}
						>
							<Copy className="size-3" />
						</Button>
					)}
				</div>
			</div>
		</div>
	);
});

/**
 * Add or Import Dialog: 3 Tabs (Manual with Quick Test, Bulk Text, Subscriptions)
 */
