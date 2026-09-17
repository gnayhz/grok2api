import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { updateAccount, type AccountDTO, type AccountUpdateInput, type BuildRouteMode } from "@/entities/account/account-api";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { showErrorToast } from "@/shared/lib/show-error";
import { Button } from "@/shared/ui/button";
import { Checkbox } from "@/shared/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { Input } from "@/shared/ui/input";
import { Label } from "@/shared/ui/label";
import { Spinner } from "@/shared/ui/spinner";
import { Switch } from "@/shared/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/shared/ui/tabs";
import { Textarea } from "@/shared/ui/textarea";

// Mount once per editing identity. Closing/replacing the editor cancels its
// request and prevents an old acknowledgement from closing the next editor.
export function AccountEditor({ account: editing, onClose }: { account: AccountDTO; onClose: () => void }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const accountSchema = z.object({
    name: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
    priority: z.number().int(),
    maxConcurrent: z.number().int().min(1, t("errors.positive")).max(256),
    minimumRemaining: z.number().min(0),
    cloudflareCookies: z.string().max(16 << 10, t("settings.invalidValue")),
    clearCloudflareCookies: z.boolean(),
    buildSuperEntitled: z.boolean(),
    buildRouteMode: z.enum(["auto", "build", "xai"]),
  });
  type AccountForm = z.infer<typeof accountSchema>;
  const form = useForm<AccountForm>({
    resolver: zodResolver(accountSchema),
    defaultValues: {
      name: editing.name, enabled: editing.enabled, priority: editing.priority, maxConcurrent: editing.maxConcurrent, minimumRemaining: editing.minimumRemaining,
      cloudflareCookies: "", clearCloudflareCookies: false, buildSuperEntitled: editing.buildSuperEntitled, buildRouteMode: editing.buildRouteMode,
    },
  });
  const accountEnabled = useWatch({ control: form.control, name: "enabled" });
  const clearCloudflareCookies = useWatch({ control: form.control, name: "clearCloudflareCookies" });
  const buildSuperEntitled = useWatch({ control: form.control, name: "buildSuperEntitled" });
  const buildRouteMode = useWatch({ control: form.control, name: "buildRouteMode" });
  const updateMutation = useLifetimeMutation({
    mutationFn: (values: AccountForm, signal) => {
      const input: AccountUpdateInput = {
        name: values.name,
        priority: values.priority,
        maxConcurrent: values.maxConcurrent,
        minimumRemaining: values.minimumRemaining,
      };
      if (values.enabled !== editing.enabled) input.enabled = values.enabled;
      if (editing.provider !== "grok_build") {
        if (values.clearCloudflareCookies) input.clearCloudflareCookies = true;
        else if (values.cloudflareCookies.trim()) input.cloudflareCookies = values.cloudflareCookies;
      } else {
        input.buildRouteMode = values.buildRouteMode;
        if (values.buildSuperEntitled !== editing.buildSuperEntitled) input.buildSuperEntitled = values.buildSuperEntitled;
      }
      // 手动风控打标已下线(架构基准 B5:质量语义归仲裁庭;账号启停归底座)。
      // 遗留标记经列表徽章只读可见,不再经编辑表单改写。
      return updateAccount(editing.id, input, signal);
    },
    onSuccess: (account, values) => {
      const entitlementChanged = editing?.provider === "grok_build" && values.buildSuperEntitled !== editing.buildSuperEntitled;
      void queryClient.invalidateQueries({ queryKey: ["accounts"] });
      if (entitlementChanged) void queryClient.invalidateQueries({ queryKey: ["models"] });
      onClose();
      if (account.modelSyncFailed) toast.warning(t("accounts.updatedWithModelSyncFailure"));
      else if (account.enabledDoesNotClearCooldown) toast.warning(t("accounts.enabledDoesNotClearCooldown"));
      else toast.success(t("accounts.updated"));
    },
    onError: (error) => showErrorToast(error, t),
  });

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("common.edit")} {editing.name}</DialogTitle>
            <DialogDescription>{editing.email ?? editing.userId}</DialogDescription>
          </DialogHeader>
          <form className="space-y-4" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
            <div className="space-y-2"><Label htmlFor="account-name">{t("accounts.name")}</Label><Input id="account-name" {...form.register("name")} />{form.formState.errors.name ? <p className="text-xs text-destructive">{form.formState.errors.name.message}</p> : null}</div>
            <div className="flex items-center justify-between border-b py-2"><Label htmlFor="account-enabled">{accountEnabled ? t("common.enabled") : t("common.disabled")}</Label><Switch id="account-enabled" checked={accountEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} /></div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2"><Label htmlFor="account-priority">{t("accounts.priority")}</Label><Input id="account-priority" type="number" {...form.register("priority", { valueAsNumber: true })} /></div>
              <div className="space-y-2"><Label htmlFor="account-concurrency">{t("accounts.maxConcurrent")}</Label><Input id="account-concurrency" type="number" min="1" max="256" {...form.register("maxConcurrent", { valueAsNumber: true })} /></div>
            </div>
            <div className="space-y-2"><Label htmlFor="account-minimum">{t("accounts.minimumRemaining")}</Label><Input id="account-minimum" type="number" min="0" step="0.01" {...form.register("minimumRemaining", { valueAsNumber: true })} /></div>
            {editing.provider === "grok_build" ? (
              <div className="space-y-4">
                <div className="flex items-start justify-between gap-4 rounded-md bg-muted/50 p-3">
                  <div className="space-y-1">
                    <Label htmlFor="account-build-super-entitled">{t("accounts.buildSuperEntitled.label")}</Label>
                    <p className="text-xs text-muted-foreground">{t("accounts.buildSuperEntitled.description")}</p>
                  </div>
                  <Switch id="account-build-super-entitled" checked={buildSuperEntitled} onCheckedChange={(checked) => form.setValue("buildSuperEntitled", checked, { shouldDirty: true })} />
                </div>
                <div className="space-y-2">
                  <Label id="account-build-route-mode">{t("accounts.buildRouteMode.label")}</Label>
                  <Tabs value={buildRouteMode} onValueChange={(value) => form.setValue("buildRouteMode", value as BuildRouteMode, { shouldDirty: true })}>
                    <TabsList aria-labelledby="account-build-route-mode" className="grid h-10 w-full grid-cols-3 p-1">
                    {(["auto", "build", "xai"] as BuildRouteMode[]).map((mode) => (
                      <TabsTrigger
                        key={mode}
                        value={mode}
                        className="h-8 px-2 font-normal data-[state=active]:font-medium"
                      >
                        {t(`accounts.buildRouteMode.${mode}`)}
                      </TabsTrigger>
                    ))}
                    </TabsList>
                  </Tabs>
                  <p className="text-xs text-muted-foreground">{t(`accounts.buildRouteMode.${buildRouteMode}Description`)}</p>
                  {buildRouteMode === "xai" && !buildSuperEntitled && !(editing.quota.type === "paid" && editing.quota.source !== "buildSuperEntitlement") ? (
                    <p className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-300"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" />{t("accounts.buildRouteMode.xaiUnconfirmedWarning")}</p>
                  ) : null}
                </div>
              </div>
            ) : null}
            {editing.provider !== "grok_build" ? (
              <div className="space-y-2">
                <Label htmlFor="account-cloudflare-cookie">{t("settings.egress.cloudflareCookie")}</Label>
                <Textarea
                  id="account-cloudflare-cookie"
                  className="min-h-20 font-mono text-xs"
                  autoComplete="new-password"
                  spellCheck={false}
                  disabled={clearCloudflareCookies}
                  placeholder={editing.cloudflareCookieConfigured ? t("settings.egress.keepConfigured") : "cf_clearance=..."}
                  {...form.register("cloudflareCookies")}
                />
                {editing.cloudflareCookieConfigured ? (
                  <label className="flex items-center gap-2 text-xs text-muted-foreground">
                    <Checkbox checked={clearCloudflareCookies} onCheckedChange={(checked) => form.setValue("clearCloudflareCookies", checked === true)} />
                    {t("common.clear")}
                  </label>
                ) : null}
                {form.formState.errors.cloudflareCookies ? <p className="text-xs text-destructive">{form.formState.errors.cloudflareCookies.message}</p> : null}
              </div>
            ) : null}
            <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => onClose()}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={updateMutation.isPending}>{updateMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
  );
}
