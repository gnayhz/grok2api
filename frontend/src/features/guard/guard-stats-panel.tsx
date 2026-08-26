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
  const { t } = useTranslation();
  const statsQuery = useQuery({ queryKey: ["guard-stats"], queryFn: getGuardStats, refetchInterval: 10_000 });

  if (statsQuery.isPending) {
    return <div className="flex min-h-24 items-center justify-center"><Spinner /></div>;
  }
  if (statsQuery.isError) {
    return <p className="px-1 text-sm text-muted-foreground">{statsQuery.error.message}</p>;
  }
  const stats = statsQuery.data;

  if (tab === "egressRotation") {
    // 固定结论行常显(与特征表同构):已知结论零值显示 0,后端新增的
    // 未知结论追加在后面——不做空态占位。
    const knownOutcomes = ["clean", "degraded", "no_account", "unconfigured", "error"];
    const seen = new Set(knownOutcomes);
    const extra = Object.keys(stats.canary).filter((k) => !seen.has(k));
    const outcomes = [...knownOutcomes, ...extra];
    return (
      <Table>
        <TableHeader><TableRow className="hover:bg-transparent"><TableHead className="h-9 text-sm font-medium text-foreground">{t("settings.guardStats.canaryOutcome")}</TableHead><TableHead className="h-9 text-sm font-medium text-right text-foreground">{t("settings.guardStats.count")}</TableHead></TableRow></TableHeader>
        <TableBody>
          {outcomes.map((outcome) => (
            <TableRow key={outcome}>
              <TableCell className="py-2.5 text-sm font-medium">{t("settings.guardStats.canary." + outcome, { defaultValue: outcome })}</TableCell>
              <TableCell className={cn(num, "font-semibold")}>{stats.canary[outcome] ?? 0}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
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
      <div className="grid gap-1 px-1 pb-1 text-sm text-muted-foreground sm:grid-cols-2">
        <span>{t("settings.guardStats.sameAccountRetry", { used: stats.retrial.sameAccountRetryUsed, rescued: stats.retrial.sameAccountRetryRescued })}</span>
        <span>{t("settings.guardStats.exhausted", { deliverLast: stats.retrial.exhaustedDeliverLast, rejected: stats.retrial.exhaustedRejected })}</span>
      </div>
    </div>
  );
}
