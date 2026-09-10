import { Check, CircleHelp, X } from "lucide-react";
import { useTranslation } from "react-i18next";

import { cn } from "@/shared/lib/cn";
import { auditResult } from "./audit-presentation";
import type { AuditDTO } from "./request-audits-api";

export function AuditResultBadge({ audit, className }: {
  audit: Pick<AuditDTO, "statusCode" | "errorCode">;
  className?: string;
}) {
  const { t } = useTranslation();
  const result = auditResult(audit);
  const Icon = result === "success" ? Check : result === "failed" ? X : CircleHelp;
  return (
    <span className={cn(
      "inline-flex shrink-0 items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium leading-4",
      result === "success" && "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
      result === "failed" && "bg-red-500/10 text-red-700 dark:text-red-300",
      result === "unknown" && "bg-muted text-muted-foreground",
      className,
    )}>
      <Icon className="size-3.5" aria-hidden="true" />
      {t("audits.results." + result)}
    </span>
  );
}
