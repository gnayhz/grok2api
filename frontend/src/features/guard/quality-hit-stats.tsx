import { useQuery } from "@tanstack/react-query";
import {
	Activity,
	AlertCircle,
	AlertTriangle,
	Bot,
	CheckCircle2,
	Clock,
	Cpu,
	Server,
	ShieldAlert,
	ShieldCheck,
	Sparkles,
	Zap,
} from "lucide-react";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/shared/ui/badge";
import { Spinner } from "@/shared/ui/spinner";
import { cn } from "@/shared/lib/cn";

import { getGuardStats } from "@/entities/guard/guard-stats-api";
import { OperationsError } from "@/shared/ui/operations";

/** Signal humanized descriptions and iconography matching backend canonical tokens */
const SIGNAL_PROFILES: Record<
	string,
	{ title: string; desc: string; icon: typeof Zap; tone: string; border: string }
> = {
	missing_thinking: {
		title: "思考特征缺失 (降智拦截)",
		desc: "模型响应未产出有效思维链或直接给出劣质输出，判定为降智污染并即时阻断，自动切换纯净出口重试",
		icon: Sparkles,
		tone: "text-amber-500 bg-amber-500/10",
		border: "border-amber-500/30",
	},
	created_timeout: {
		title: "首字握手超时 (首事件截止)",
		desc: "上游服务首包 Token 响应严重超时，触发早退保护并自动切换备选可用出口重试",
		icon: Clock,
		tone: "text-blue-500 bg-blue-500/10",
		border: "border-blue-500/30",
	},
	evidence_timeout: {
		title: "零证据等待截止",
		desc: "流式交互过程中持续未获取到有效思考证据，达到零证据安全截止时限，自动触发安全重试",
		icon: AlertTriangle,
		tone: "text-orange-500 bg-orange-500/10",
		border: "border-orange-500/30",
	},
	empty_stream: {
		title: "空响应流断开 (空流拦截)",
		desc: "上游流式传输过早关闭且无任何生成内容，触发流量保全并自动重试调度",
		icon: AlertCircle,
		tone: "text-purple-500 bg-purple-500/10",
		border: "border-purple-500/30",
	},
	traffic_degraded: {
		title: "智能降智拦截",
		desc: "捕获到输出文本降智与短路特征，即时执行流量熔断与健康出口重试",
		icon: Sparkles,
		tone: "text-amber-500 bg-amber-500/10",
		border: "border-amber-500/30",
	},
	upstream_http: {
		title: "上游服务异常",
		desc: "上游返回非预期的 HTTP 5xx 或连接重置，即时隔离该物理通路并执行故障转移",
		icon: Server,
		tone: "text-rose-500 bg-rose-500/10",
		border: "border-rose-500/30",
	},
	upstream_stream_empty: {
		title: "流式传输过早关闭",
		desc: "上游流式连接过早异常终止且无有效生成内容，触发流量保全并紧急调度重试",
		icon: AlertCircle,
		tone: "text-purple-500 bg-purple-500/10",
		border: "border-purple-500/30",
	},
	quality_evidence_timeout: {
		title: "证据收集超时",
		desc: "涉案路径对照测试未在预定时限内完成判定，自动执行安全保护并恢复业务",
		icon: AlertTriangle,
		tone: "text-orange-500 bg-orange-500/10",
		border: "border-orange-500/30",
	},
};

const DEFAULT_SIGNAL_PROFILE = {
	title: "未知防御信号",
	desc: "守卫规则捕获的异常特征信号，触发调度隔离与流量重试保护",
	icon: ShieldAlert,
	tone: "text-primary bg-primary/10",
	border: "border-border/80",
};

/** Format model string with its channel prefix (Build/grok-4.5, Console/grok-4.5, Web/grok-4.5) */
function formatGuardedModel(rawModel: string): { channel: string; modelName: string; fullDisplay: string } {
	if (rawModel.includes(":")) {
		const [scope, ...rest] = rawModel.split(":");
		const name = rest.join(":");
		let channel = scope;
		if (scope === "grok_build" || scope === "build") channel = "Build";
		else if (scope === "grok_console" || scope === "console") channel = "Console";
		else if (scope === "grok_web" || scope === "web") channel = "Web";
		return { channel, modelName: name, fullDisplay: `${channel}/${name}` };
	}
	return { channel: "", modelName: rawModel, fullDisplay: rawModel };
}

export function QualityHitStats() {
	const { t, i18n } = useTranslation();
	const isZh = i18n.language.startsWith("zh");

	const query = useQuery({
		queryKey: ["guard-stats"],
		queryFn: getGuardStats,
		refetchInterval: 10_000,
	});

	const stats = query.data;

	// Aggregated metrics
	const summary = useMemo(() => {
		if (!stats) return null;
		const totalTriggered = stats.signals.reduce((acc, s) => acc + s.triggered, 0);
		const totalRequests = stats.signals.reduce((acc, s) => acc + s.requests, 0);
		const totalRescued = stats.signals.reduce((acc, s) => acc + s.rescued, 0);
		const totalFailed = stats.signals.reduce((acc, s) => acc + s.failed, 0);
		const totalSettled = totalRescued + totalFailed;
		const recoveryRate = totalSettled > 0 ? Math.round((totalRescued / totalSettled) * 100) : 100;
		const exemptions = (stats.exempts ?? []).filter((e) => e.count > 0);
		const totalExemptions = exemptions.reduce((acc, e) => acc + e.count, 0);

		return {
			totalTriggered,
			totalRequests,
			totalRescued,
			totalFailed,
			recoveryRate,
			exemptions,
			totalExemptions,
			exhausted: stats.retrial?.exhaustedRejected ?? totalFailed,
		};
	}, [stats]);

	if (query.isPending) {
		return (
			<div className="flex h-64 flex-col items-center justify-center gap-3">
				<Spinner className="size-6 text-primary" />
				<p className="text-xs text-muted-foreground">{t("common.loading")}</p>
			</div>
		);
	}

	if (query.isError) {
		return (
			<div className="p-4">
				<OperationsError
					message={query.error.message}
					retry={() => void query.refetch()}
				/>
			</div>
		);
	}

	if (!stats || !summary) return null;

	const hasSignals = stats.signals.length > 0 && summary.totalTriggered > 0;
	const formattedSince = stats.since ? new Date(stats.since).toLocaleString() : "";

	return (
		<div className="flex flex-col gap-5 min-w-0 pb-2">
			{/* Top 4 Defense Telemetry Velocity Cards */}
			<div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
				{/* Card 1: Total Intercepts */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<ShieldAlert className="size-4 text-indigo-500" />
							<span>{isZh ? "阻击降智总数" : "Total Intercepts"}</span>
						</span>
						<Badge variant="outline" className="text-[10px] font-semibold text-indigo-500 border-indigo-500/20 bg-indigo-500/5">
							{isZh ? "拦截保护" : "Shielded"}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{summary.totalTriggered}
						</span>
						<span className="text-xs text-muted-foreground">
							{isZh ? `涉及 ${summary.totalRequests} 次请求` : `${summary.totalRequests} reqs`}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80">
						{isZh ? "上游降智特征与异常流量即时截断" : "Real-time anomaly interception"}
					</p>
				</div>

				{/* Card 2: Recovery Rate */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<Activity className="size-4 text-emerald-500" />
							<span>{isZh ? "无感挽救成功率" : "Recovery Rate"}</span>
						</span>
						<Badge
							variant="outline"
							className={cn(
								"text-[10px] font-semibold",
								summary.recoveryRate >= 90
									? "text-emerald-600 dark:text-emerald-400 border-emerald-500/30 bg-emerald-500/5"
									: summary.recoveryRate >= 70
										? "text-amber-600 dark:text-amber-400 border-amber-500/30 bg-amber-500/5"
										: "text-rose-600 dark:text-rose-400 border-rose-500/30 bg-rose-500/5"
							)}
						>
							{summary.recoveryRate >= 90 ? (isZh ? "高效护航" : "Optimal") : (isZh ? "需关注" : "Warning")}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-1.5">
						<span className={cn(
							"text-3xl font-black tracking-tight tabular-nums",
							summary.recoveryRate >= 90 ? "text-emerald-600 dark:text-emerald-400" : "text-amber-600 dark:text-amber-400"
						)}>
							{summary.recoveryRate}%
						</span>
						<span className="text-xs text-muted-foreground font-medium">
							{isZh ? `(${summary.totalRescued} 次已救回)` : `(${summary.totalRescued} saved)`}
						</span>
					</div>
					<div className="mt-2.5 h-1.5 w-full overflow-hidden rounded-full bg-muted">
						<div
							className="h-full rounded-full bg-emerald-500 transition-all duration-700"
							style={{ width: `${summary.recoveryRate}%` }}
						/>
					</div>
				</div>

				{/* Card 3: Terminal Circuit Breaker */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<AlertTriangle className="size-4 text-rose-500" />
							<span>{isZh ? "熔断止损拦截" : "Circuit Breaker"}</span>
						</span>
						<Badge variant="outline" className="text-[10px] font-semibold text-rose-600 dark:text-rose-400 border-rose-500/20 bg-rose-500/5">
							{isZh ? "会话保全" : "Guarded"}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{summary.exhausted}
						</span>
						<span className="text-xs text-muted-foreground">
							{isZh ? "次超限熔断" : "cutoffs"}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80">
						{isZh ? "重试用尽后拒绝，防止污染下游会话" : "Prevent downstream context pollution"}
					</p>
				</div>

				{/* Card 4: Defense Scope & Policy */}
				<div className="group relative overflow-hidden rounded-xl border border-border/80 bg-card/75 p-4 shadow-sm backdrop-blur-sm transition-all hover:border-border hover:shadow-md">
					<div className="flex items-center justify-between text-xs text-muted-foreground">
						<span className="flex items-center gap-1.5 font-medium">
							<CheckCircle2 className="size-4 text-sky-500" />
							<span>{isZh ? "防线覆盖范围" : "Guarded Scope"}</span>
						</span>
						<Badge variant="outline" className="text-[10px] font-semibold text-sky-600 dark:text-sky-400 border-sky-500/20 bg-sky-500/5">
							{stats.effective?.enabled ? (isZh ? "全时在线" : "Active") : (isZh ? "未开启" : "Inactive")}
						</Badge>
					</div>
					<div className="mt-2.5 flex items-baseline gap-2">
						<span className="text-3xl font-black tracking-tight tabular-nums text-foreground">
							{stats.effective?.guardedModels?.length ?? 0}
						</span>
						<span className="text-xs text-muted-foreground">
							{isZh ? "个模型纳入守护" : "models"}
						</span>
					</div>
					<p className="mt-2 text-[11px] text-muted-foreground/80 truncate" title={stats.effective?.onExhausted}>
						{isZh ? `最大重试 ${stats.effective?.maxAttempts ?? 3} 次 · 熔断策略: ${stats.effective?.onExhausted ?? "fail_closed"}` : `Max ${stats.effective?.maxAttempts ?? 3} retries`}
					</p>
				</div>
			</div>

			{/* Middle Section: Defense Signals Matrix */}
			<div className="rounded-xl border border-border/80 bg-card/60 p-5 shadow-sm backdrop-blur-sm">
				<div className="flex flex-wrap items-center justify-between gap-3 border-b border-border/70 pb-4">
					<div>
						<div className="flex items-center gap-2">
							<h2 className="text-base font-bold text-foreground">
								{isZh ? "防御特征信号矩阵" : "Signal Defense Matrix"}
							</h2>
							<span className="inline-flex items-center gap-1 rounded-full bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-600 dark:text-emerald-400">
								<span className="size-1.5 rounded-full bg-emerald-500 animate-ping" />
								{isZh ? "实时防线生效中" : "Live Shield Active"}
							</span>
						</div>
						<p className="text-xs text-muted-foreground mt-1">
							{isZh
								? `自本次服务进程启动（${formattedSince || "当前"}）以来捕获的降智响应与错误拦截详情`
								: "Interception telemetry recorded since process startup"}
						</p>
					</div>

					<span className="text-xs text-muted-foreground font-mono">
						{stats.signals.length} {isZh ? "个特征信号规则" : "signal rules"}
					</span>
				</div>

				{!hasSignals ? (
					<div className="my-8 flex flex-col items-center justify-center gap-2 text-center">
						<div className="flex size-12 items-center justify-center rounded-2xl bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 ring-1 ring-emerald-500/20">
							<ShieldCheck className="size-6" />
						</div>
						<h3 className="text-sm font-bold text-foreground mt-1">
							{isZh ? "暂无异常响应拦截" : "No Anomaly Signals Intercepted"}
						</h3>
						<p className="max-w-md text-xs text-muted-foreground">
							{isZh
								? "当前所有上游推理会话表现纯净稳定，未触发降智、首字超时或异常流式中断。守卫已全天候就绪监听。"
								: "All current inference sessions are running cleanly with no degradation detected."}
						</p>
					</div>
				) : (
					<div className="mt-4 grid grid-cols-1 gap-3.5 md:grid-cols-2">
						{stats.signals.map((sig) => {
							const profile = SIGNAL_PROFILES[sig.signal] ?? DEFAULT_SIGNAL_PROFILE;
							const SigIcon = profile.icon;
							const settled = sig.rescued + sig.failed;
							const ratio = settled > 0 ? Math.round((sig.rescued / settled) * 100) : 100;
							const hasTriggered = sig.triggered > 0;

							return (
								<div
									key={sig.signal}
									className={cn(
										"flex flex-col justify-between rounded-xl border p-4 transition-all hover:shadow-sm",
										hasTriggered
											? "border-border/90 bg-card/90"
											: "border-border/50 bg-muted/20 opacity-70"
									)}
								>
									<div>
										{/* Signal Title & Badge */}
										<div className="flex items-start justify-between gap-2">
											<div className="flex items-center gap-2.5">
												<div className={cn("flex size-8 shrink-0 items-center justify-center rounded-lg ring-1 ring-border/60", profile.tone)}>
													<SigIcon className="size-4" />
												</div>
												<div>
													<h4 className="text-sm font-bold text-foreground">
														{profile.title}
													</h4>
													<p className="font-mono text-[10px] text-muted-foreground">
														{sig.signal}
													</p>
												</div>
											</div>

											<Badge
												variant={hasTriggered ? "default" : "secondary"}
												className="font-mono text-[11px] tabular-nums"
											>
												{isZh ? `触发 ${sig.triggered} 次` : `${sig.triggered} hits`}
											</Badge>
										</div>

										{/* Strategy Description */}
										<p className="mt-3 text-xs text-muted-foreground leading-relaxed">
											{profile.desc}
										</p>
									</div>

									{/* Metrics & Progress Bar */}
									<div className="mt-4 border-t border-border/60 pt-3">
										<div className="flex items-center justify-between text-xs">
											<div className="flex items-center gap-3">
												<span className="flex items-center gap-1.5 text-emerald-600 dark:text-emerald-400 font-medium">
													<span className="size-1.5 rounded-full bg-emerald-500" />
													{isZh ? "成功挽救" : "Rescued"}: <strong className="tabular-nums font-bold">{sig.rescued}</strong>
												</span>
												<span className="flex items-center gap-1.5 text-rose-500 font-medium">
													<span className="size-1.5 rounded-full bg-rose-500" />
													{isZh ? "熔断阻断" : "Failed"}: <strong className="tabular-nums font-bold">{sig.failed}</strong>
												</span>
											</div>

											<span className="font-mono font-bold text-foreground tabular-nums">
												{ratio}% {isZh ? "挽救率" : "saved"}
											</span>
										</div>

										<div className="mt-2 flex h-2 w-full overflow-hidden rounded-full bg-muted">
											<div
												className="h-full bg-emerald-500 transition-all duration-500"
												style={{ width: `${ratio}%` }}
											/>
											<div
												className="h-full bg-rose-500 transition-all duration-500"
												style={{ width: `${100 - ratio}%` }}
											/>
										</div>

										<div className="mt-2 flex items-center justify-between text-[10px] text-muted-foreground">
											<span>{isZh ? `累计截获 ${sig.requests} 次请求` : `${sig.requests} requests`}</span>
											<span>
												{sig.lastSeen ? (isZh ? `最近一次: ${new Date(sig.lastSeen).toLocaleTimeString()}` : new Date(sig.lastSeen).toLocaleTimeString()) : (isZh ? "暂无拦截" : "None")}
											</span>
										</div>
									</div>
								</div>
							);
						})}
					</div>
				)}
			</div>

			{/* Bottom Grid: Exemptions Transparency & Guarded Models Deck */}
			<div className="grid grid-cols-1 gap-5 lg:grid-cols-[1.2fr_1fr]">
				{/* Block 1: Exemptions Transparency */}
				<div className="rounded-xl border border-border/80 bg-card/60 p-5 shadow-sm backdrop-blur-sm">
					<div className="flex items-center gap-2 border-b border-border/70 pb-3">
						<Bot className="size-4 text-primary" />
						<h3 className="text-sm font-bold text-foreground">
							{isZh ? "守护豁免与绕行透视 (Exemptions)" : "Guard Exemptions"}
						</h3>
					</div>
					<p className="mt-2 text-xs text-muted-foreground leading-relaxed">
						{isZh
							? "以下请求因协议特性、模型未支持推理或显式配置绕行，未经过守卫干预直接透传："
							: "Requests that bypassed guard intervention due to protocol or model scope:"}
					</p>

					{summary.exemptions.length === 0 ? (
						<div className="my-6 flex items-center gap-3 rounded-lg border border-border/60 bg-muted/30 p-3.5 text-xs text-muted-foreground">
							<CheckCircle2 className="size-4 text-emerald-500 shrink-0" />
							<span>{isZh ? "当前没有记录到任何豁免流量，所有目标模型请求均处于守护监听下。" : "No exempted requests recorded."}</span>
						</div>
					) : (
						<div className="mt-3 divide-y divide-border/60">
							{summary.exemptions.map((e) => (
								<div key={e.reason} className="flex items-center justify-between py-2.5 text-xs">
									<div className="flex items-center gap-2 min-w-0">
										<span className="size-1.5 rounded-full bg-muted-foreground/60 shrink-0" />
										<span className="truncate text-foreground font-medium">
											{t(`settings.guardStats.exempts.${e.reason}`, { defaultValue: e.reason })}
										</span>
									</div>
									<Badge variant="secondary" className="font-mono text-[11px] tabular-nums shrink-0">
										{e.count} {isZh ? "次" : "hits"}
									</Badge>
								</div>
							))}
						</div>
					)}
				</div>

				{/* Block 2: Guarded Model Fleet */}
				<div className="rounded-xl border border-border/80 bg-card/60 p-5 shadow-sm backdrop-blur-sm">
					<div className="flex items-center gap-2 border-b border-border/70 pb-3">
						<Cpu className="size-4 text-sky-500" />
						<h3 className="text-sm font-bold text-foreground">
							{isZh ? "受守护核心模型名单" : "Guarded Models Fleet"}
						</h3>
					</div>
					<p className="mt-2 text-xs text-muted-foreground leading-relaxed">
						{isZh
							? "以下模型的思考链、结构化输出与推理流受到质量守卫的全天候实时保护："
							: "Inference traffic for the following models is actively protected:"}
					</p>

					<div className="mt-4 flex flex-wrap gap-2">
						{stats.effective?.guardedModels && stats.effective.guardedModels.length > 0 ? (
							stats.effective.guardedModels.map((rawModel) => {
								const formatted = formatGuardedModel(rawModel);
								return (
									<span
										key={rawModel}
										className="inline-flex items-center gap-1.5 rounded-lg border border-primary/25 bg-card px-2.5 py-1 text-xs font-mono font-medium shadow-2xs hover:border-primary/50 transition-colors"
									>
										<span className="size-1.5 rounded-full bg-emerald-500" />
										{formatted.channel ? (
											<span className="flex items-center gap-1">
												<span className="text-primary font-bold">{formatted.channel}</span>
												<span className="text-muted-foreground/60">/</span>
												<span className="text-foreground font-semibold">{formatted.modelName}</span>
											</span>
										) : (
											<span className="text-foreground font-semibold">{formatted.modelName}</span>
										)}
									</span>
								);
							})
						) : (
							<span className="text-xs text-muted-foreground">{isZh ? "全量模型自动覆盖" : "All models covered"}</span>
						)}
					</div>
				</div>
			</div>
		</div>
	);
}
