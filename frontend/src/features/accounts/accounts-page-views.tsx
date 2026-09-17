import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { X } from "lucide-react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { clearAccountCooldown } from "@/entities/account/account-api";
import type { AccountDTO, QuotaDTO } from "@/entities/account/account-api";
import { ApiError } from "@/shared/api/client";
import { formatDateTime } from "@/shared/lib/format";
import { toast } from "sonner";
import { cn } from "@/shared/lib/cn";
import { Badge } from "@/shared/ui/badge";
import { Spinner } from "@/shared/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";

// accounts 页展示组件(ui 层):指标面板、账号类型/状态徽章、
// 冷却按钮与提示容器。状态与命令经命名 props 传入,不持有生命周期。
export function AccountMetricPanel({ icon, label, value, detail, detailItems, loading, tone }: {
  icon: ReactNode;
  label: string;
  value: string;
  detail: string;
  detailItems?: Array<{ label: string; value: string; tone?: string }>;
  loading: boolean;
  tone: string;
}) {
  return (
    <div className="min-h-28 rounded-lg bg-card p-4" aria-busy={loading}>
      <div className="flex min-h-5 items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className={cn("flex size-5 items-center justify-center [&_svg]:size-4", tone)}>{icon}</span>
      </div>
      <div className="mt-3 flex min-h-8 items-center text-2xl font-medium tracking-tight tabular-nums">{loading ? <Spinner /> : value}</div>
      {detailItems ? (
        <div className={cn("-ml-1.5 mt-1.5 flex min-h-5 flex-wrap gap-1 text-[11px] leading-4", loading && "invisible")} title={detail}>
          {detailItems.map((item) => (
            <span key={item.label} className={cn("inline-flex shrink-0 items-baseline gap-1 whitespace-nowrap rounded-md px-1.5 py-0.5", item.tone ?? "bg-muted text-muted-foreground")}>
              <span>{item.label}</span>
              {item.value ? <span className="font-medium tabular-nums">{item.value}</span> : null}
            </span>
          ))}
        </div>
      ) : (
        <p className={cn("mt-1.5 min-h-4 truncate text-[11px] text-muted-foreground", loading && "invisible")} title={detail}>{detail}</p>
      )}
    </div>
  );
}

export function WebAccountType({ tier }: { tier?: AccountDTO["webTier"] }) {
  const { t } = useTranslation();
  const label = tier === "basic" ? t("accountType.free") : tier === "super" ? t("accountType.super") : tier === "heavy" ? t("accountType.heavy") : t("accountType.auto");
  return <AccountTypeText label={label} variant={tier === "basic" ? "free" : "default"} />;
}

export function AccountType({ quota }: { quota: QuotaDTO }) {
  const { t } = useTranslation();
  if (quota.type === "unknown") {
    return <AccountTypeText label={t("accountType.pending")} title={t("accountType.pendingDescription")} variant="muted" />;
  }

  const isFree = quota.type === "free";
  const label = isFree ? t("accountType.free") : t("accountType.paid");
  return <AccountTypeText label={label} variant={isFree ? "free" : "default"} />;
}

export function AccountTypeText({ label, title, variant }: { label: string; title?: string; variant: "default" | "free" | "muted" }) {
  if (variant === "muted") {
    return <span title={title ?? label} className="text-xs text-muted-foreground">{label}</span>;
  }
  return <span title={title ?? label} className={cn("max-w-32 truncate text-xs font-medium", variant === "free" ? "text-emerald-700 dark:text-emerald-300" : "text-primary")}>{label}</span>;
}

export function AccountStatus({ account }: { account: AccountDTO }) {
  const { t, i18n } = useTranslation();
  if (!account.enabled) {
    return <Badge variant="outline" className="text-muted-foreground">{t("accounts.statusDisabled")}</Badge>;
  }
  if (account.authStatus === "reauthRequired") {
    const refreshErrorDetails = formatAdditionalRefreshErrorDetails(account);
    const hasRefreshError = Boolean(account.authError || account.lastRefreshErrorStatus || account.lastRefreshErrorCode || account.lastRefreshErrorMessage || refreshErrorDetails);
    if (!hasRefreshError) return <Badge variant="destructive">{t("accounts.statusReauthRequired")}</Badge>;
    return (
      <StatusTooltip content={(
        <div className="grid w-72 max-w-[calc(100vw-2rem)] grid-cols-[4.5rem_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs font-normal leading-5">
          {account.authError ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorMessage")}</span><span className="break-words">{account.authError}</span></> : null}
          {account.lastRefreshErrorStatus ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorStatus")}</span><span>{account.lastRefreshErrorStatus}</span></> : null}
          {account.lastRefreshErrorCode ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorCode")}</span><span className="break-all">{account.lastRefreshErrorCode}</span></> : null}
          {account.lastRefreshErrorMessage ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorMessage")}</span><span className="break-words">{account.lastRefreshErrorMessage}</span></> : null}
          {refreshErrorDetails ? <><span className="text-primary-foreground/60">{t("accounts.refreshErrorResponse")}</span><span className="max-h-40 overflow-auto whitespace-pre-wrap break-all">{refreshErrorDetails}</span></> : null}
        </div>
      )}>
        <Badge variant="destructive">{t("accounts.statusReauthRequired")}</Badge>
      </StatusTooltip>
    );
  }
  const consoleWindow = account.provider === "grok_console"
    ? account.quotaWindows?.find((window) => window.mode === "console" && window.remaining <= 0)
    : undefined;
  if (consoleWindow) {
    const detail = consoleWindow.resetAt
      ? t("console.recoveryProbeAt", { time: formatDateTime(consoleWindow.resetAt, i18n.language) })
      : t("accounts.quotaResetUnknown");
    return (
      <StatusTooltip content={detail}>
        <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.waitingReset")}</Badge>
      </StatusTooltip>
    );
  }
  if (account.quota.status === "waitingReset") {
    const detail = account.quota.nextProbeAt
      ? t(account.quota.type === "paid" ? "accounts.paidWaitingResetUntil" : "accounts.waitingResetUntil", { time: formatDateTime(account.quota.nextProbeAt, i18n.language) })
      : t("accounts.quotaResetUnknown");
    return (
      <StatusTooltip content={detail}>
        <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.waitingReset")}</Badge>
      </StatusTooltip>
    );
  }
  if (account.quota.status === "probing") {
    return (
      <StatusTooltip content={t(account.quota.type === "paid" ? "accounts.paidProbingQuota" : "accounts.probingQuota")}>
        <Badge variant="secondary" className="bg-sky-500/10 text-sky-700 dark:text-sky-300">{t("accounts.probing")}</Badge>
      </StatusTooltip>
    );
  }
  if (account.cooldownUntil && new Date(account.cooldownUntil) > new Date()) {
			// Cooldown reason taxonomy: routing-guard markers (missing_thinking family /
			// empty-stream idle) vs generic upstream failures - tells the operator
			// whether the cooldown is attributable/clearable by a clean RSC verdict.
			const cooldownReasons = ["missing_thinking", "missing_thinking_disabled", "quality_idle_timeout"] as const;
			const reasonKey: (typeof cooldownReasons)[number] | "generic" = cooldownReasons.includes(account.lastError as (typeof cooldownReasons)[number])
				? (account.lastError as (typeof cooldownReasons)[number])
				: "generic";
			return (
				<StatusTooltip content={`${t("accounts.cooldownReasons." + reasonKey)} · ${formatDateTime(account.cooldownUntil, i18n.language)}`}>
					<div className="inline-flex items-center gap-1">
						<Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.statusCooldown")}</Badge>
						<ClearCooldownButton account={account} />
					</div>
				</StatusTooltip>
			);
				  }
  return <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("accounts.statusActive")}</Badge>;
}

function ClearCooldownButton({ account }: { account: AccountDTO }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const clearCooldown = useMutation({
    mutationFn: () => clearAccountCooldown(String(account.id)),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["accounts"] });
      toast.success(t("accounts.cooldownCleared"));
    },
    onError: (error: unknown) => {
      toast.error(error instanceof ApiError ? error.message : t("accounts.cooldownClearFailed"));
    },
  });
  return (
    <button
      type="button"
      className="text-muted-foreground transition-colors hover:text-foreground"
      aria-label={t("accounts.clearCooldown")}
      title={t("accounts.clearCooldownHelp")}
      disabled={clearCooldown.isPending}
      onClick={(event) => {
        event.stopPropagation();
        clearCooldown.mutate();
      }}
    >
      {clearCooldown.isPending ? <Spinner className="size-3" /> : <X className="size-3" />}
    </button>
  );
}

function formatAdditionalRefreshErrorDetails(account: AccountDTO): string | undefined {
  const response = account.lastRefreshErrorResponse?.trim();
  if (!response) return undefined;
  try {
    const parsed: unknown = JSON.parse(response);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return response;
    const details = { ...(parsed as Record<string, unknown>) };
    const messages = new Set((account.lastRefreshErrorMessage ?? "").split(" · ").map((value) => value.trim()).filter(Boolean));
    if (typeof details.error === "string" && details.error === account.lastRefreshErrorCode) delete details.error;
    for (const key of ["error_description", "message", "detail", "description", "title"]) {
      if (typeof details[key] === "string" && messages.has(details[key])) delete details[key];
    }
    if (details.error && typeof details.error === "object" && !Array.isArray(details.error)) {
      const nested = { ...(details.error as Record<string, unknown>) };
      if (typeof nested.code === "string" && nested.code === account.lastRefreshErrorCode) delete nested.code;
      for (const key of ["error_description", "message", "detail", "description"]) {
        if (typeof nested[key] === "string" && messages.has(nested[key])) delete nested[key];
      }
      if (Object.keys(nested).length === 0) delete details.error;
      else details.error = nested;
    }
    if (Object.keys(details).length === 0) return undefined;
    return JSON.stringify(details, null, 2);
  } catch {
    return response;
  }
}

export function StatusTooltip({ children, content }: { children: ReactNode; content: ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div tabIndex={0} className="inline-flex cursor-help">{children}</div>
      </TooltipTrigger>
      <TooltipContent className="w-max max-w-sm">{content}</TooltipContent>
    </Tooltip>
  );
}
