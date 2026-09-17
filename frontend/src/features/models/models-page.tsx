import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { TFunction } from "i18next";
import { AudioLines, Clapperboard, Image as ImageIcon, MessagesSquare, MessageSquareText, Mic, MoreHorizontal, Paintbrush, Pencil, Plus, Radio, RefreshCw, Search, SquareTerminal, Trash2 } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/shared/ui/alert-dialog";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Checkbox } from "@/shared/ui/checkbox";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/shared/ui/dropdown-menu";
import { Input } from "@/shared/ui/input";
import { Spinner } from "@/shared/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/shared/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { deleteModel, deleteModels, fetchModelSyncRun, listModelGroups, syncModels, updateModelsEnabled } from "@/entities/model/model-api";
import type { ModelEndpointCapability, ModelRouteDTO, ModelRouteGroupDTO } from "@/entities/model/types";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { ModelEditor } from "./model-editor";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";
import { showErrorToast } from "@/shared/lib/show-error";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const modelSyncToastID = "model-sync-progress";

export function ModelsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [providerFilter, setProviderFilter] = useState<ModelRouteDTO["provider"] | "">("");
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [editing, setEditing] = useState<ModelRouteDTO | "new" | null>(null);
  const [deleting, setDeleting] = useState<ModelRouteGroup | null>(null);
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const debouncedSearch = useDebouncedValue(search);

  const modelsQuery = useQuery({
    queryKey: ["models", "grouped", page, pageSize, debouncedSearch, statusFilter, providerFilter, sort.field, sort.order],
    queryFn: ({ signal }) => listModelGroups({ page, pageSize, search: debouncedSearch, status: statusFilter, provider: providerFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined }, signal),
    refetchOnMount: "always",
  });

  const deleteMutation = useLifetimeMutation({
    mutationFn: async (routes: ModelRouteDTO[], signal) => {
      if (routes.length === 1) await deleteModel(routes[0].id, signal);
      else await deleteModels(routes.map((route) => route.id), signal);
    },
    onSuccess: (_, routes) => {
      const ids = new Set(routes.map((route) => route.id));
      setSelected((current) => new Set([...current].filter((id) => !ids.has(id))));
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      setDeleting((current) => current?.routes.every((route) => ids.has(route.id)) ? null : current);
      setPage(1);
      toast.success(t("models.deleted"));
    },
    onError: showError,
  });

  const batchDeleteMutation = useLifetimeMutation({
    mutationFn: (ids: string[], signal) => deleteModels(ids, signal),
    onSuccess: (result, ids) => {
      const completed = new Set(ids);
      setSelected((current) => new Set([...current].filter((id) => !completed.has(id))));
      setPage(1);
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.batchDeleted", { count: result.deleted }));
    },
    onError: showError,
  });

  const batchUpdateMutation = useLifetimeMutation({
    mutationFn: ({ ids, enabled }: { ids: string[]; enabled: boolean }, signal) => updateModelsEnabled(ids, enabled, signal),
    onSuccess: (_, { ids }) => {
      const completed = new Set(ids);
      setSelected((current) => new Set([...current].filter((id) => !completed.has(id))));
      setPage(1);
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.batchUpdated"));
    },
    onError: showError,
  });

  const syncMutation = useLifetimeMutation({
    mutationFn: (_: void, signal) => {
      toast.loading(t("models.syncing"), { id: modelSyncToastID });
      return syncModels((progress) => {
        if (!signal.aborted) toast.loading(t("models.syncingProgress", progress), { id: modelSyncToastID });
      }, signal);
    },
    onSuccess: (result) => {
      setSelected(new Set());
      setPage(1);
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.synced", { count: result.synced }), { id: modelSyncToastID });
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : t("errors.generic"), { id: modelSyncToastID });
    },
  });

  // Sync-run recovery: the full-account sync is detached from the SSE
  // connection (a run over a very large account pool outlives browser
  // tabs). On mount, and
  // whenever this page regains focus, probe the shared snapshot and resume
  // displaying progress for an in-flight run the previous tab started.
  const syncRunQuery = useQuery({
    queryKey: ["models", "sync-run"],
    queryFn: ({ signal }) => fetchModelSyncRun(signal),
    refetchOnMount: "always",
    refetchInterval: (query) => (query.state.data?.active ? 2_000 : false),
    refetchOnWindowFocus: true,
    staleTime: 10_000,
  });
  // Drive effects from primitives, not the polled object identity: data
  // refreshes every 2s during a run and an object-keyed effect (or its
  // cleanup) would re-fire on every poll — dismissing toasts and refetching
  // the list for the entire multi-minute run.
  const resumedRunActive = syncRunQuery.data?.active === true && !syncMutation.isPending;
  const resumedCompleted = syncRunQuery.data?.completed ?? 0;
  const resumedTotal = syncRunQuery.data?.total ?? 0;
  const resumedFailed = syncRunQuery.data?.failed === true;
  const hasResumedRun = useRef(false);
  useEffect(() => {
    if (!resumedRunActive) return;
    hasResumedRun.current = true;
    toast.loading(t("models.syncingProgress", { completed: resumedCompleted, total: resumedTotal }), { id: modelSyncToastID });
  }, [resumedRunActive, resumedCompleted, resumedTotal, t]);
  useEffect(() => {
    if (resumedRunActive || !hasResumedRun.current) return;
    // A resumed run just finished: refresh the list once and surface the real
    // outcome — a detached run that failed must not report success.
    hasResumedRun.current = false;
    void queryClient.invalidateQueries({ queryKey: ["models"] });
    if (resumedFailed) toast.error(t("models.syncedRunFailed"), { id: modelSyncToastID });
    else toast.success(t("models.syncedFinish"), { id: modelSyncToastID });
  }, [resumedRunActive, resumedFailed, queryClient, t]);
  useEffect(() => () => {
    // Leaving the page mid-resume: drop the progress toast (the detached run
    // keeps going and any later visit resumes the display).
    toast.dismiss(modelSyncToastID);
  }, []);

  function showError(error: unknown): void {
    showErrorToast(error, t);
  }

  const result = useMemo(() => modelsQuery.data ? { ...modelsQuery.data, items: modelsQuery.data.items.map((group) => newModelRouteGroup(group, t)) } : undefined, [modelsQuery.data, t]);
  const pageIDs = result?.items.flatMap((group) => group.routes.map((route) => route.id)) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;
  const selectedGroupCount = result?.items.filter((group) => group.routes.some((route) => selected.has(route.id))).length ?? 0;

  function togglePage(checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      for (const id of pageIDs) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }

  function toggleModelGroup(routes: ModelRouteDTO[], checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      for (const route of routes) {
        if (checked) next.add(route.id);
        else next.delete(route.id);
      }
      return next;
    });
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
    setSelected(new Set());
  }

  return (
    <div className="space-y-5">
      <header className="flex min-h-8 items-center">
        <h1 className="text-xl font-medium">{t("models.title")}</h1>
        <p className="sr-only">{t("models.description")}</p>
      </header>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); setSelected(new Set()); }} placeholder={t("models.search")} aria-label={t("models.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "provider", label: t("models.provider"), value: providerFilter, onChange: (value) => { setProviderFilter(value as ModelRouteDTO["provider"] | ""); setPage(1); setSelected(new Set()); }, options: [
                  { value: "grok_build", label: t("models.providerGrokBuild") },
                  { value: "grok_web", label: t("models.providerGrokWeb") },
                  { value: "grok_console", label: t("console.name") },
                ] },
                { id: "status", label: t("models.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); setSelected(new Set()); }, options: [
                  { value: "enabled", label: t("common.enabled") },
                  { value: "disabled", label: t("common.disabled") },
                ] },
              ]} />
            </div>
            <div className="flex flex-wrap items-center gap-1.5">
              {selected.size > 0 ? (
                <>
                  <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selectedGroupCount })}</span>
                  <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate({ ids: [...selected], enabled: true })}>{t("common.enable")}</Button>
                  <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate({ ids: [...selected], enabled: false })}>{t("common.disable")}</Button>
                  <Button variant="secondary" size="sm" className="text-destructive hover:text-destructive" onClick={() => setBatchDeleteOpen(true)}>{t("common.delete")}</Button>
                </>
              ) : null}
              <Button variant="secondary" size="sm" disabled={syncMutation.isPending} onClick={() => syncMutation.mutate()}>
                {syncMutation.isPending ? <Spinner /> : <RefreshCw />}
                {t("models.sync")}
              </Button>
              <Button size="sm" onClick={() => setEditing("new")}><Plus />{t("models.create")}</Button>
            </div>
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={(value) => { setPage(value); setSelected(new Set()); }} onPageSizeChange={(value) => { setPageSize(value); setPage(1); setSelected(new Set()); }} /> : undefined}
      >
        {modelsQuery.isError ? <ErrorState message={modelsQuery.error.message} onRetry={() => void modelsQuery.refetch()} /> : null}
        {!modelsQuery.isPending && !modelsQuery.isError && result?.items.length === 0 ? <EmptyState /> : null}
        {modelsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={56} className="min-w-[1120px] table-fixed text-xs">
            <colgroup>
              <col className="w-10" />
              <col className="w-56" />
              <col className="w-32" />
              <col className="w-52" />
              <col className="w-24" />
              <col className="w-32" />
              <col className="w-40" />
              <col className="w-44" />
              <col className="w-10" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="px-2 text-center"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="publicId" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("models.model")}</SortableTableHead>
                <SortableTableHead field="upstreamModel" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("models.upstream")}</SortableTableHead>
                <TableHead className="text-center">{t("models.capability")}</TableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("models.status")}</SortableTableHead>
                <SortableTableHead field="provider" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("models.provider")}</SortableTableHead>
                <SortableTableHead field="accountSupport" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" onSort={changeSort}>{t("models.accountSupport")}</SortableTableHead>
                <SortableTableHead field="lastSyncedAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("models.lastSyncedAt")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {modelsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={9} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={9} rowHeight={56} renderRow={(model) => {
                const selectedRoutes = model.routes.filter((route) => selected.has(route.id)).length;
                return (
                <TableRow className="group h-14" key={model.key} data-state={selectedRoutes > 0 ? "selected" : undefined}>
                  <TableCell className="px-2 text-center"><Checkbox checked={selectedRoutes === model.routes.length ? true : selectedRoutes > 0 ? "indeterminate" : false} onCheckedChange={(checked) => toggleModelGroup(model.routes, checked === true)} aria-label={t("common.selectItem", { name: model.publicId })} /></TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate text-xs font-medium" title={model.publicId}>{model.publicId}</span>
                  </TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate text-xs text-muted-foreground" title={model.upstreamModel}>{model.upstreamModel}</span>
                  </TableCell>
                  <TableCell className="text-center"><ModelCapabilities capabilities={model.capabilities} />{model.routes.some((route) => route.capabilitySupported === false) ? <span className="mt-1 block text-[10px] text-destructive">{t("models.unsupportedCapability")}</span> : null}</TableCell>
                  <TableCell className="text-center">{model.enabledState === "enabled" ? <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("common.enabled")}</Badge> : model.enabledState === "disabled" ? <Badge variant="outline" className="text-muted-foreground">{t("common.disabled")}</Badge> : <Badge variant="outline" className="text-amber-700 dark:text-amber-300">{t("models.partiallyEnabled")}</Badge>}</TableCell>
                  <TableCell className="text-center"><ModelProvider provider={model.provider} /></TableCell>
                  <TableCell className="text-center text-xs">
                    <div title={model.supportTitle}>
                      <span className="inline-flex items-baseline gap-1 tabular-nums"><span className={cn("font-medium", model.supportedMax > 0 ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground")}>{model.supportedLabel}</span><span className="text-muted-foreground">/ {model.totalLabel}</span></span>
                      <span className="mt-0.5 block text-[10px] text-muted-foreground">{t(model.bindingState === "bound" ? "models.boundAccounts" : model.bindingState === "automatic" ? "models.automaticAccounts" : "models.mixedAccounts")}</span>
                    </div>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(model.lastSyncedAt, i18n.language)}</TableCell>
                  <TableActionCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        {model.routes.map((route) => <DropdownMenuItem key={route.id} onClick={() => setEditing(route)}><Pencil />{model.routes.length === 1 ? t("common.edit") : t("models.editCapability", { capability: capabilityLabel(route.capability, t) })}</DropdownMenuItem>)}
                        <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => setDeleting(model)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableActionCell>
                </TableRow>
                );
              }} />
            )}
          </Table>
        ) : null}
      </DataTableShell>

      {editing !== null && <ModelEditor key={editing === "new" ? "new" : editing.id} editing={editing} onClose={() => setEditing(null)} onSaved={() => setSelected(new Set())} />}

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("models.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t(deleting && deleting.routes.length > 1 ? "models.deleteGroupDescription" : "models.deleteDescription", { name: deleting?.publicId ?? "", count: deleting?.routes.length ?? 0 })}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending} onClick={() => deleting && deleteMutation.mutate(deleting.routes)}>{deleteMutation.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("models.batchDeleteTitle", { count: selectedGroupCount })}</AlertDialogTitle><AlertDialogDescription>{t("models.batchDeleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={batchDeleteMutation.isPending} onClick={() => batchDeleteMutation.mutate([...selected])}>{batchDeleteMutation.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ModelProvider({ provider }: { provider: ModelRouteDTO["provider"] }) {
  const { t } = useTranslation();
  const label = provider === "grok_web" ? t("models.providerGrokWeb") : provider === "grok_console" ? t("console.name") : t("models.providerGrokBuild");
  const color = provider === "grok_web" ? "bg-quota-product-2" : provider === "grok_console" ? "bg-quota-product-4" : "bg-quota-product-1";
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap text-xs text-muted-foreground">
      <span className={cn("size-2 rounded-full", color)} />
      {label}
    </span>
  );
}

function ModelCapabilities({ capabilities }: { capabilities: ModelDisplayCapability[] }) {
  const { t } = useTranslation();
  return (
    <span className="mx-auto inline-flex max-w-28 flex-wrap items-center justify-center gap-0.5">
      {capabilities.map((capability) => {
        const metadata = endpointCapabilityMetadata[capability];
        const Icon = metadata.icon;
        const label = displayCapabilityLabel(capability, t);
        return (
          <Tooltip key={capability}>
            <TooltipTrigger asChild>
              <span tabIndex={0} role="img" aria-label={label} className={cn("inline-flex size-5 cursor-help items-center justify-center rounded outline-none transition-colors hover:bg-muted focus-visible:bg-muted focus-visible:ring-2 focus-visible:ring-ring/40", metadata.color)}>
                <Icon className="size-3.5" strokeWidth={1.8} />
              </span>
            </TooltipTrigger>
            <TooltipContent className="space-y-0.5 text-left">
              <div className="font-medium">{label}</div>
              <code className="block text-[10px] text-primary-foreground/70">{metadata.method} {metadata.path}</code>
            </TooltipContent>
          </Tooltip>
        );
      })}
    </span>
  );
}

type ModelDisplayCapability = ModelEndpointCapability;

const endpointCapabilityMetadata = {
  completions: { icon: MessageSquareText, method: "POST", path: "/v1/chat/completions", color: "text-sky-600 dark:text-sky-400" },
  responses: { icon: SquareTerminal, method: "POST", path: "/v1/responses", color: "text-violet-600 dark:text-violet-400" },
  messages: { icon: MessagesSquare, method: "POST", path: "/v1/messages", color: "text-orange-600 dark:text-orange-400" },
  image: { icon: ImageIcon, method: "POST", path: "/v1/images/generations", color: "text-emerald-600 dark:text-emerald-400" },
  image_edit: { icon: Paintbrush, method: "POST", path: "/v1/images/edits", color: "text-amber-700 dark:text-amber-400" },
  video: { icon: Clapperboard, method: "POST", path: "/v1/videos/generations", color: "text-rose-600 dark:text-rose-400" },
  tts: { icon: AudioLines, method: "POST", path: "/v1/tts", color: "text-cyan-700 dark:text-cyan-400" },
  stt: { icon: Mic, method: "POST", path: "/v1/stt", color: "text-teal-700 dark:text-teal-400" },
  realtime: { icon: Radio, method: "GET", path: "/v1/realtime", color: "text-sky-700 dark:text-sky-400" },
} as const;

type ModelRouteGroup = {
  key: string;
  routes: ModelRouteDTO[];
  publicId: string;
  provider: ModelRouteDTO["provider"];
  upstreamModel: string;
  capabilities: ModelDisplayCapability[];
  enabledState: "enabled" | "disabled" | "mixed";
  bindingState: "automatic" | "bound" | "mixed";
  supportedMax: number;
  supportedLabel: string;
  totalLabel: string;
  supportTitle: string;
  lastSyncedAt?: string;
};

function newModelRouteGroup(value: ModelRouteGroupDTO, t: TFunction): ModelRouteGroup {
  const routes = value.routes;
  const first = routes[0];
  const enabledCount = routes.filter((route) => route.enabled).length;
  const boundCount = routes.filter((route) => route.bindingMode).length;
  const supportedValues = routes.map((route) => route.supportedAccounts);
  const totalValues = routes.map((route) => route.totalAccounts);
  const supportedMin = Math.min(...supportedValues);
  const supportedMax = Math.max(...supportedValues);
  const totalMin = Math.min(...totalValues);
  const totalMax = Math.max(...totalValues);
  const syncedTimes = routes.map((route) => route.lastSyncedAt).filter((value): value is string => Boolean(value)).sort();
  return {
    key: value.key,
    routes,
    publicId: first.publicId,
    provider: first.provider,
    upstreamModel: first.upstreamModel,
    capabilities: value.endpointCapabilities,
    enabledState: enabledCount === routes.length ? "enabled" : enabledCount === 0 ? "disabled" : "mixed",
    bindingState: boundCount === routes.length ? "bound" : boundCount === 0 ? "automatic" : "mixed",
    supportedMax,
    supportedLabel: supportedMin === supportedMax ? String(supportedMax) : `${supportedMin}–${supportedMax}`,
    totalLabel: totalMin === totalMax ? String(totalMax) : `${totalMin}–${totalMax}`,
    supportTitle: routes.map((route) => `${capabilityLabel(route.capability, t)}: ${t("models.supportSummary", { supported: route.supportedAccounts, total: route.totalAccounts })}`).join("\n"),
    lastSyncedAt: syncedTimes.at(-1),
  };
}

function capabilityLabel(capability: ModelRouteDTO["capability"], t: TFunction): string {
  if (capability === "responses" || capability === "chat") return t("models.capabilityConversation");
  return displayCapabilityLabel(capability, t);
}

function displayCapabilityLabel(capability: ModelDisplayCapability, t: TFunction): string {
  return {
    completions: t("models.capabilityCompletions"),
    responses: t("models.capabilityResponses"),
    messages: t("models.capabilityMessages"),
    image: t("models.capabilityImage"),
    image_edit: t("models.capabilityImageEdit"),
    video: t("models.capabilityVideo"),
    tts: t("models.capabilityTTS"),
    stt: t("models.capabilitySTT"),
    realtime: t("models.capabilityRealtime"),
  }[capability];
}
