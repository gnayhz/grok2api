import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";

import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { getGuardStats } from "@/features/guard/guard-stats-api";

const signalKey = (signal: string): string => "settings.guardStats.signals." + signal;

function percent(rescued: number, total: number): string {
  if (total <= 0) return "—";
  return Math.round((rescued / total) * 100) + "%";
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
    return <p className="px-1 text-xs text-muted-foreground">{statsQuery.error.message}</p>;
  }
  const stats = statsQuery.data;
  const hasActivity =
    stats.signals.some((signal) => signal.triggered > 0)
    || Object.keys(stats.canary).length > 0
    || stats.retrial.sameAccountRetryUsed > 0
    || stats.retrial.exhaustedDeliverLast > 0
    || stats.retrial.exhaustedRejected > 0;
  if (!hasActivity) {
    return <p className="px-1 text-xs text-muted-foreground">{t("settings.guardStats.empty")}</p>;
  }

  if (tab === "egressRotation") {
    const canaryEntries = Object.entries(stats.canary);
    return (
      <Table>
        <TableHeader><TableRow><TableHead className="h-8 text-xs">{t("settings.guardStats.canaryOutcome")}</TableHead><TableHead className="h-8 text-xs text-right">{t("settings.guardStats.count")}</TableHead></TableRow></TableHeader>
        <TableBody>
          {canaryEntries.length === 0 ? (
            <TableRow><TableCell className="py-3 text-xs text-muted-foreground" colSpan={2}>{t("settings.guardStats.empty")}</TableCell></TableRow>
          ) : canaryEntries.map(([outcome, count]) => (
            <TableRow key={outcome}>
              <TableCell className="py-2 text-xs">{t("settings.guardStats.canary." + outcome, { defaultValue: outcome })}</TableCell>
              <TableCell className="py-2 text-right font-mono text-xs">{count}</TableCell>
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
          <TableRow>
            <TableHead className="h-8 text-xs">{t("settings.guardStats.signal")}</TableHead>
            <TableHead className="h-8 text-xs text-right">{t("settings.guardStats.triggered")}</TableHead>
            <TableHead className="h-8 text-xs text-right">{t("settings.guardStats.requests")}</TableHead>
            <TableHead className="h-8 text-xs text-right">{t("settings.guardStats.rescued")}</TableHead>
            <TableHead className="h-8 text-xs text-right">{t("settings.guardStats.failed")}</TableHead>
            <TableHead className="h-8 text-xs text-right">{t("settings.guardStats.rescueRate")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {stats.signals.map((signal) => (
            <TableRow key={signal.signal}>
              <TableCell className="py-2 text-xs">{t(signalKey(signal.signal), { defaultValue: signal.signal })}</TableCell>
              <TableCell className="py-2 text-right font-mono text-xs">{signal.triggered}</TableCell>
              <TableCell className="py-2 text-right font-mono text-xs">{signal.requests}</TableCell>
              <TableCell className="py-2 text-right font-mono text-xs text-emerald-600">{signal.rescued}</TableCell>
              <TableCell className="py-2 text-right font-mono text-xs text-destructive">{signal.failed}</TableCell>
              <TableCell className="py-2 text-right font-mono text-xs">{percent(signal.rescued, signal.rescued + signal.failed)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <div className="grid gap-1 px-1 pb-1 text-xs text-muted-foreground sm:grid-cols-2">
        <span>{t("settings.guardStats.sameAccountRetry", { used: stats.retrial.sameAccountRetryUsed, rescued: stats.retrial.sameAccountRetryRescued })}</span>
        <span>{t("settings.guardStats.exhausted", { deliverLast: stats.retrial.exhaustedDeliverLast, rejected: stats.retrial.exhaustedRejected })}</span>
      </div>
    </div>
  );
}
