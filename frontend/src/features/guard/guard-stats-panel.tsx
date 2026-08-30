import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";

import { cn } from "@/shared/lib/cn";

import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { getGuardStats } from "@/features/guard/guard-stats-api";

const signalKey = (signal: string): string => "settings.guardStats.signals." + signal;

function percent(rescued: number, total: number): string {
  if (total <= 0) return "—";
  return Math.round((rescued / total) * 100) + "%";
}

// 数字单元格：等宽字体保证对齐，tabular-nums 保证 0-9 等宽跳动不抖动。
const num = "text-right font-mono text-sm tabular-nums";

// 救回率配色：>=80% 绿（该特征拦截后几乎总能救回）,>=50% 黄（勉强）,
// <50% 红（拦截了也多半失败）;无数据灰。
function rateClass(rate: string): string {
  if (rate === "—") return "text-muted-foreground";
  const value = Number.parseInt(rate, 10);
  if (value >= 80) return "text-emerald-600 dark:text-emerald-400";
  if (value >= 50) return "text-amber-600 dark:text-amber-400";
  return "text-destructive";
}

// GuardStatsPanel 展示每个防降智特征的量化观测:触发次数与请求级结局
// (救回/失败),救回率即"该规则拦截降智后恢复请求的能力"。计数自进程
// 启动累计,重启归零;每 10 秒自动刷新。
export function GuardStatsPanel({ tab }: { tab: "requestRetry" | "egressRotation" }) {
  const { t, i18n } = useTranslation();
  const statsQuery = useQuery({ queryKey: ["guard-stats"], queryFn: getGuardStats, refetchInterval: 10_000 });
  const timeFormatter = new Intl.DateTimeFormat(i18n.language, { hour: "2-digit", minute: "2-digit", second: "2-digit" });

  if (statsQuery.isPending) {
    return <div className="flex min-h-24 items-center justify-center"><Spinner /></div>;
  }
  if (statsQuery.isError) {
    return <p className="px-1 text-sm text-muted-foreground">{statsQuery.error.message}</p>;
  }
  const stats = statsQuery.data;

  if (tab === "egressRotation") {
    // 固定结论块常显（与豁免台账同构的统计块，每行两个）：已知结论零值
    // 显示 0，后端新增的未知结论追加在后面——不做空态占位。数字按结论
    // 语义着色：clean 绿 / degraded 红，其余中性。
    const knownOutcomes = ["clean", "degraded", "no_account", "unconfigured", "error"];
    const seen = new Set(knownOutcomes);
    const extra = Object.keys(stats.canary).filter((k) => !seen.has(k));
    const outcomes = [...knownOutcomes, ...extra];
    const outcomeCountClass: Record<string, string> = {
      clean: "text-emerald-600 dark:text-emerald-400",
      degraded: "text-destructive",
    };
    return (
      <div className="grid grid-cols-1 gap-2 pb-1 sm:grid-cols-2">
        {outcomes.map((outcome) => (
          <div key={outcome} className="flex items-center justify-between gap-3 rounded-md border bg-muted/30 px-3 py-2">
            <div className="min-w-0 truncate text-sm font-medium">{t("settings.guardStats.canary." + outcome, { defaultValue: outcome })}</div>
            <span className={cn(num, "text-base font-semibold", outcomeCountClass[outcome])}>{stats.canary[outcome] ?? 0}</span>
          </div>
        ))}
      </div>
    );
  }

  return (
    <div className="space-y-2">
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="h-9 text-sm font-medium text-foreground">{t("settings.guardStats.signal")}</TableHead>
            <TableHead className="h-9 text-sm font-medium text-right text-foreground">{t("settings.guardStats.triggered")}</TableHead>
            <TableHead className="h-9 text-sm font-medium text-right text-foreground">{t("settings.guardStats.requests")}</TableHead>
            <TableHead className="h-9 text-sm font-medium text-right text-emerald-600 dark:text-emerald-400">{t("settings.guardStats.rescued")}</TableHead>
            <TableHead className="h-9 text-sm font-medium text-right text-destructive">{t("settings.guardStats.failed")}</TableHead>
            <TableHead className="h-9 text-sm font-medium text-right text-foreground">{t("settings.guardStats.rescueRate")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {stats.signals.map((signal) => {
            const rate = percent(signal.rescued, signal.rescued + signal.failed);
            return (
            <TableRow key={signal.signal}>
              <TableCell className="py-2.5 text-sm font-medium">
                <Badge variant="secondary" className="mr-2 font-mono text-[11px] tabular-nums">{signal.triggered}</Badge>
                {t(signalKey(signal.signal), { defaultValue: signal.signal })}
              </TableCell>
              <TableCell className={cn(num, "font-semibold")}>{signal.triggered}</TableCell>
              <TableCell className={num}>{signal.requests}</TableCell>
              <TableCell className={cn(num, "font-semibold text-emerald-600 dark:text-emerald-400")}>{signal.rescued}</TableCell>
              <TableCell className={cn(num, "font-semibold text-destructive")}>{signal.failed}</TableCell>
              <TableCell className={cn(num, "font-semibold", rateClass(rate))}>{rate}</TableCell>
            </TableRow>
          );
          })}
        </TableBody>
      </Table>
      {(stats.exempts ?? []).length > 0 ? (
        <div className="space-y-2">
          {/* 豁免台账：短口径统计块（每行两个，代码库统计块的既有样式），
              上方特征表是多列数据仍用 Table——两者按信息密度各取所长。 */}
          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 px-1">
            <span className="text-sm font-medium text-foreground">{t("settings.guardStats.exemptsTitle")}</span>
            <span className="text-xs text-muted-foreground">{t("settings.guardStats.exemptsHelp")}</span>
          </div>
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {(stats.exempts ?? []).map((exempt) => (
              <div key={exempt.reason} className="flex items-center justify-between gap-3 rounded-md border bg-muted/30 px-3 py-2">
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium">{t("settings.guardStats.exempts." + exempt.reason, { defaultValue: exempt.reason })}</div>
                  {exempt.lastSeen ? <div className="text-xs text-muted-foreground">{timeFormatter.format(new Date(exempt.lastSeen))}</div> : null}
                </div>
                <span className={cn(num, "text-base font-semibold")}>{exempt.count}</span>
              </div>
            ))}
          </div>
        </div>
      ) : null}
      {/* 恢复路径汇总：与豁免统计块同构；右侧大数字取“结局”计数
          （同号重试=最终成功，耗尽=拒绝），明细行携带全量数字。 */}
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        <div className="flex items-center justify-between gap-3 rounded-md border bg-muted/30 px-3 py-2">
          <div className="min-w-0">
            <div className="truncate text-sm font-medium">{t("settings.guardStats.sameAccountRetryTitle")}</div>
            <div className="text-xs text-muted-foreground">{t("settings.guardStats.sameAccountRetryDetail", { used: stats.retrial.sameAccountRetryUsed, rescued: stats.retrial.sameAccountRetryRescued })}</div>
          </div>
          <span className={cn(num, "text-base font-semibold text-emerald-600 dark:text-emerald-400")}>{stats.retrial.sameAccountRetryRescued}</span>
        </div>
        <div className="flex items-center justify-between gap-3 rounded-md border bg-muted/30 px-3 py-2">
          <div className="min-w-0">
            <div className="truncate text-sm font-medium">{t("settings.guardStats.exhaustedTitle")}</div>
            <div className="text-xs text-muted-foreground">{t("settings.guardStats.exhaustedDetail", { deliverLast: stats.retrial.exhaustedDeliverLast, rejected: stats.retrial.exhaustedRejected })}</div>
          </div>
          <span className={cn(num, "text-base font-semibold text-destructive")}>{stats.retrial.exhaustedRejected}</span>
        </div>
      </div>
      {stats.since ? (
        <p className="px-1 pb-1 text-xs text-muted-foreground">{t("settings.guardStats.since", { time: timeFormatter.format(new Date(stats.since)) })}</p>
      ) : null}
    </div>
  );
}
