import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { nodeCooling } from "@/features/proxies/operations-shared";
import { CircleAlert, CircleHelp, MoreHorizontal, Network, Pencil, Plus, Power, PowerOff, RefreshCw, Search, Trash2, Upload } from "lucide-react";
import { type ReactNode, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SubscriptionsPanel } from "@/features/proxies/subscriptions-panel";
import { ProbeSettingsButton } from "@/features/proxies/probe-settings";
import { Rss } from "lucide-react";
import { listEgressSources } from "@/features/settings/settings-api";
import { getSettings, batchSetEgressRotation, cleanupUnhealthyEgressNodes, createEgressNode, deleteEgressNode, deleteEgressNodes, getEgressNodeProxyURL, getEgressNodeRotationURL, importEgressText, listAllEgressNodes, listEgressNodes, previewUnhealthyEgressNodes, refreshEgressClearance, rotateEgressNode, testEgressNode, testEgressNodes, updateEgressNode, updateEgressNodesEnabled, type ClearanceMode, type EgressNodeDTO, type EgressNodeInput } from "@/features/settings/settings-api";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const emptyInput: EgressNodeInput = { name: "", enabled: true, proxyPool: false, proxyURL: "", rotationURL: "", rotationEnabled: false };
// rotationOpen is the dialog-local switch state (single source of truth for
// 支持更换IP); rotationURL/rotationEnabled derive from it at save time.
type NodeForm = EgressNodeInput & { rotationOpen: boolean };
const emptyForm: NodeForm = { ...emptyInput, rotationOpen: false };
type ImportForm = { name: string; content: string };
const emptyImport: ImportForm = { name: "", content: "" };

// Batched probing keeps the admin HTTP timeout safe: each node probes IPv4
// and IPv6 in parallel with a 15-second ceiling.
const egressProbeBatchSize = 32;

async function testAllEgressNodes() {
  const nodes = await listAllEgressNodes();
  const ids = nodes.items.filter((node) => node.enabled && node.proxyConfigured).map((node) => node.id);
  return probeAllEnabledNodes(ids);
}

async function probeAllEnabledNodes(ids: string[]) {
  const result = { requested: 0, healthy: 0, unhealthy: 0, failed: 0 };
  let firstError: unknown;
  for (let index = 0; index < ids.length; index += egressProbeBatchSize) {
    const batchIDs = ids.slice(index, index + egressProbeBatchSize);
    try {
      const batch = await testEgressNodes(batchIDs);
      result.requested += batch.requested;
      result.healthy += batch.healthy;
      result.unhealthy += batch.unhealthy;
    } catch (error) {
      firstError ??= error;
      result.failed += batchIDs.length;
    }
  }
  if (result.requested === 0 && result.failed > 0) {
    throw firstError;
  }
  return result;
}

export function NodesPanel() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  // Clearance mode lives in the Grok Web settings form; the nodes table only
  // reads it to decide whether the per-row "refresh clearance" action applies.
  const settingsQuery = useQuery({ queryKey: ["settings"], queryFn: getSettings, staleTime: 30_000 });
  const clearanceMode: ClearanceMode = settingsQuery.data?.config.providerWeb.clearanceMode ?? "manual";
  const [editing, setEditing] = useState<EgressNodeDTO | null | undefined>(undefined);
  const [revealedProxyURL, setRevealedProxyURL] = useState("");
  const [revealedRotationURL, setRevealedRotationURL] = useState("");
  const [importOpen, setImportOpen] = useState(false);
  const [sourcesOpen, setSourcesOpen] = useState(false);
  const [importForm, setImportForm] = useState<ImportForm>(emptyImport);
  const [form, setForm] = useState<NodeForm>(emptyForm);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [search, setSearch] = useState("");
  const [enabledFilter, setEnabledFilter] = useState("");
  const [probeFilter, setProbeFilter] = useState("");
  const [selected, setSelected] = useState<Map<string, EgressNodeDTO>>(() => new Map());
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [cleanupOpen, setCleanupOpen] = useState(false);
  const debouncedSearch = useDebouncedValue(search);
  // Node mutations ripple into pool member counts, subscription summaries and
  // shared proxy profiles; one helper keeps every invalidation site in sync.
  const invalidateAfterNodeChange = () => {
    void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-pools"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-sources"] });
  };
  const query = useQuery({
    queryKey: ["egress-nodes", "page", page, pageSize, debouncedSearch, enabledFilter, probeFilter, sort.field, sort.order],
    queryFn: ({ signal }) => listEgressNodes({
      page, pageSize, search: debouncedSearch, enabled: enabledFilter,
      probe: probeFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined,
    }, signal),
    staleTime: 15_000,
    refetchOnWindowFocus: true,
  });
  const save = useMutation({
    mutationFn: () => {
      const normalizedProxyURL = form.proxyURL?.trim() || "";
      const trimmedRotationURL = form.rotationOpen ? form.rotationURL?.trim() || "" : "";
      const formFields: EgressNodeInput = { name: form.name, enabled: form.enabled, proxyPool: form.proxyPool, proxyURL: form.proxyURL, rotationURL: form.rotationURL, rotationEnabled: form.rotationEnabled };
      const input: EgressNodeInput = {
        ...formFields,
        proxyURL: normalizedProxyURL && (!editing || normalizedProxyURL !== revealedProxyURL) ? normalizedProxyURL : undefined,
        clearProxyURL: !normalizedProxyURL && revealedProxyURL ? true : undefined,
        // 换 IP webhook 与代理地址同语义:仅在确认已有存量(reveal 已返回)且被清空时
        // 才显式清除;reveal 未返回/失败时保持存量, 避免快速保存把已配置的 webhook
        // 静默删除。开关关闭=暂停(保留 webhook), 未触及=不修改。
        rotationURL: form.rotationOpen && trimmedRotationURL && trimmedRotationURL !== revealedRotationURL ? trimmedRotationURL : undefined,
        clearRotationURL: form.rotationOpen && !trimmedRotationURL && revealedRotationURL ? true : undefined,
        rotationEnabled: form.rotationOpen ? (trimmedRotationURL ? true : revealedRotationURL ? false : undefined) : false,
      };
      return editing ? updateEgressNode(editing.id, input) : createEgressNode(input);
    },
    onSuccess: () => { invalidateAfterNodeChange(); setEditing(undefined); toast.success(t("settings.egress.saved")); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const importText = useMutation({
    mutationFn: () => importEgressText(importForm),
    onSuccess: (value) => { invalidateAfterNodeChange(); setImportOpen(false); toast.success(t("settings.egress.imported", value)); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const remove = useMutation({
    mutationFn: deleteEgressNode,
    onSuccess: (_, id) => {
      if (page > 1 && query.data?.items.length === 1) setPage(page - 1);
      setSelected((current) => {
        const next = new Map(current);
        next.delete(id);
        return next;
      });
      invalidateAfterNodeChange();
      toast.success(t("settings.egress.deleted"));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const removeMany = useMutation({
    mutationFn: () => deleteEgressNodes([...selected.keys()]),
    onSuccess: (value) => {
      const selectedOnCurrentPage = query.data?.items.filter((node) => selected.has(node.id)).length ?? 0;
      if (page > 1 && query.data && query.data.items.length > 0 && selectedOnCurrentPage === query.data.items.length) setPage(page - 1);
      setSelected(new Map());
      setBatchDeleteOpen(false);
      invalidateAfterNodeChange();
      toast.success(t("settings.egress.batchDeleted", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const updateManyEnabled = useMutation({
    mutationFn: (enabled: boolean) => updateEgressNodesEnabled([...selected.keys()], enabled),
    onSuccess: (value, enabled) => {
      setSelected(new Map());
      invalidateAfterNodeChange();
      toast.success(t(enabled ? "settings.egress.batchEnabled" : "settings.egress.batchDisabled", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const cleanupPreview = useMutation({
    mutationFn: previewUnhealthyEgressNodes,
  });
  const cleanupUnhealthy = useMutation({
    mutationFn: cleanupUnhealthyEgressNodes,
    onSuccess: (value) => {
      setPage(1);
      setSelected(new Map());
      setCleanupOpen(false);
      cleanupPreview.reset();
      invalidateAfterNodeChange();
      toast.success(t("settings.egress.cleanupUnavailableComplete", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const refreshClearance = useMutation({
    mutationFn: (id: string) => refreshEgressClearance(id),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); toast.success(t("settings.egress.clearanceRefreshed")); },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
  });
  const rotateNode = useMutation({
    mutationFn: (id: string) => rotateEgressNode(id),
    onSuccess: () => { toast.success(t("settings.egress.rotateQueued")); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const [batchRotationOpen, setBatchRotationOpen] = useState(false);
  const [batchRotationTemplate, setBatchRotationTemplate] = useState("");

  const batchRotation = useMutation({
    mutationFn: () => batchSetEgressRotation([...selected.keys()], batchRotationTemplate),
    onSuccess: (value) => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); setBatchRotationOpen(false); toast.success(t("settings.egress.batchRotationDone", value)); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const testAll = useMutation({
    mutationFn: testAllEgressNodes,
    onSuccess: (value) => {
      if (value.failed > 0) toast.warning(t("settings.egress.testedPartial", value));
      else toast.success(t("settings.egress.tested", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
    onSettled: () => { invalidateAfterNodeChange(); },
  });
  const testNode = useMutation({
    mutationFn: testEgressNode,
    onSuccess: (result) => {
      if (result.status === "healthy") toast.success(t("settings.egress.testedOne"));
      else toast.error(result.error || t("settings.egress.operationFailed"));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
    onSettled: () => { invalidateAfterNodeChange(); },
  });

  function openCreate() {
    setRevealedProxyURL("");
    setRevealedRotationURL("");
    setForm(emptyForm);
    setEditing(null);
  }

  function openCleanup() {
    cleanupPreview.reset();
    setCleanupOpen(true);
    cleanupPreview.mutate();
  }

  function openEdit(node: EgressNodeDTO) {
    setForm({ name: node.name, enabled: node.enabled, proxyPool: node.proxyPool, proxyURL: "", rotationURL: "", rotationEnabled: node.rotationConfigured && (node.rotationEnabled ?? true), rotationOpen: node.rotationConfigured && (node.rotationEnabled ?? true) });
    setRevealedProxyURL("");
    setRevealedRotationURL("");
    setEditing(node);
    // Proxy address and rotation webhook are plain editable fields: fetch the
    // stored values once so operators can see and delete them without a
    // reveal dance. Shared-profile nodes keep their managed address hidden.
    if (node.proxyConfigured) {
      getEgressNodeProxyURL(node.id)
        .then(({ proxyURL }) => {
          setRevealedProxyURL(proxyURL);
          setForm((current) => (current.proxyURL === "" ? { ...current, proxyURL } : current));
        })
        .catch(() => undefined);
    }
    if (node.rotationConfigured) {
      getEgressNodeRotationURL(node.id)
        .then(({ rotationURL }) => {
          setRevealedRotationURL(rotationURL);
          setForm((current) => (current.rotationURL === "" ? { ...current, rotationURL } : current));
        })
        .catch(() => undefined);
    }
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  function togglePage(checked: boolean): void {
    setSelected((current) => {
      const next = new Map(current);
      for (const node of nodes) {
        if (checked) next.set(node.id, node);
        else next.delete(node.id);
      }
      return next;
    });
  }

  function toggleNode(node: EgressNodeDTO, checked: boolean): void {
    setSelected((current) => {
      const next = new Map(current);
      if (checked) next.set(node.id, node);
      else next.delete(node.id);
      return next;
    });
  }

  const nodes = query.data?.items ?? [];
  const selectedOnPage = nodes.filter((node) => selected.has(node.id));
  const allPageSelected = nodes.length > 0 && selectedOnPage.length === nodes.length;
  const selectedNodes = [...selected.values()];
  const selectedSourceNodes = selectedNodes.filter((node) => node.sourceId).length;
  const batchPending = removeMany.isPending || updateManyEnabled.isPending;
  const hasActiveFilters = Boolean(debouncedSearch || enabledFilter || probeFilter);

  const sourcesQuery = useQuery({ queryKey: ["egress-sources"], queryFn: () => listEgressSources(), staleTime: 15_000 });
  const sourceCount = sourcesQuery.data?.items.length ?? 0;

  return (
    <div className="space-y-5">
      <DataTableShell
          toolbar={(
            <>
              <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">
                <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                  <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                  <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); setSelected(new Map()); }} placeholder={t("settings.egress.search")} aria-label={t("settings.egress.search")} />
                </div>
                <DataTableFilters filters={[
                  { id: "enabled", label: t("settings.egress.enabled"), value: enabledFilter, onChange: (value) => { setEnabledFilter(value); setPage(1); setSelected(new Map()); }, options: [
                    { value: "enabled", label: t("common.enable") },
                    { value: "disabled", label: t("common.disable") },
                  ] },
                  { id: "probe", label: t("settings.egress.probe"), value: probeFilter, onChange: (value) => { setProbeFilter(value); setPage(1); setSelected(new Map()); }, options: [
                    { value: "healthy", label: t("settings.egress.healthy") },
                    { value: "unhealthy", label: t("settings.egress.unhealthy") },
                    { value: "unknown", label: t("settings.egress.notTested") },
                  ] },
                ]} />
              </div>
              <div className="flex flex-wrap items-center gap-1.5">
                {selected.size > 0 ? (
                  <>
                    <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                    <Button type="button" size="sm" variant="secondary" disabled={batchPending || selectedNodes.every((node) => node.enabled)} onClick={() => updateManyEnabled.mutate(true)}><Power />{t("common.enable")}</Button>
                    <Button type="button" size="sm" variant="secondary" disabled={batchPending} onClick={() => { setBatchRotationTemplate(""); setBatchRotationOpen(true); }}>{t("settings.egress.batchRotation")}</Button>
                    <Button type="button" size="sm" variant="secondary" disabled={batchPending || selectedNodes.every((node) => !node.enabled)} onClick={() => updateManyEnabled.mutate(false)}><PowerOff />{t("common.disable")}</Button>
                    <Button type="button" size="sm" variant="secondary" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={batchPending} onClick={() => setBatchDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
                  </>
                ) : null}
                <Button type="button" size="icon" variant="secondary" className="size-8" disabled={query.isFetching} onClick={() => void query.refetch()} aria-label={t("common.refresh")} title={t("common.refresh")}><RefreshCw className={cn(query.isFetching && "animate-spin")} /></Button>
                <Button type="button" size="sm" variant="secondary" disabled={testAll.isPending} onClick={() => testAll.mutate()}>{testAll.isPending ? <Spinner /> : <Network />}{t("settings.egress.testAll")}</Button>
                <ProbeSettingsButton />
                <Button type="button" size="sm" variant="secondary" onClick={() => setSourcesOpen(true)}><Rss />{sourceCount > 0 ? t("proxies.supply.manageWithCount", { count: sourceCount }) : t("proxies.supply.manage")}</Button>
                <Button type="button" size="sm" variant="secondary" disabled={cleanupUnhealthy.isPending} onClick={openCleanup}><Trash2 />{t("settings.egress.cleanupUnavailable")}</Button>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild><Button type="button" size="sm" variant="secondary"><Plus />{t("settings.egress.add")}</Button></DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuItem onClick={openCreate}><Plus />{t("settings.egress.addManually")}</DropdownMenuItem>
                    <DropdownMenuItem onClick={() => { setImportForm(emptyImport); setImportOpen(true); }}><Upload />{t("settings.egress.importText")}</DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </>
          )}
          footer={query.data && query.data.total > 0 ? <Pagination page={query.data.page} pageSize={query.data.pageSize} total={query.data.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
        >
          {query.isError ? <ErrorState message={query.error.message} onRetry={() => void query.refetch()} /> : null}
          {!query.isError ? <Table viewportRows={10} rowHeight={48} className="min-w-[920px] table-fixed">
          <TableHeader><TableRow className="hover:bg-transparent"><TableHead className="w-10 px-2"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} disabled={nodes.length === 0} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead><SortableTableHead className="w-40 text-center" field="name" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.name")}</SortableTableHead><TableHead className="w-24 text-center">{t("proxies.nodes.source")}</TableHead><TableHead className="w-28 text-center">{t("settings.egress.proxy")}</TableHead><TableHead className="w-32 text-center">{t("proxies.nodes.exitIP")}</TableHead><TableHead className="w-20 text-center">{t("proxies.nodes.latency")}</TableHead><SortableTableHead className="w-20 text-center" field="health" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" onSort={changeSort} title={t("settings.egress.healthHelp")}>{t("settings.egress.health")}</SortableTableHead><TableHead className="w-20 text-center">{t("proxies.nodes.rotatable")}</TableHead><TableActionHead /></TableRow></TableHeader>
          {query.isPending ? <TableBody><TableLoadingRow colSpan={9} /></TableBody> : null}
          {!query.isPending && nodes.length === 0 ? <TableBody><TableRow><TableCell colSpan={9} className="h-24 text-center text-xs text-muted-foreground">{hasActiveFilters ? t("settings.egress.noMatches") : t("settings.egress.directFallback")}</TableCell></TableRow></TableBody> : null}
          {!query.isPending && nodes.length > 0 ? <VirtualTableBody items={nodes} colSpan={9} rowHeight={48} renderRow={(node) => (
              <TableRow className="group h-12" key={node.id} data-state={selected.has(node.id) ? "selected" : undefined}>
                <TableCell className="px-2"><Checkbox checked={selected.has(node.id)} onCheckedChange={(checked) => toggleNode(node, checked === true)} aria-label={t("common.selectItem", { name: node.name })} /></TableCell>
                <TableCell className="max-w-44 text-center">
                  <div className="flex min-w-0 items-center justify-center gap-1.5">
                    <span className={cn("size-1.5 shrink-0 rounded-full", node.enabled ? "bg-emerald-500" : "bg-muted-foreground/35")} />
                    <span className={cn("truncate text-xs font-medium", !node.enabled && "text-muted-foreground")} title={node.name}>{node.name}</span>
                    {node.lastError ? <ErrorTooltip message={node.lastError} /> : null}
                    {node.degradeCount > 0 ? <Badge variant="outline" className="shrink-0 border-amber-500/40 text-[10px] text-amber-600" title={t("settings.egress.degradeBadgeHelp", { count: node.degradeCount })}>IP×{node.degradeCount}</Badge> : null}
                    {node.rotationConfigured && node.rotationAttempts > 0 ? <Badge variant="outline" className="shrink-0 text-[10px] text-muted-foreground" title={node.lastRotationError || t("settings.egress.rotationBadgeHelp")}>{t("settings.egress.rotationBadge", { count: node.rotationAttempts })}</Badge> : null}
                    {nodeCooling(node) ? <Badge variant="outline" className="shrink-0 border-amber-500/40 text-[10px] text-amber-600" title={t("proxies.nodes.coolingBadgeHelp", { until: new Date(node.cooldownUntil as string).toLocaleString() })}>{t("proxies.nodes.coolingBadge")}</Badge> : null}
                    {!node.enabled ? <Badge variant="outline" className="shrink-0 text-[10px] text-muted-foreground">{t("proxies.nodes.disabledBadge")}</Badge> : null}
                  </div>
                </TableCell>
                <TableCell className="text-center">
                  {node.sourceName ? <span className="text-[10px] text-muted-foreground" title={t("proxies.nodes.fromSubscription", { name: node.sourceName })}>{node.sourceName}</span> : <span className="text-[10px] text-muted-foreground/70">{t("proxies.nodes.manual")}</span>}
                </TableCell>
                <TableCell className="text-center">
                  {node.proxyConfigured
                    ? <span className="font-mono text-[10px] text-muted-foreground" title={node.proxyDisplay || t("settings.egress.configured")}>#{node.proxyFingerprint || "proxy"}</span>
                    : <Badge variant="outline" className="text-[10px] text-muted-foreground">{t("settings.egress.direct")}</Badge>}
                </TableCell>
                <TableCell className="text-center"><ExitIPCell node={node} /></TableCell>
                <TableCell className="text-center"><LatencyCell node={node} /></TableCell>
                <TableCell className="text-center"><HealthCell node={node} /></TableCell>
                <TableCell className="text-center"><RotatableCell node={node} /></TableCell>
                <TableActionCell>
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuItem onClick={() => openEdit(node)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                      <DropdownMenuSeparator />
                      {clearanceMode !== "manual" && !node.accountBoundProxy ? <DropdownMenuItem disabled={refreshClearance.isPending} onClick={() => refreshClearance.mutate(node.id)}><RefreshCw />{t("settings.egress.refreshClearance")}</DropdownMenuItem> : null}
                      <DropdownMenuItem disabled={testNode.isPending || !node.proxyConfigured} onClick={() => testNode.mutate(node.id)}><RefreshCw />{t("settings.egress.test")}</DropdownMenuItem>
                      <DropdownMenuItem disabled={!node.rotationConfigured} onClick={() => rotateNode.mutate(node.id)}><RefreshCw />{t("settings.egress.rotateExitIP")}</DropdownMenuItem>
                      <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => remove.mutate(node.id)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableActionCell>
              </TableRow>
            )} /> : null}
          </Table>
          : null}
        </DataTableShell>

      <Dialog open={sourcesOpen} onOpenChange={setSourcesOpen}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[640px]">
          <DialogHeader>
            <DialogTitle>{t("proxies.supply.title")}</DialogTitle>
          </DialogHeader>
          <SubscriptionsPanel showHeader={false} />
        </DialogContent>
      </Dialog>

      <Dialog open={batchRotationOpen} onOpenChange={setBatchRotationOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t("settings.egress.batchRotation")}</DialogTitle>
            <DialogDescription>{t("settings.egress.batchRotationDescription", { count: selected.size })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Input value={batchRotationTemplate} placeholder="http://203.0.113.10:9000/rotate/{port}?token=xxx" onChange={(event) => setBatchRotationTemplate(event.target.value)} />
            <p className="text-xs leading-5 text-muted-foreground">{t("settings.egress.batchRotationHelp")}</p>
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" onClick={() => setBatchRotationOpen(false)}>{t("common.cancel")}</Button>
            <Button type="button" size="sm" disabled={batchRotation.isPending} onClick={() => batchRotation.mutate()}>{batchRotation.isPending ? <Spinner /> : null}{t("common.save")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("settings.egress.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription className="space-y-1">
              <span className="block">{t("settings.egress.batchDeleteDescription", { count: selected.size })}</span>
              {selectedSourceNodes > 0 ? <span className="block">{t("settings.egress.batchDeleteSourceHint", { count: selectedSourceNodes })}</span> : null}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel disabled={removeMany.isPending}>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={removeMany.isPending} onClick={(event) => { event.preventDefault(); removeMany.mutate(); }}>{removeMany.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={cleanupOpen} onOpenChange={(open) => {
        if (!open && cleanupUnhealthy.isPending) return;
        if (!open) cleanupPreview.reset();
        setCleanupOpen(open);
      }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("settings.egress.cleanupUnavailableTitle")}</AlertDialogTitle>
            <AlertDialogDescription className="space-y-2">
              <span className="block">{t("settings.egress.cleanupUnavailableDescription")}</span>
              <span className="block">{t("settings.egress.cleanupUnavailableImpact")}</span>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="min-h-20 rounded-md bg-muted/45 p-3 text-xs">
            {cleanupPreview.isPending ? <div className="flex h-14 items-center justify-center gap-2 text-muted-foreground"><Spinner />{t("settings.egress.cleanupPreviewLoading")}</div> : null}
            {cleanupPreview.isError ? <div className="flex h-14 items-center justify-center text-center text-destructive">{t("settings.egress.cleanupPreviewFailed")}</div> : null}
            {cleanupPreview.data ? (
              <div className="grid grid-cols-2 gap-3 text-center">
                <CleanupPreviewValue label={t("settings.egress.cleanupNodeCount")} value={cleanupPreview.data.nodes} />
                <CleanupPreviewValue label={t("settings.egress.cleanupSubscriptionCount")} value={cleanupPreview.data.subscriptionManaged} />
              </div>
            ) : null}
          </div>
          {cleanupPreview.data && cleanupPreview.data.subscriptionManaged > 0 ? <p className="text-xs leading-5 text-amber-700 dark:text-amber-300">{t("settings.egress.cleanupSubscriptionHint")}</p> : null}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={cleanupUnhealthy.isPending}>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              disabled={cleanupUnhealthy.isPending || !cleanupPreview.data || cleanupPreview.data.nodes === 0}
              onClick={(event) => { event.preventDefault(); cleanupUnhealthy.mutate(); }}
            >
              {cleanupUnhealthy.isPending ? <Spinner /> : null}{t("settings.egress.cleanupUnavailableConfirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={editing !== undefined} onOpenChange={(open) => { if (!open) setEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
          <DialogHeader className="pr-8">
            <DialogTitle>{editing ? t("settings.egress.editTitle") : t("settings.egress.addTitle")}</DialogTitle>
            <DialogDescription>{t("settings.egress.dialogDescription")}</DialogDescription>
          </DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); save.mutate(); }}>
            <div className="flex items-center justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
              <Label htmlFor="egress-enabled">{t("settings.egress.enabled")}</Label>
              <Switch id="egress-enabled" checked={form.enabled} onCheckedChange={(enabled) => setForm({ ...form, enabled })} />
            </div>
            <Field label={t("settings.egress.name")} controlId="egress-name">
              <Input id="egress-name" maxLength={160} value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} />
            </Field>
                        <Field label={t("settings.egress.proxyURL")} controlId="egress-proxy" help={t("settings.egress.proxyProtocols")}>
              <div className="flex gap-2">
              <Input id="egress-proxy" type="text" autoComplete="off" className="h-8 flex-1 text-xs" placeholder="socks5h://user:pass@host:port" value={form.proxyURL} onChange={(event) => {
                const proxyURL = event.target.value;
                const hasProxy = Boolean(editing?.proxyConfigured) || Boolean(proxyURL.trim());
                setForm({ ...form, proxyURL, proxyPool: hasProxy ? form.proxyPool : false, rotationURL: hasProxy ? form.rotationURL : "", rotationOpen: hasProxy ? form.rotationOpen : false, rotationEnabled: hasProxy ? form.rotationEnabled : false });
              }} />
              </div>
            </Field>
            <div className="grid grid-cols-2 gap-3">
              <div className="flex h-8 items-center justify-between gap-3 rounded-md bg-secondary/55 px-3">
                <Label htmlFor="egress-proxy-pool">{t("settings.egress.proxyPool")}</Label>
                <Switch id="egress-proxy-pool" checked={form.proxyPool} disabled={!editing?.proxyConfigured && !form.proxyURL?.trim()} onCheckedChange={(proxyPool) => setForm({ ...form, proxyPool })} />
              </div>
              <div className="flex h-8 items-center justify-between gap-3 rounded-md bg-secondary/55 px-3">
                <Label htmlFor="egress-rotation-enabled" className="flex items-center gap-1.5">
                  {t("settings.egress.rotationSupport")}
                  <Tooltip>
                    <TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("settings.egress.rotationHelp")}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
                    <TooltipContent className="max-w-80 whitespace-pre-line">{t("settings.egress.rotationHelp")}</TooltipContent>
                  </Tooltip>
                </Label>
                <Switch id="egress-rotation-enabled" checked={form.rotationOpen} disabled={!editing?.proxyConfigured && !form.proxyURL?.trim()} onCheckedChange={(rotationOpen) => { if (!rotationOpen) setForm({ ...form, rotationOpen: false, rotationURL: "", rotationEnabled: false }); else setForm({ ...form, rotationOpen: true }); }} />
              </div>
            </div>
            {form.rotationOpen ? (
              <Field label={t("settings.egress.webhook")} controlId="egress-rotation">
                <Input id="egress-rotation" type="text" autoComplete="off" className="h-8 text-xs" placeholder={editing?.rotationConfigured && !form.rotationURL?.trim() ? t("settings.egress.keepConfigured") : "https://server-b:9000/rotate/{port}?token=..."} value={form.rotationURL} onChange={(event) => { const rotationURL = event.target.value.replace(/^\s+|\s+$/g, ""); setForm({ ...form, rotationURL, rotationEnabled: Boolean(rotationURL) }); }} />
              </Field>
            ) : null}
            <DialogFooter>
              <Button type="button" variant="secondary" size="sm" onClick={() => setEditing(undefined)}>{t("common.cancel")}</Button>
              <Button type="submit" size="sm" disabled={!form.name.trim() || save.isPending}>{save.isPending ? <Spinner /> : null}{t("common.save")}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[620px]">
          <DialogHeader className="pr-8"><DialogTitle>{t("settings.egress.importText")}</DialogTitle><DialogDescription>{t("settings.egress.importDialogDescription")}</DialogDescription></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); importText.mutate(); }}>
            <Field label={t("settings.egress.name")} controlId="egress-import-name"><Input id="egress-import-name" maxLength={160} value={importForm.name} onChange={(event) => setImportForm({ ...importForm, name: event.target.value })} /></Field>
            <Field label={t("settings.egress.proxyList")} controlId="egress-import-list"><Textarea className="min-h-52 font-mono text-xs" id="egress-import-list" value={importForm.content} onChange={(event) => setImportForm({ ...importForm, content: event.target.value })} /></Field>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setImportOpen(false)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!importForm.name.trim() || !importForm.content.trim() || importText.isPending}>{importText.isPending ? <Spinner /> : null}{t("settings.egress.importText")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

    </div>
  );
}


function CleanupPreviewValue({ label, value }: { label: string; value: number }) {
  return (
    <div className="space-y-1">
      <div className="text-base font-medium tabular-nums text-foreground">{value}</div>
      <div className="text-muted-foreground">{label}</div>
    </div>
  );
}

function ErrorTooltip({ message }: { message: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex shrink-0 cursor-help text-destructive" tabIndex={0} aria-label={message}><CircleAlert className="size-3.5" /></span>
      </TooltipTrigger>
      <TooltipContent className="max-w-80">{message}</TooltipContent>
    </Tooltip>
  );
}

/** Health column: rolling success ratio of real requests through this node. */
function HealthCell({ node }: { node: EgressNodeDTO }) {
  const percent = Math.max(0, Math.min(100, Math.round(node.health * 100)));
  return (
    <div className="mx-auto flex w-20 items-center gap-1.5">
      <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-[width]", percent >= 70 ? "bg-emerald-500" : percent >= 35 ? "bg-amber-500" : "bg-destructive")} style={{ width: percent + "%" }} />
      </div>
      <span className="w-8 shrink-0 text-right text-[11px] tabular-nums text-muted-foreground">{percent}%</span>
    </div>
  );
}

/** Rotatable column: whether an exit-IP rotation webhook is configured. */
function RotatableCell({ node }: { node: EgressNodeDTO }) {
  const { t } = useTranslation();
  return node.rotationConfigured
    ? <Badge variant="outline" className="border-emerald-500/40 text-[10px] text-emerald-600" title={t("proxies.nodes.rotatableHelp")}>{t("proxies.nodes.rotatableYes")}</Badge>
    : <span className="text-[10px] text-muted-foreground" title={t("proxies.nodes.rotatableNoHelp")}>{t("proxies.nodes.rotatableNo")}</span>;
}
/** Latency column: probe latency of the healthy family, em-dash when unknown. */
function LatencyCell({ node }: { node: EgressNodeDTO }) {
  const { t } = useTranslation();
  const probe = node.ipv4Probe?.status === "healthy" ? node.ipv4Probe : node.ipv6Probe?.status === "healthy" ? node.ipv6Probe : undefined;
  if (probe && probe.latencyMs) {
    return <span className="text-xs tabular-nums text-muted-foreground" title={t("proxies.nodes.latencyHelp")}>{probe.latencyMs} ms</span>;
  }
  const probed = node.ipv4Probe?.status !== "unknown" || node.ipv6Probe?.status !== "unknown";
  return <span className="text-[10px] text-muted-foreground/70" title={probed ? t("proxies.nodes.latencyUnhealthy") : t("proxies.nodes.latencyUntested")}>{probed ? t("proxies.nodes.latencyUnhealthy") : t("proxies.nodes.latencyUntested")}</span>;
}
/** Compact exit-IP cell: IPv4 status + address; full dual-stack
 *  detail stays in the tooltip so the column stays narrow. */
function ExitIPCell({ node }: { node: EgressNodeDTO }) {
  const { t } = useTranslation();
  const probe = node.ipv4Probe?.status === "healthy" ? node.ipv4Probe : node.ipv6Probe?.status === "healthy" ? node.ipv6Probe : node.ipv4Probe ?? node.ipv6Probe;
  if (!probe || probe.status === "unknown") {
    return <span className="text-[10px] text-muted-foreground/70">{t("settings.egress.notTested")}</span>;
  }
  const healthy = probe.status === "healthy";
  const detail = [
    node.ipv4Probe?.exitIp ? "IPv4 " + node.ipv4Probe.exitIp + (node.ipv4Probe.error ? " · " + node.ipv4Probe.error : "") : null,
    node.ipv6Probe?.exitIp ? "IPv6 " + node.ipv6Probe.exitIp + (node.ipv6Probe.error ? " · " + node.ipv6Probe.error : "") : null,
    node.ipv4Probe?.error && node.ipv4Probe?.status !== "healthy" ? "IPv4: " + node.ipv4Probe.error : null,
    node.ipv6Probe?.error && node.ipv6Probe?.status !== "healthy" ? "IPv6: " + node.ipv6Probe.error : null,
  ].filter(Boolean).join(String.fromCharCode(10));
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className={cn("flex min-w-0 cursor-help items-center justify-center gap-1.5 text-xs", healthy ? "text-foreground" : "text-destructive")}>
          <span className={cn("size-1.5 shrink-0 rounded-full", healthy ? "bg-emerald-500" : "bg-red-500")} />
          <span className="truncate font-mono text-[10px] tabular-nums">{probe.exitIp || (healthy ? t("settings.egress.healthy") : t("settings.egress.unhealthy"))}</span>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 whitespace-pre-line">{detail || t("settings.egress.probeHelp")}</TooltipContent>
    </Tooltip>
  );
}
function Field({ label, controlId, description, help, children }: { label: string; controlId: string; description?: string; help?: string; children: ReactNode }) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-1.5">
        <Label htmlFor={controlId}>{label}</Label>
        {help ? (
          <Tooltip>
            <TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={help}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
            <TooltipContent className="max-w-80 whitespace-pre-line">{help}</TooltipContent>
          </Tooltip>
        ) : null}
      </div>
      {children}
      {description ? <p className="whitespace-pre-line text-xs leading-5 text-muted-foreground">{description}</p> : null}
    </div>
  );
}

function showError(error: unknown, fallback: string) {
  toast.error(error instanceof Error ? error.message : fallback);
}
