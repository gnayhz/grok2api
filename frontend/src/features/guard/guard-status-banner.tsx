import { useQuery } from "@tanstack/react-query";
import { ShieldAlert } from "lucide-react";
import { useTranslation } from "react-i18next";

import { getGuardStats } from "@/features/guard/guard-stats-api";

// 守卫离场红色横幅:仅在守卫关闭时渲染,健康状态返回 null(静默)。
// 历史事故:持久化运行时设置静默覆盖文件配置把守卫关闭,面板此前
// 无处可见该状态,降智请求整批漏放后才被发现。
export function GuardStatusBanner() {
  const { t } = useTranslation();
  const statsQuery = useQuery({ queryKey: ["guard-stats"], queryFn: getGuardStats, refetchInterval: 30_000 });
  const effective = statsQuery.data?.effective;
  if (!effective || effective.enabled) {
    return null;
  }
  return (
    <section className="rounded-lg border border-destructive/40 bg-destructive/10 px-4 py-3">
      <div className="flex items-start gap-3">
        <ShieldAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
        <div className="min-w-0 space-y-1">
          <p className="text-sm font-medium text-destructive">{t("settings.guardStats.bannerDisabledTitle")}</p>
          <p className="text-xs text-muted-foreground">{t("settings.guardStats.bannerDisabledDescription")}</p>
        </div>
      </div>
    </section>
  );
}
