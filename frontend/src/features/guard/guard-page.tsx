import { useMutation } from "@tanstack/react-query";
import { BarChart3, RefreshCw, RotateCcw, ShieldAlert, ShieldCheck } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { Controller } from "react-hook-form";
import { useTranslation } from "react-i18next";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { GuardStatsPanel } from "@/features/guard/guard-stats-panel";
import { runAccountRiskPatrol } from "@/features/settings/settings-api";
import { DurationInput, SettingsField, SettingsPane, SettingsSection } from "@/features/settings/settings-ui";
import { useSettings } from "@/features/settings/use-settings";
import { ErrorState } from "@/shared/components/data-state";

// 质量防护页:路由守卫(实时扣留/重试)与出口换 IP 轮换的独立入口。
// 复用 useSettings 的完整表单——PUT 提交整份配置,避免部分提交把其他节清零;
// 页面只渲染这两节的字段。保存后与设置页共享同一份 react-query 缓存,
// 两边任一处修改都会即时同步。

export function GuardPage() {
  const { t } = useTranslation();
  const { form, settingsQuery, updateMutation, resetDefaultsMutation, reset } = useSettings();
  const [resetDefaultsConfirm, setResetDefaultsConfirm] = useState(false);
  const patrolMutation = useMutation({
    mutationFn: runAccountRiskPatrol,
    onSuccess: (result) => {
      toast.success(t("settings.accountRisk.patrolRunQueued", { count: result.due }));
    },
    onError: (error: Error) => {
      toast.error(error.message);
    },
  });

  if (settingsQuery.isError) {
    return <ErrorState message={settingsQuery.error.message} onRetry={() => void settingsQuery.refetch()} />;
  }

  const loading = settingsQuery.isPending;

  return (
    <form className="w-full space-y-5" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
      <header className="flex min-h-8 items-center justify-between gap-3">
        <h1 className="text-xl font-medium">{t("settings.guard.title")}</h1>
        <p className="sr-only">{t("settings.guard.description")}</p>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <Tooltip>
            <TooltipTrigger asChild>
              <Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.reset")} disabled={loading || updateMutation.isPending || !form.formState.isDirty} onClick={reset}>
                <RotateCcw />
              </Button>
            </TooltipTrigger>
            <TooltipContent>{t("common.reset")}</TooltipContent>
          </Tooltip>
          <Button type="submit" size="sm" disabled={loading || updateMutation.isPending || !form.formState.isDirty}>
            {updateMutation.isPending ? <Spinner /> : null}{t("common.save")}
          </Button>
          <Button type="button" variant="outline" size="sm" disabled={loading || updateMutation.isPending || resetDefaultsMutation.isPending} onClick={() => setResetDefaultsConfirm(true)}>
            {resetDefaultsMutation.isPending ? <Spinner /> : null}{t("settings.resetToDefaults")}
          </Button>
        </div>
      </header>

      {loading ? <div className="flex min-h-64 items-center justify-center"><Spinner /></div> : null}
      {settingsQuery.data ? (
        <Tabs defaultValue="stats" className="gap-5">
          <div className="flex flex-wrap items-center gap-2">
            <TabsList>
              <TabsTrigger value="stats" className="gap-1.5">
                <BarChart3 className="size-3.5" />
                <span>{t("settings.guardStats.tabTitle")}</span>
              </TabsTrigger>
              <TabsTrigger value="requestRetry" className="gap-1.5">
                <ShieldCheck className="size-3.5" />
                <span>{t("settings.requestRetry.title")}</span>
              </TabsTrigger>
              <TabsTrigger value="egressRotation" className="gap-1.5">
                <RefreshCw className="size-3.5" />
                <span>{t("settings.egressRotation.title")}</span>
              </TabsTrigger>
              <TabsTrigger value="accountRisk" className="gap-1.5">
                <ShieldAlert className="size-3.5" />
                <span>{t("settings.accountRisk.title")}</span>
              </TabsTrigger>
            </TabsList>
          </div>

          <div className="min-w-0 flex-1">
          <SettingsPane value="stats">
          <SettingsSection title={t("settings.guardStats.title")}>
            <GuardStatsPanel tab="requestRetry" />
          </SettingsSection>

          <SettingsSection title={t("settings.guardStats.canaryTitle")}>
            <GuardStatsPanel tab="egressRotation" />
          </SettingsSection>
          </SettingsPane>

          <SettingsPane value="requestRetry">
          <SettingsSection title={t("settings.requestRetry.title")}>
            <div className="space-y-0">
              <SettingsField controlId="request-retry-enabled" className="sm:col-span-2" label={t("settings.requestRetry.enabled")} description={t("settings.requestRetry.enabledHelp")}><Controller control={form.control} name="requestRetry.enabled" render={({ field }) => <div className="flex h-8 items-center"><Switch id="request-retry-enabled" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
              <SettingsField controlId="request-retry-created-timeout" label={t("settings.requestRetry.createdTimeout")} description={t("settings.requestRetry.createdTimeoutHelp")} error={form.formState.errors.requestRetry?.createdTimeout?.message}><Controller control={form.control} name="requestRetry.createdTimeout" render={({ field }) => <DurationInput id="request-retry-created-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="request-retry-evidence-timeout" label={t("settings.requestRetry.evidenceTimeout")} description={t("settings.requestRetry.evidenceTimeoutHelp")} error={form.formState.errors.requestRetry?.evidenceTimeout?.message}><Controller control={form.control} name="requestRetry.evidenceTimeout" render={({ field }) => <DurationInput id="request-retry-evidence-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="request-retry-hold-timeout" label={t("settings.requestRetry.holdTimeout")} description={t("settings.requestRetry.holdTimeoutHelp")} error={form.formState.errors.requestRetry?.holdTimeout?.message}><Controller control={form.control} name="requestRetry.holdTimeout" render={({ field }) => <DurationInput id="request-retry-hold-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="request-retry-early-header-abort" label={t("settings.requestRetry.earlyHeaderAbort")} description={t("settings.requestRetry.earlyHeaderAbortHelp")} error={form.formState.errors.requestRetry?.earlyHeaderAbort?.message}><Controller control={form.control} name="requestRetry.earlyHeaderAbort" render={({ field }) => <DurationInput id="request-retry-early-header-abort" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="request-retry-max-attempts" label={t("settings.requestRetry.maxAttempts")} description={t("settings.requestRetry.maxAttemptsHelp")} error={form.formState.errors.requestRetry?.maxAttempts?.message}><Input id="request-retry-max-attempts" type="number" min={1} max={6} {...form.register("requestRetry.maxAttempts", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="request-retry-min-output-tokens" label={t("settings.requestRetry.minOutputTokens")} description={t("settings.requestRetry.minOutputTokensHelp")} error={form.formState.errors.requestRetry?.minOutputTokens?.message}><Input id="request-retry-min-output-tokens" type="number" min={1} max={256} {...form.register("requestRetry.minOutputTokens", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="request-retry-same-account-retry" label={t("settings.requestRetry.sameAccountRetry")} description={t("settings.requestRetry.sameAccountRetryHelp")}><Controller control={form.control} name="requestRetry.sameAccountRetry" render={({ field }) => <div className="flex h-8 items-center"><Switch id="request-retry-same-account-retry" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
              <SettingsField controlId="request-retry-on-exhausted" label={t("settings.requestRetry.onExhausted")} description={t("settings.requestRetry.onExhaustedHelp")} error={form.formState.errors.requestRetry?.onExhausted?.message}>
                <Controller control={form.control} name="requestRetry.onExhausted" render={({ field }) => (
                  <Tabs value={field.value} onValueChange={field.onChange}>
                    <TabsList id="request-retry-on-exhausted" className="grid w-full grid-cols-2 bg-muted/55">
                      <TabsTrigger value="fail_closed" className="font-normal">{t("settings.requestRetry.failClosed")}</TabsTrigger>
                      <TabsTrigger value="fail_open" className="font-normal">{t("settings.requestRetry.failOpen")}</TabsTrigger>
                    </TabsList>
                  </Tabs>
                )} />
              </SettingsField>
              <SettingsField controlId="request-retry-account-cooldown" label={t("settings.requestRetry.accountCooldown")} description={t("settings.requestRetry.accountCooldownHelp")} error={form.formState.errors.requestRetry?.accountCooldown?.message}><Controller control={form.control} name="requestRetry.accountCooldown" render={({ field }) => <DurationInput id="request-retry-account-cooldown" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="request-retry-idle-cooldown" label={t("settings.requestRetry.idleAccountCooldown")} description={t("settings.requestRetry.idleAccountCooldownHelp")} error={form.formState.errors.requestRetry?.idleAccountCooldown?.message}><Controller control={form.control} name="requestRetry.idleAccountCooldown" render={({ field }) => <DurationInput id="request-retry-idle-cooldown" value={field.value} onChange={field.onChange} />} /></SettingsField>
            </div>
          </SettingsSection>
          </SettingsPane>

          <SettingsPane value="egressRotation">
          <SettingsSection title={t("settings.egressRotation.title")}>
            <div className="space-y-0">
              <SettingsField controlId="egress-rotation-enabled" className="sm:col-span-2" label={t("settings.egressRotation.enabled")} description={t("settings.egressRotation.enabledHelp")}><Controller control={form.control} name="egressRotation.enabled" render={({ field }) => <div className="flex h-8 items-center"><Switch id="egress-rotation-enabled" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
              <SettingsField controlId="egress-rotation-min-interval" label={t("settings.egressRotation.minNodeInterval")} description={t("settings.egressRotation.minNodeIntervalHelp")} error={form.formState.errors.egressRotation?.minNodeInterval?.message}><Controller control={form.control} name="egressRotation.minNodeInterval" render={({ field }) => <DurationInput id="egress-rotation-min-interval" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="egress-rotation-max-attempts" label={t("settings.egressRotation.maxAttemptsPerQuarantine")} description={t("settings.egressRotation.maxAttemptsPerQuarantineHelp")} error={form.formState.errors.egressRotation?.maxAttemptsPerQuarantine?.message}><Input id="egress-rotation-max-attempts" type="number" min={1} max={100} {...form.register("egressRotation.maxAttemptsPerQuarantine", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="egress-rotation-max-global" label={t("settings.egressRotation.maxGlobalPerHour")} description={t("settings.egressRotation.maxGlobalPerHourHelp")} error={form.formState.errors.egressRotation?.maxGlobalPerHour?.message}><Input id="egress-rotation-max-global" type="number" min={1} max={10_000} {...form.register("egressRotation.maxGlobalPerHour", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="egress-rotation-settle-delay" label={t("settings.egressRotation.settleDelay")} description={t("settings.egressRotation.settleDelayHelp")} error={form.formState.errors.egressRotation?.settleDelay?.message}><Controller control={form.control} name="egressRotation.settleDelay" render={({ field }) => <DurationInput id="egress-rotation-settle-delay" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="egress-rotation-probe-timeout" label={t("settings.egressRotation.probeTimeout")} description={t("settings.egressRotation.probeTimeoutHelp")} error={form.formState.errors.egressRotation?.probeTimeout?.message}><Controller control={form.control} name="egressRotation.probeTimeout" render={({ field }) => <DurationInput id="egress-rotation-probe-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="egress-rotation-probe-interval" label={t("settings.egressRotation.probeInterval")} description={t("settings.egressRotation.probeIntervalHelp")} error={form.formState.errors.egressRotation?.probeInterval?.message}><Controller control={form.control} name="egressRotation.probeInterval" render={({ field }) => <DurationInput id="egress-rotation-probe-interval" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="egress-rotation-webhook-timeout" label={t("settings.egressRotation.webhookTimeout")} description={t("settings.egressRotation.webhookTimeoutHelp")} error={form.formState.errors.egressRotation?.webhookTimeout?.message}><Controller control={form.control} name="egressRotation.webhookTimeout" render={({ field }) => <DurationInput id="egress-rotation-webhook-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="egress-rotation-webhook-retries" label={t("settings.egressRotation.webhookRetries")} description={t("settings.egressRotation.webhookRetriesHelp")} error={form.formState.errors.egressRotation?.webhookRetries?.message}><Input id="egress-rotation-webhook-retries" type="number" min={0} max={10} {...form.register("egressRotation.webhookRetries", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="egress-rotation-canary-model" className="sm:col-span-2" label={t("settings.egressRotation.canaryModelPublicId")} description={t("settings.egressRotation.canaryModelPublicIdHelp")} error={form.formState.errors.egressRotation?.canaryModelPublicId?.message}><Input id="egress-rotation-canary-model" placeholder="Build/grok-4.5" {...form.register("egressRotation.canaryModelPublicId")} /></SettingsField>
              <SettingsField controlId="egress-rotation-canary-timeout" label={t("settings.egressRotation.canaryCreatedTimeout")} description={t("settings.egressRotation.canaryCreatedTimeoutHelp")} error={form.formState.errors.egressRotation?.canaryCreatedTimeout?.message}><Controller control={form.control} name="egressRotation.canaryCreatedTimeout" render={({ field }) => <DurationInput id="egress-rotation-canary-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
            </div>
          </SettingsSection>
          </SettingsPane>

          <SettingsPane value="accountRisk">
          <SettingsSection title={t("settings.accountRisk.title")} action={
            <Button type="button" variant="outline" size="sm" disabled={loading || patrolMutation.isPending} onClick={() => patrolMutation.mutate()}>
              {patrolMutation.isPending ? <Spinner /> : <RefreshCw />}
              {t("settings.accountRisk.patrolRunNow")}
            </Button>
          }>
            <div className="space-y-0">
              <SettingsField controlId="account-risk-enabled" className="sm:col-span-2" label={t("settings.accountRisk.enabled")} description={t("settings.accountRisk.enabledHelp")}><Controller control={form.control} name="accountRisk.enabled" render={({ field }) => <div className="flex h-8 items-center"><Switch id="account-risk-enabled" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
              <SettingsField controlId="account-risk-method" label={t("settings.accountRisk.method")} description={t("settings.accountRisk.methodHelp")} error={form.formState.errors.accountRisk?.method?.message}>
                <Controller control={form.control} name="accountRisk.method" render={({ field }) => (
                  <Tabs value={field.value} onValueChange={field.onChange}>
                    <TabsList id="account-risk-method" className="grid w-full grid-cols-2 bg-muted/55">
                      <TabsTrigger value="ssoProbe" className="font-normal">{t("settings.accountRisk.methodSSOProbe")}</TabsTrigger>
                      <TabsTrigger value="homepage" className="font-normal">{t("settings.accountRisk.methodHomepage")}</TabsTrigger>
                    </TabsList>
                  </Tabs>
                )} />
              </SettingsField>
              <SettingsField controlId="account-risk-on-denied" label={t("settings.accountRisk.onDenied")} description={t("settings.accountRisk.onDeniedHelp")} error={form.formState.errors.accountRisk?.onDenied?.message}>
                <Controller control={form.control} name="accountRisk.onDenied" render={({ field }) => (
                  <Tabs value={field.value} onValueChange={field.onChange}>
                    <TabsList id="account-risk-on-denied" className="grid w-full grid-cols-3 bg-muted/55">
                      <TabsTrigger value="flag" className="font-normal">{t("settings.accountRisk.onDeniedFlag")}</TabsTrigger>
                      <TabsTrigger value="disable" className="font-normal">{t("settings.accountRisk.onDeniedDisable")}</TabsTrigger>
                      <TabsTrigger value="markOnly" className="font-normal">{t("settings.accountRisk.onDeniedMarkOnly")}</TabsTrigger>
                    </TabsList>
                  </Tabs>
                )} />
              </SettingsField>
              <SettingsField controlId="account-risk-timeout" label={t("settings.accountRisk.timeout")} description={t("settings.accountRisk.timeoutHelp")} error={form.formState.errors.accountRisk?.timeout?.message}><Controller control={form.control} name="accountRisk.timeout" render={({ field }) => <DurationInput id="account-risk-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="account-risk-concurrency" label={t("settings.accountRisk.concurrency")} description={t("settings.accountRisk.concurrencyHelp")} error={form.formState.errors.accountRisk?.concurrency?.message}><Input id="account-risk-concurrency" type="number" min={1} max={8} {...form.register("accountRisk.concurrency", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="account-risk-build-probe" className="sm:col-span-2" label={t("settings.accountRisk.buildProbeEnabled")} description={t("settings.accountRisk.buildProbeEnabledHelp")}><Controller control={form.control} name="accountRisk.buildProbeEnabled" render={({ field }) => <div className="flex h-8 items-center"><Switch id="account-risk-build-probe" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
              <SettingsField controlId="account-risk-patrol-enabled" className="sm:col-span-2" label={t("settings.accountRisk.patrolEnabled")} description={t("settings.accountRisk.patrolEnabledHelp")}><Controller control={form.control} name="accountRisk.patrolEnabled" render={({ field }) => <div className="flex h-8 items-center"><Switch id="account-risk-patrol-enabled" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
              <SettingsField controlId="account-risk-patrol-days" label={t("settings.accountRisk.patrolBucketDays")} description={t("settings.accountRisk.patrolBucketDaysHelp")} error={form.formState.errors.accountRisk?.patrolBucketDays?.message}><Input id="account-risk-patrol-days" type="number" min={7} max={90} {...form.register("accountRisk.patrolBucketDays", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="account-risk-patrol-interval" label={t("settings.accountRisk.patrolInterval")} description={t("settings.accountRisk.patrolIntervalHelp")} error={form.formState.errors.accountRisk?.patrolInterval?.message}><Controller control={form.control} name="accountRisk.patrolInterval" render={({ field }) => <DurationInput id="account-risk-patrol-interval" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="account-risk-patrol-batch" label={t("settings.accountRisk.patrolBatchSize")} description={t("settings.accountRisk.patrolBatchSizeHelp")} error={form.formState.errors.accountRisk?.patrolBatchSize?.message}><Input id="account-risk-patrol-batch" type="number" min={1} max={200} {...form.register("accountRisk.patrolBatchSize", { valueAsNumber: true })} /></SettingsField>
            </div>
          </SettingsSection>
          </SettingsPane>
          </div>
        </Tabs>
      ) : null}

      <AlertDialog open={resetDefaultsConfirm} onOpenChange={setResetDefaultsConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("settings.resetToDefaultsTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t("settings.resetToDefaultsDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => {
                resetDefaultsMutation.mutate();
                setResetDefaultsConfirm(false);
              }}
            >
              {t("settings.resetToDefaultsConfirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </form>
  );
}
