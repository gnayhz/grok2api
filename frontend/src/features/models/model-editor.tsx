import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { createModel, updateModel } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { showErrorToast } from "@/shared/lib/show-error";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { Input } from "@/shared/ui/input";
import { Label } from "@/shared/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/shared/ui/select";
import { Spinner } from "@/shared/ui/spinner";
import { Switch } from "@/shared/ui/switch";
import { ModelAccountPicker } from "./model-account-picker";

export function ModelEditor({ editing, onClose, onSaved }: {
  editing: ModelRouteDTO | "new";
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const schema = z.object({
    publicId: z.string().min(1, t("errors.required")),
    provider: z.enum(["grok_build", "grok_web", "grok_console"]),
    upstreamModel: z.string().min(1, t("errors.required")),
    capability: z.enum(["responses", "chat", "image", "image_edit", "video", "tts", "stt", "realtime"]),
    enabled: z.boolean(),
    bindingMode: z.boolean(),
    accountIds: z.array(z.string()),
  }).refine((value) => !value.bindingMode || value.accountIds.length > 0, { path: ["accountIds"], message: t("models.selectAccountRequired") });
  type ModelForm = z.infer<typeof schema>;
  const form = useForm<ModelForm>({
    resolver: zodResolver(schema),
    defaultValues: editing === "new"
      ? { publicId: "", provider: "grok_build", upstreamModel: "", capability: "responses", enabled: true, bindingMode: false, accountIds: [] }
      : { publicId: editing.publicId, provider: editing.provider, upstreamModel: editing.upstreamModel, capability: editing.capability, enabled: editing.enabled, bindingMode: editing.bindingMode, accountIds: editing.accountIds },
  });
  const modelEnabled = useWatch({ control: form.control, name: "enabled" });
  const selectedProvider = useWatch({ control: form.control, name: "provider" });
  const selectedCapability = useWatch({ control: form.control, name: "capability" });
  const bindingMode = useWatch({ control: form.control, name: "bindingMode" });
  const selectedAccountIDs = useWatch({ control: form.control, name: "accountIds" });

  const updateMutation = useLifetimeMutation({
    mutationFn: (values: ModelForm, signal) => {
      const input = { ...values, accountIds: values.bindingMode ? values.accountIds : [] };
      if (editing === "new") return createModel(input, signal);
      const patch: Parameters<typeof updateModel>[1] = {};
      if (input.publicId !== editing.publicId) patch.publicId = input.publicId;
      if (input.enabled !== editing.enabled) patch.enabled = input.enabled;
      const oldIDs = new Set(editing.accountIds);
      if (oldIDs.size !== input.accountIds.length || input.accountIds.some((id) => !oldIDs.has(id))) patch.accountIds = input.accountIds;
      return updateModel(editing.id, patch, signal);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      onSaved();
      onClose();
      toast.success(t(editing === "new" ? "models.created" : "models.updated"));
    },
    onError: (error) => showErrorToast(error, t),
  });

  function toggleBoundAccount(id: string, checked: boolean): void {
    const current = form.getValues("accountIds");
    form.setValue("accountIds", checked ? [...new Set([...current, id])] : current.filter((value) => value !== id), { shouldValidate: true });
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 text-xs sm:max-w-[600px]">
        <DialogHeader className="shrink-0 px-5 py-4 pr-12">
          <DialogTitle>{t(editing === "new" ? "models.createTitle" : "models.editTitle")}</DialogTitle>
          <DialogDescription className="truncate">{editing === "new" ? t("models.createDescription") : editing.upstreamModel}</DialogDescription>
        </DialogHeader>
        <form className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto overscroll-contain px-5 pb-4 pt-2">
            <div className="space-y-2"><Label htmlFor="model-public-id">{t("models.publicId")}</Label><Input id="model-public-id" {...form.register("publicId")} />{form.formState.errors.publicId ? <p className="text-xs text-destructive">{form.formState.errors.publicId.message}</p> : null}</div>
            {editing === "new" ? (
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="space-y-2">
                  <Label>{t("models.provider")}</Label>
                  <Select value={selectedProvider} disabled>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent><SelectItem value="grok_build">{t("models.providerGrokBuild")}</SelectItem></SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <Label>{t("models.capability")}</Label>
                  <Select value={selectedCapability} disabled>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent><SelectItem value="responses">Responses</SelectItem></SelectContent>
                  </Select>
                </div>
                <div className="space-y-2 sm:col-span-2"><Label htmlFor="model-upstream-id">{t("models.upstream")}</Label><Input id="model-upstream-id" {...form.register("upstreamModel")} />{form.formState.errors.upstreamModel ? <p className="text-xs text-destructive">{form.formState.errors.upstreamModel.message}</p> : null}</div>
              </div>
            ) : null}
            <section className="rounded-lg bg-muted/25 p-3">
              <div className="flex items-start justify-between gap-4">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <Label htmlFor="model-binding-mode">{t("models.bindAccounts")}</Label>
                    {bindingMode ? <Badge variant="secondary" className="text-[10px] font-normal tabular-nums" aria-live="polite">{t("models.selectedAccounts", { count: selectedAccountIDs.length })}</Badge> : null}
                  </div>
                  <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("models.bindAccountsDescription")}</p>
                </div>
                <Switch className="mt-0.5 shrink-0" id="model-binding-mode" checked={bindingMode} onCheckedChange={(checked) => { form.setValue("bindingMode", checked); if (!checked) form.clearErrors("accountIds"); }} />
              </div>
              {bindingMode ? (
                <div className="mt-3">
                  <ModelAccountPicker provider={selectedProvider} selectedIDs={selectedAccountIDs} onToggle={toggleBoundAccount} />
                  {form.formState.errors.accountIds ? <p className="mt-2 text-xs text-destructive">{form.formState.errors.accountIds.message}</p> : null}
                </div>
              ) : null}
            </section>
            <section className="flex items-center justify-between gap-4 rounded-lg bg-muted/35 px-3 py-2.5">
              <div className="min-w-0">
                <Label htmlFor="model-enabled">{modelEnabled ? t("common.enabled") : t("common.disabled")}</Label>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("models.enabledDescription")}</p>
              </div>
              <Switch id="model-enabled" checked={modelEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} />
            </section>
          </div>
          <DialogFooter className="shrink-0 gap-2 bg-muted/20 px-5 py-3.5 sm:gap-0"><Button type="button" variant="secondary" size="sm" onClick={() => onClose()}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={updateMutation.isPending}>{updateMutation.isPending ? <Spinner /> : null}{editing === "new" ? t("common.create") : t("common.save")}</Button></DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
