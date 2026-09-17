import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronLeft, ChevronRight, CircleHelp, Search } from "lucide-react";
import { useState } from "react";
import { Controller, useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { createClientKey, updateClientKey, type ClientKeyDTO, type ClientKeyInput, type CreateKeyResponseDTO, type ProviderScopeValue, type TierScopeValue } from "@/entities/client-key/client-key-api";
import { listModels } from "@/entities/model/model-api";
import { DateTimePicker } from "@/shared/components/date-time-picker";
import { LoadingState } from "@/shared/components/data-state";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { cn } from "@/shared/lib/cn";
import { toDateTimeLocal } from "@/shared/lib/format";
import { showErrorToast } from "@/shared/lib/show-error";
import { USD_TICKS_PER_DOLLAR as USD_TICKS } from "@/shared/lib/usd";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Checkbox } from "@/shared/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/shared/ui/dropdown-menu";
import { Input } from "@/shared/ui/input";
import { Label } from "@/shared/ui/label";
import { Spinner } from "@/shared/ui/spinner";
import { Switch } from "@/shared/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";

const MAX_BILLING_LIMIT_USD = 900_000;

export function ClientKeyEditor({ editing, onClose, onCreated }: {
  editing: ClientKeyDTO | "new";
  onClose: () => void;
  onCreated: (secret: string) => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [modelOptionsPage, setModelOptionsPage] = useState(1);
  const [modelOptionsSearch, setModelOptionsSearch] = useState("");
  const debouncedModelOptionsSearch = useDebouncedValue(modelOptionsSearch);
  const schema = z.object({
    name: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
    expiryUnlimited: z.boolean(),
    expiresAt: z.string(),
    rpmUnlimited: z.boolean(),
    rpmLimit: z.number().int().min(1, t("errors.positive")).max(100_000),
    concurrencyUnlimited: z.boolean(),
    maxConcurrent: z.number().int().min(1, t("errors.positive")).max(1_024),
    billingUnlimited: z.boolean(),
    billingLimitUsd: z.number().min(0.01, t("errors.positive")).max(MAX_BILLING_LIMIT_USD),
    allowModelAliases: z.boolean(),
    modelScopeMode: z.enum(["all", "restricted"]),
    allowedModelIds: z.array(z.string()),
    providerScope: z.array(z.enum(["all", "grok_build", "grok_web", "grok_console"])).min(1),
    tierScope: z.array(z.enum(["all", "free", "super"])).min(1),
  }).superRefine((value, context) => {
    if (!value.expiryUnlimited && !value.expiresAt) {
      context.addIssue({ code: "custom", path: ["expiresAt"], message: t("errors.required") });
    }
  });
  type KeyForm = z.infer<typeof schema>;
  const form = useForm<KeyForm>({
    resolver: zodResolver(schema),
    defaultValues: editing === "new" ? { name: "", enabled: true, expiryUnlimited: true, expiresAt: "", rpmUnlimited: false, rpmLimit: 120, concurrencyUnlimited: false, maxConcurrent: 8, billingUnlimited: true, billingLimitUsd: 10, allowModelAliases: false, modelScopeMode: "all", allowedModelIds: [], providerScope: ["all"], tierScope: ["all"] } : {
      name: editing.name,
      enabled: editing.enabled,
      expiryUnlimited: !editing.expiresAt,
      expiresAt: toDateTimeLocal(editing.expiresAt),
      rpmUnlimited: editing.rpmLimit === 0,
      rpmLimit: editing.rpmLimit > 0 ? editing.rpmLimit : 120,
      concurrencyUnlimited: editing.maxConcurrent === 0,
      maxConcurrent: editing.maxConcurrent > 0 ? editing.maxConcurrent : 8,
      billingUnlimited: editing.billingLimitUsdTicks === 0,
      billingLimitUsd: editing.billingLimitUsdTicks > 0 ? editing.billingLimitUsdTicks / USD_TICKS : 10,
      allowModelAliases: editing.allowModelAliases,
      modelScopeMode: editing.modelScope,
      allowedModelIds: editing.allowedModelIds,
      providerScope: editing.providerScope ?? ["all"],
      tierScope: editing.tierScope ?? ["all"],
    },
  });
  const keyEnabled = useWatch({ control: form.control, name: "enabled" });
  const dirtyFields = form.formState.dirtyFields;
  const allowModelAliases = useWatch({ control: form.control, name: "allowModelAliases" });
  const modelScopeMode = useWatch({ control: form.control, name: "modelScopeMode" });
  const selectedModels = useWatch({ control: form.control, name: "allowedModelIds" });
  const providerScope = useWatch({ control: form.control, name: "providerScope" });
  const tierScope = useWatch({ control: form.control, name: "tierScope" });
  const expiryUnlimited = useWatch({ control: form.control, name: "expiryUnlimited" });
  const rpmUnlimited = useWatch({ control: form.control, name: "rpmUnlimited" });
  const concurrencyUnlimited = useWatch({ control: form.control, name: "concurrencyUnlimited" });
  const billingUnlimited = useWatch({ control: form.control, name: "billingUnlimited" });
  const modelProviderScope = providerScope.filter((value): value is Exclude<ProviderScopeValue, "all"> => value !== "all");
  const modelTierScope = tierScope.filter((value): value is Exclude<TierScopeValue, "all"> => value !== "all");
  const providerScopeSummary = providerScope.includes("all") ? t("keys.allProviders") : modelProviderScope.map((value) => ({ grok_build: "Build", grok_web: "Web", grok_console: "Console" })[value]).join(" · ");
  const tierScopeSummary = tierScope.includes("all") ? t("keys.allTiers") : modelTierScope.map((value) => value === "free" ? "Free" : "Super").join(" · ");
  const modelScopeSummary = modelScopeMode === "all" ? t("keys.allModels") : selectedModels.length === 0 ? t("keys.noAllowedModels") : t("keys.selectedModels", { count: selectedModels.length });

  const modelsQuery = useQuery({
    queryKey: ["models", "options", modelOptionsPage, debouncedModelOptionsSearch, modelProviderScope.join(","), modelTierScope.join(",")],
    queryFn: ({ signal }) => listModels({ page: modelOptionsPage, pageSize: 50, search: debouncedModelOptionsSearch, providerScope: modelProviderScope, tierScope: modelTierScope }, signal),
    enabled: modelScopeMode === "restricted",
  });
  const saveMutation = useLifetimeMutation<CreateKeyResponseDTO | ClientKeyDTO, KeyForm>({
    mutationFn: (values: KeyForm, signal) => {
      const body = {
        name: values.name,
        enabled: values.enabled,
        rpmLimit: values.rpmUnlimited ? 0 : values.rpmLimit,
        maxConcurrent: values.concurrencyUnlimited ? 0 : values.maxConcurrent,
        billingLimitUsdTicks: values.billingUnlimited ? 0 : Math.round(values.billingLimitUsd * USD_TICKS),
        allowModelAliases: values.allowModelAliases,
        modelScope: values.modelScopeMode,
        allowedModelIds: values.allowedModelIds,
        providerScope: values.providerScope,
        tierScope: values.tierScope,
        expiresAt: values.expiryUnlimited ? "" : new Date(values.expiresAt).toISOString(),
      };
      if (editing === "new") {
        return createClientKey(body, signal);
      }
      const dirty = dirtyFields;
      const patch: Partial<ClientKeyInput> = {};
      if (dirty.name) patch.name = body.name;
      if (dirty.enabled) patch.enabled = body.enabled;
      if (dirty.rpmUnlimited || dirty.rpmLimit) patch.rpmLimit = body.rpmLimit;
      if (dirty.concurrencyUnlimited || dirty.maxConcurrent) patch.maxConcurrent = body.maxConcurrent;
      if (dirty.billingUnlimited || dirty.billingLimitUsd) patch.billingLimitUsdTicks = body.billingLimitUsdTicks;
      if (dirty.allowModelAliases) patch.allowModelAliases = body.allowModelAliases;
      if (dirty.providerScope) patch.providerScope = body.providerScope;
      if (dirty.tierScope) patch.tierScope = body.tierScope;
      if (dirty.expiryUnlimited || dirty.expiresAt) patch.expiresAt = body.expiresAt;
      if (dirty.modelScopeMode || dirty.allowedModelIds) {
        patch.modelScope = body.modelScope;
        patch.allowedModelIds = body.allowedModelIds;
      }
      return updateClientKey(editing.id, patch, signal);
    },
    onSuccess: (result) => {
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      if ("secret" in result) {
        onCreated(result.secret);
        toast.success(t("keys.created"));
      } else {
        toast.success(t("keys.updated"));
      }
      onClose();
    },
    onError: (error) => showErrorToast(error, t),
  });
  function toggleModel(id: string): void {
    const current = form.getValues("allowedModelIds");
    form.setValue("allowedModelIds", current.includes(id) ? current.filter((value) => value !== id) : [...current, id], { shouldDirty: true, shouldValidate: true });
  }
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 text-xs sm:max-w-[560px]">
        <DialogHeader className="shrink-0 px-5 py-4 pr-12">
          <DialogTitle>{editing === "new" ? t("keys.createTitle") : t("keys.editTitle")}</DialogTitle>
          <DialogDescription>{editing === "new" ? t("keys.description") : editing.prefix}</DialogDescription>
        </DialogHeader>
        <form className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden" onSubmit={form.handleSubmit((values) => saveMutation.mutate(values))}>
          <div className="min-h-0 min-w-0 flex-1 space-y-3 overflow-y-auto overscroll-contain px-5 pb-4 pt-2">
            <div className="space-y-2"><Label htmlFor="key-name">{t("keys.name")}</Label><Input id="key-name" {...form.register("name")} />{form.formState.errors.name ? <p className="text-xs text-destructive">{form.formState.errors.name.message}</p> : null}</div>
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-2">
                <div className="flex items-center justify-between gap-3">
                  <Label htmlFor="key-rpm">{t("keys.rpm")}</Label>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">{t("keys.unlimited")}</span>
                    <Switch id="key-rpm-unlimited" checked={rpmUnlimited} onCheckedChange={(checked) => form.setValue("rpmUnlimited", checked, { shouldDirty: true })} />
                  </div>
                </div>
                <Input id="key-rpm" type="number" min="1" max="100000" disabled={rpmUnlimited} {...form.register("rpmLimit", { valueAsNumber: true })} />
              </div>
              <div className="space-y-2">
                <div className="flex items-center justify-between gap-3">
                  <Label htmlFor="key-concurrency">{t("keys.maxConcurrent")}</Label>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">{t("keys.unlimited")}</span>
                    <Switch id="key-concurrency-unlimited" checked={concurrencyUnlimited} onCheckedChange={(checked) => form.setValue("concurrencyUnlimited", checked, { shouldDirty: true })} />
                  </div>
                </div>
                <Input id="key-concurrency" type="number" min="1" max="1024" disabled={concurrencyUnlimited} {...form.register("maxConcurrent", { valueAsNumber: true })} />
              </div>
            </div>
            <div className="grid items-start gap-3 sm:grid-cols-2">
              <div className="space-y-2">
                <div className="flex h-5 items-center justify-between gap-3">
                  <div className="flex min-w-0 items-center gap-1.5">
                    <Label htmlFor="key-billing-unlimited">{t("keys.billingLimit")}</Label>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("keys.billingLimitDescription")}>
                          <CircleHelp className="size-3.5" />
                        </button>
                      </TooltipTrigger>
                      <TooltipContent className="max-w-72">{t("keys.billingLimitDescription")}</TooltipContent>
                    </Tooltip>
                  </div>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">{t("keys.unlimited")}</span>
                    <Switch id="key-billing-unlimited" checked={billingUnlimited} onCheckedChange={(checked) => form.setValue("billingUnlimited", checked, { shouldDirty: true })} />
                  </div>
                </div>
                <div className="relative">
                  <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-xs text-muted-foreground">$</span>
                  <Input className="pl-7" type="number" min="0.01" max={MAX_BILLING_LIMIT_USD} step="0.01" disabled={billingUnlimited} {...form.register("billingLimitUsd", { valueAsNumber: true })} />
                </div>
              </div>
              <div className="space-y-2">
                <div className="flex h-5 items-center justify-between gap-3">
                  <Label htmlFor="key-expiry-unlimited">{t("keys.expires")}</Label>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">{t("keys.unlimited")}</span>
                    <Switch id="key-expiry-unlimited" checked={expiryUnlimited} onCheckedChange={(checked) => {
                      form.setValue("expiryUnlimited", checked, { shouldDirty: true });
                      if (checked) form.clearErrors("expiresAt");
                    }} />
                  </div>
                </div>
                <Controller control={form.control} name="expiresAt" render={({ field }) => <DateTimePicker value={expiryUnlimited ? "" : field.value} onChange={field.onChange} disabled={expiryUnlimited} placeholder={expiryUnlimited ? t("common.neverExpires") : t("keys.selectExpiry")} />} />
                {form.formState.errors.expiresAt ? <p className="text-xs text-destructive">{form.formState.errors.expiresAt.message}</p> : null}
              </div>
            </div>
            <section className="flex items-center justify-between gap-4 rounded-lg bg-muted/25 px-3 py-2.5">
              <div className="min-w-0">
                <div className="flex min-w-0 items-center gap-1.5">
                  <Label htmlFor="key-model-aliases">{t("keys.modelAliases")}</Label>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("keys.modelAliasesDescription")}>
                        <CircleHelp className="size-3.5" />
                      </button>
                    </TooltipTrigger>
                    <TooltipContent className="max-w-96 space-y-2 py-2 text-left leading-relaxed">
                      <p>{t("keys.modelAliasesDescription")}</p>
                      <ul className="list-disc space-y-1 pl-4 text-primary-foreground/80">
                        <li>{t("keys.modelAliasesBuildSupport")}</li>
                        <li>{t("keys.modelAliasesConsoleSupport")}</li>
                        <li>{t("keys.modelAliasesUnsupported")}</li>
                      </ul>
                    </TooltipContent>
                  </Tooltip>
                </div>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{allowModelAliases ? t("keys.modelAliasesOn") : t("keys.modelAliasesOff")}</p>
              </div>
              <Switch className="shrink-0" id="key-model-aliases" checked={allowModelAliases} onCheckedChange={(checked) => form.setValue("allowModelAliases", checked, { shouldDirty: true })} />
            </section>
            <section className="flex items-center justify-between gap-4 rounded-lg bg-muted/25 px-3 py-2.5">
              <div className="min-w-0">
                <Label htmlFor="key-enabled">{keyEnabled ? t("common.enabled") : t("common.disabled")}</Label>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("keys.enabledDescription")}</p>
              </div>
              <Switch className="shrink-0" id="key-enabled" checked={keyEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked, { shouldDirty: true })} />
            </section>
            <div className="overflow-hidden rounded-lg border">
              <div className="divide-y">
                <div className="flex min-h-12 items-center gap-3 px-3">
                  <Label className="shrink-0">{t("keys.providerScope")}</Label>
                  <Controller control={form.control} name="providerScope" render={({ field }) => (
                    <ScopeDropdown
                      ariaLabel={t("keys.providerScope")}
                      summary={providerScopeSummary}
                      value={field.value}
                      onChange={(value) => {
                        field.onChange(value);
                        setModelOptionsPage(1);
                        if (modelScopeMode === "restricted" && selectedModels.length > 0) {
                          form.setValue("allowedModelIds", [], { shouldDirty: true, shouldValidate: true });
                          toast.info(t("keys.modelsClearedForScopeChange"));
                        }
                      }}
                      normalizeAllWhenComplete
                      options={[
                        { value: "grok_build", label: "Build" },
                        { value: "grok_web", label: "Web" },
                        { value: "grok_console", label: "Console" },
                      ]}
                    />
                  )} />
                </div>
                <div className="flex min-h-12 items-center gap-3 px-3">
                  <div className="flex shrink-0 items-center gap-1.5">
                    <Label>{t("keys.tierScope")}</Label>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("keys.accountScopeDescription")}>
                          <CircleHelp className="size-3.5" />
                        </button>
                      </TooltipTrigger>
                      <TooltipContent className="max-w-80 leading-relaxed">{t("keys.accountScopeDescription")}</TooltipContent>
                    </Tooltip>
                  </div>
                  <Controller control={form.control} name="tierScope" render={({ field }) => (
                    <ScopeDropdown
                      ariaLabel={t("keys.tierScope")}
                      summary={tierScopeSummary}
                      value={field.value}
                      onChange={(value) => {
                        field.onChange(value);
                        setModelOptionsPage(1);
                        if (modelScopeMode === "restricted" && selectedModels.length > 0) {
                          form.setValue("allowedModelIds", [], { shouldDirty: true, shouldValidate: true });
                          toast.info(t("keys.modelsClearedForScopeChange"));
                        }
                      }}
                      allLabel={t("keys.allTiersIncludingUnknown")}
                      options={[
                        { value: "free", label: "Free" },
                        { value: "super", label: "Super" },
                      ]}
                    />
                  )} />
                </div>
                <div className="flex min-h-12 items-center gap-3 px-3">
                  <Label className="shrink-0">{t("keys.modelScope")}</Label>
                  <Controller control={form.control} name="modelScopeMode" render={({ field }) => (
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button type="button" variant="ghost" size="sm" className="ml-auto h-8 min-w-0 max-w-[70%] justify-end gap-1.5 px-2 font-normal" aria-label={t("keys.modelScope")}>
                          <span className="truncate">{modelScopeSummary}</span>
                          <ChevronDown className="size-3.5 shrink-0 text-muted-foreground" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end" className="w-44">
                        <DropdownMenuRadioGroup value={field.value} onValueChange={(value) => {
                          field.onChange(value);
                          setModelOptionsPage(1);
                          if (value === "all") {
                            form.setValue("allowedModelIds", [], { shouldDirty: true, shouldValidate: true });
                            form.clearErrors("allowedModelIds");
                          }
                        }}>
                          <DropdownMenuRadioItem value="all">{t("keys.allModels")}</DropdownMenuRadioItem>
                          <DropdownMenuRadioItem value="restricted">{t("keys.restrictedModels")}</DropdownMenuRadioItem>
                        </DropdownMenuRadioGroup>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  )} />
                </div>
              </div>
              {modelScopeMode === "restricted" && selectedModels.length === 0 ? <p className="text-xs text-muted-foreground">{t("keys.noAllowedModelsHelp")}</p> : null}
              {modelScopeMode === "restricted" ? (
                <div className="min-w-0 border-t p-3">
                  <div className="min-w-0 overflow-hidden rounded-md bg-muted/25 p-1">
                    <div className="relative">
                      <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                      <Input className="bg-transparent pl-8 shadow-none focus-visible:bg-background/70" value={modelOptionsSearch} onChange={(event) => { setModelOptionsSearch(event.target.value); setModelOptionsPage(1); }} placeholder={t("keys.modelSearch")} aria-label={t("keys.modelSearch")} />
                    </div>
                    <div className="mt-1 max-h-40 overflow-y-auto overscroll-contain sm:max-h-44">
                      {modelsQuery.isPending ? <LoadingState className="min-h-20" /> : modelsQuery.data?.items.map((model) => {
                        const checked = selectedModels.includes(model.id);
                        const controlId = `allowed-model-${model.id}`;
                        return (
                          <label key={model.id} htmlFor={controlId} className={cn("flex h-8 cursor-pointer items-center gap-2.5 rounded-md px-2 text-xs transition-colors hover:bg-accent/40", checked && "bg-accent/55")}>
                            <Checkbox id={controlId} checked={checked} onCheckedChange={() => toggleModel(model.id)} aria-label={t("common.selectItem", { name: model.publicId })} />
                            <span className="min-w-0 flex-1 truncate" title={model.publicId}>{model.publicId}</span>
                            <span className="hidden max-w-[42%] shrink-0 truncate text-[11px] text-muted-foreground sm:block" title={model.upstreamModel}>{model.upstreamModel}</span>
                            {!model.enabled ? <Badge variant="secondary" className="shrink-0 text-[10px] font-normal text-muted-foreground">{t("common.disabled")}</Badge> : null}
                          </label>
                        );
                      })}
                      {modelsQuery.data?.items.length === 0 ? <p className="p-3 text-center text-xs text-muted-foreground">{t("common.noData")}</p> : null}
                    </div>
                    {modelsQuery.data && modelsQuery.data.total > modelsQuery.data.pageSize ? <ModelOptionPagination page={modelsQuery.data.page} pageSize={modelsQuery.data.pageSize} total={modelsQuery.data.total} onPageChange={setModelOptionsPage} /> : null}
                  </div>
                  {form.formState.errors.allowedModelIds ? <p className="mt-1.5 text-xs text-destructive">{form.formState.errors.allowedModelIds.message}</p> : null}
                </div>
              ) : null}
            </div>
          </div>
          <DialogFooter className="shrink-0 gap-2 bg-muted/20 px-5 py-3.5 sm:gap-0"><Button type="button" variant="secondary" size="sm" onClick={() => onClose()}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={saveMutation.isPending}>{saveMutation.isPending ? <Spinner /> : null}{editing === "new" ? t("common.create") : t("common.save")}</Button></DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function ModelOptionPagination({ page, pageSize, total, onPageChange }: { page: number; pageSize: number; total: number; onPageChange: (page: number) => void }) {
  const { t } = useTranslation();
  const pages = Math.max(1, Math.ceil(total / pageSize));
  return (
    <div className="flex h-9 items-center justify-between border-t bg-muted/20 px-2">
      <span className="px-1 text-xs text-muted-foreground">{t("common.pageOf", { page, pages })}</span>
      <div className="flex items-center gap-0.5">
        <Button type="button" variant="ghost" size="icon" className="size-7" disabled={page <= 1} onClick={() => onPageChange(page - 1)} aria-label={t("common.previousPage")}><ChevronLeft /></Button>
        <Button type="button" variant="ghost" size="icon" className="size-7" disabled={page >= pages} onClick={() => onPageChange(page + 1)} aria-label={t("common.nextPage")}><ChevronRight /></Button>
      </div>
    </div>
  );
}

function ScopeDropdown({ allLabel, ariaLabel, summary, value, onChange, options, normalizeAllWhenComplete = false }: { allLabel?: string; ariaLabel: string; summary: string; value: string[]; onChange: (value: string[]) => void; options: Array<{ value: string; label: string }>; normalizeAllWhenComplete?: boolean }) {
  function toggle(nextValue: string): void {
    if (nextValue === "all") {
      onChange(["all"]);
      return;
    }
    const current = value.includes("all") ? (allLabel ? [] : options.map((option) => option.value)) : value;
    const next = current.includes(nextValue) ? current.filter((item) => item !== nextValue) : [...current, nextValue];
    if (next.length === 0) {
      return;
    }
    if (normalizeAllWhenComplete && next.length === options.length) {
      onChange(["all"]);
      return;
    }
    onChange(next);
  }
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button type="button" variant="ghost" size="sm" className="ml-auto h-8 min-w-0 max-w-[70%] justify-end gap-1.5 px-2 font-normal" aria-label={ariaLabel}>
          <span className="truncate">{summary}</span>
          <ChevronDown className="size-3.5 shrink-0 text-muted-foreground" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-48">
        {allLabel ? (
          <>
            <DropdownMenuCheckboxItem checked={value.includes("all")} onCheckedChange={() => toggle("all")} onSelect={(event) => event.preventDefault()}>
              {allLabel}
            </DropdownMenuCheckboxItem>
            <DropdownMenuSeparator />
          </>
        ) : null}
        {options.map((option) => (
          <DropdownMenuCheckboxItem key={option.value} checked={!allLabel && value.includes("all") ? true : value.includes(option.value)} onCheckedChange={() => toggle(option.value)} onSelect={(event) => event.preventDefault()}>
            {option.label}
          </DropdownMenuCheckboxItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
