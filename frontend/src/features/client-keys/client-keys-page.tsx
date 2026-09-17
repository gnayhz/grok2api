import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, MoreHorizontal, Pencil, Plus, Search, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { CopyButton } from "@/shared/components/copy-button";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/shared/ui/alert-dialog";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Checkbox } from "@/shared/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/shared/ui/dropdown-menu";
import { Input } from "@/shared/ui/input";
import { Label } from "@/shared/ui/label";
import { Spinner } from "@/shared/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/shared/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { deleteClientKey, deleteClientKeys, getClientKeySecret, listClientKeys, updateClientKeysEnabled, type ClientKeyDTO, type ProviderScopeValue, type TierScopeValue } from "@/entities/client-key/client-key-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { ClientKeyEditor } from "./client-key-editor";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { USD_TICKS_PER_DOLLAR as USD_TICKS } from "@/shared/lib/usd";
import { formatDateTime } from "@/shared/lib/format";
import { showErrorToast } from "@/shared/lib/show-error";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

type SecretDialogState = {
  secret: string;
  source: "created" | "retrieved";
};

export function ClientKeysPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [modelScopeFilter, setModelScopeFilter] = useState("");
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [editing, setEditing] = useState<ClientKeyDTO | "new" | null>(null);
  const [deleting, setDeleting] = useState<ClientKeyDTO | null>(null);
  const [secretDialog, setSecretDialog] = useState<SecretDialogState | null>(null);
  const [statusReferenceTime] = useState(() => Date.now());
  const debouncedSearch = useDebouncedValue(search);

  const keysQuery = useQuery({
    queryKey: ["client-keys", page, pageSize, debouncedSearch, statusFilter, modelScopeFilter, sort.field, sort.order],
    queryFn: ({ signal }) => listClientKeys({ page, pageSize, search: debouncedSearch, status: statusFilter, modelScope: modelScopeFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined }, signal),
    refetchOnMount: "always",
  });

  const deleteMutation = useLifetimeMutation({
    mutationFn: (id: string, signal) => deleteClientKey(id, signal),
    onSuccess: (_, id) => {
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      setDeleting((current) => current?.id === id ? null : current);
      toast.success(t("keys.deleted"));
    },
    onError: showError,
  });

  const copyMutation = useLifetimeMutation({
    mutationFn: (id: string, signal) => getClientKeySecret(id, signal),
    onSuccess: (result) => setSecretDialog({ secret: result.secret, source: "retrieved" }),
    onError: showError,
  });

  const batchUpdateMutation = useLifetimeMutation({
    mutationFn: ({ ids, enabled }: { ids: string[]; enabled: boolean }, signal) => updateClientKeysEnabled(ids, enabled, signal),
    onSuccess: (_, { ids }) => {
      const completed = new Set(ids);
      setSelected((current) => new Set([...current].filter((id) => !completed.has(id))));
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      toast.success(t("keys.batchUpdated"));
    },
    onError: showError,
  });

  const batchDeleteMutation = useLifetimeMutation({
    mutationFn: (ids: string[], signal) => deleteClientKeys(ids, signal),
    onSuccess: (_, ids) => {
      const completed = new Set(ids);
      setSelected((current) => new Set([...current].filter((id) => !completed.has(id))));
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      toast.success(t("keys.deleted"));
    },
    onError: showError,
  });

  function showError(error: unknown): void {
    showErrorToast(error, t);
  }

  const result = keysQuery.data;
  const pageIDs = result?.items.map((key) => key.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

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

  function toggleKey(id: string, checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }
  return (
    <div className="space-y-5">
      <header className="flex min-h-8 items-center">
        <h1 className="text-xl font-medium">{t("keys.title")}</h1>
        <p className="sr-only">{t("keys.description")}</p>
      </header>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("keys.search")} aria-label={t("keys.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "status", label: t("keys.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "active", label: t("keys.statusActive") },
                  { value: "disabled", label: t("common.disabled") },
                  { value: "expired", label: t("keys.statusExpired") },
                ] },
                { id: "modelScope", label: t("keys.modelScope"), value: modelScopeFilter, onChange: (value) => { setModelScopeFilter(value); setPage(1); }, options: [
                  { value: "all", label: t("keys.allModels") },
                  { value: "restricted", label: t("keys.restrictedModels") },
                ] },
              ]} />
            </div>
            {selected.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate({ ids: [...selected], enabled: true })}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate({ ids: [...selected], enabled: false })}>{t("common.disable")}</Button>
                <Button variant="ghost" size="sm" className="text-destructive hover:text-destructive" onClick={() => setBatchDeleteOpen(true)}>{t("common.delete")}</Button>
              </div>
            ) : <Button size="sm" onClick={() => setEditing("new")}><Plus />{t("keys.create")}</Button>}
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {keysQuery.isError ? <ErrorState message={keysQuery.error.message} onRetry={() => void keysQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {keysQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={56} className="min-w-[1200px] table-fixed text-xs">
            <colgroup>
              <col className="w-10" />
              <col className="w-36" />
              <col className="w-56" />
              <col className="w-20" />
              <col className="w-24" />
              <col className="w-18" />
              <col className="w-20" />
              <col className="w-40" />
              <col className="w-36" />
              <col className="w-36" />
              <col className="w-10" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("keys.name")}</SortableTableHead>
                <SortableTableHead field="prefix" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("keys.prefix")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("keys.status")}</SortableTableHead>
                <TableHead className="text-center">{t("keys.accountScope")}</TableHead>
                <SortableTableHead field="rpmLimit" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("keys.rpmShort")}</SortableTableHead>
                <SortableTableHead field="maxConcurrent" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("keys.concurrencyShort")}</SortableTableHead>
                <SortableTableHead field="billingLimit" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("keys.billingLimit")}</SortableTableHead>
                <SortableTableHead field="expiresAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("keys.expires")}</SortableTableHead>
                <SortableTableHead field="lastUsedAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("keys.lastUsed")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {keysQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={11} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={11} rowHeight={56} renderRow={(key) => (
                <TableRow className="group h-14" key={key.id} data-state={selected.has(key.id) ? "selected" : undefined}>
                  <TableCell><Checkbox checked={selected.has(key.id)} onCheckedChange={(checked) => toggleKey(key.id, checked === true)} aria-label={t("common.selectItem", { name: key.name })} /></TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate font-medium" title={key.name}>{key.name}</span>
                    <span className="mt-0.5 block truncate text-[10px] text-muted-foreground">
                      {key.modelScope === "all" ? t("keys.allModels") : key.allowedModelIds.length === 0 ? t("keys.noAllowedModels") : t("keys.selectedModels", { count: key.allowedModelIds.length })}
                      {key.allowModelAliases ? ` · ${t("keys.modelAliases")}` : ""}
                    </span>
                  </TableCell>
                  <TableCell className="overflow-hidden">
                    <div className="flex w-full min-w-0 items-center gap-1">
                      <code className="min-w-0 flex-1 truncate rounded bg-muted px-1.5 py-1 text-xs text-muted-foreground" title={`g2a_${key.prefix}_********`}>g2a_{key.prefix}_********</code>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Button type="button" variant="ghost" size="icon" className="size-7 shrink-0" disabled={copyMutation.isPending} aria-label={t("keys.copySecret")} onClick={() => copyMutation.mutate(key.id)}>
                            {copyMutation.isPending && copyMutation.variables === key.id ? <Spinner className="size-3.5" /> : <Copy className="size-3.5" />}
                          </Button>
                        </TooltipTrigger>
                        <TooltipContent>{t("keys.copySecret")}</TooltipContent>
                      </Tooltip>
                    </div>
                  </TableCell>
                  <TableCell className="text-center"><ClientKeyStatus value={key} referenceTime={statusReferenceTime} /></TableCell>
                  <TableCell className="text-center"><AccountScopeSummary providerScope={key.providerScope ?? ["all"]} tierScope={key.tierScope ?? ["all"]} /></TableCell>
                  <TableCell className="text-center text-xs tabular-nums">{key.rpmLimit > 0 ? key.rpmLimit : t("keys.unlimited")}</TableCell>
                  <TableCell className="text-center text-xs tabular-nums">{key.maxConcurrent > 0 ? key.maxConcurrent : t("keys.unlimited")}</TableCell>
                  <TableCell><BillingUsage value={key} /></TableCell>
                  <TableCell className="overflow-hidden text-ellipsis whitespace-nowrap text-xs text-muted-foreground" title={key.expiresAt ? formatDateTime(key.expiresAt, i18n.language) : t("common.neverExpires")}>{key.expiresAt ? formatDateTime(key.expiresAt, i18n.language) : t("common.neverExpires")}</TableCell>
                  <TableCell className="overflow-hidden text-ellipsis whitespace-nowrap text-xs text-muted-foreground" title={formatDateTime(key.lastUsedAt, i18n.language)}>{formatDateTime(key.lastUsedAt, i18n.language)}</TableCell>
                  <TableActionCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setEditing(key)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => setDeleting(key)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableActionCell>
                </TableRow>
              )} />
            )}
          </Table>
        ) : null}
      </DataTableShell>

      {editing !== null && <ClientKeyEditor key={editing === "new" ? "new" : editing.id} editing={editing} onClose={() => setEditing(null)} onCreated={(secret) => setSecretDialog({ secret, source: "created" })} />}

      <Dialog open={secretDialog !== null} onOpenChange={(open) => !open && setSecretDialog(null)}>
        <DialogContent className="max-w-[440px]">
          <DialogHeader>
            <DialogTitle>{t(secretDialog?.source === "created" ? "keys.secretTitle" : "keys.copySecretTitle")}</DialogTitle>
            <DialogDescription>{t(secretDialog?.source === "created" ? "keys.secretDescription" : "keys.copySecretDescription")}</DialogDescription>
          </DialogHeader>
          <div className="min-w-0 space-y-1.5">
            <Label>{t("keys.secretLabel")}</Label>
            <div className="flex h-8 w-full min-w-0 overflow-hidden rounded-md border border-input bg-secondary/55">
              <code className="flex min-w-0 flex-1 select-all items-center overflow-x-auto whitespace-nowrap px-3 font-mono text-xs text-muted-foreground">{secretDialog?.secret ?? ""}</code>
              <CopyButton value={secretDialog?.secret ?? ""} copyLabel={t("keys.copySecret")} disabled={!secretDialog?.secret} className="h-full w-8 shrink-0 rounded-none border-l" onCopied={() => toast.success(t("common.copied"))} />
            </div>
          </div>
          <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setSecretDialog(null)}>{t("common.close")}</Button></DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("keys.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("keys.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" onClick={() => deleting && deleteMutation.mutate(deleting.id)}>{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("keys.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle><AlertDialogDescription>{t("keys.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" onClick={() => batchDeleteMutation.mutate([...selected])}>{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function BillingUsage({ value }: { value: ClientKeyDTO }) {
  const { t, i18n } = useTranslation();
  const used = value.billedUsageUsdTicks / USD_TICKS;
  const reserved = (value.reservedUsageUsdTicks ?? 0) / USD_TICKS;
  const pending = reserved > 0 ? <div className="truncate text-xs text-muted-foreground" title={t("keys.reservedUsageHelp")}>{t("keys.reservedUsage", { value: formatUSD(reserved, i18n.language) })}</div> : null;
  if (value.billingLimitUsdTicks <= 0) {
    return (
      <div className="min-w-0">
        <div className="text-xs">{t("keys.unlimited")}</div>
        <div className="truncate text-xs text-muted-foreground">{t("keys.billedUsage", { value: formatUSD(used, i18n.language) })}</div>
        {pending}
      </div>
    );
  }
  const limit = value.billingLimitUsdTicks / USD_TICKS;
  const percent = Math.min(100, Math.max(0, (used / limit) * 100));
  return (
    <div className="min-w-0 space-y-1.5">
      <div className="truncate text-xs tabular-nums" title={`${formatUSD(used, i18n.language)} / ${formatUSD(limit, i18n.language)}`}>{formatUSD(used, i18n.language)} / {formatUSD(limit, i18n.language)}</div>
      {pending}
      <div className="h-1 overflow-hidden rounded-full bg-muted" aria-hidden="true">
        <div className="h-full rounded-full bg-emerald-500 transition-[width]" style={{ width: `${percent}%` }} />
      </div>
    </div>
  );
}

function formatUSD(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
}

function ClientKeyStatus({ value, referenceTime }: { value: ClientKeyDTO; referenceTime: number }) {
  const { t } = useTranslation();
  if (!value.enabled) {
    return <Badge variant="outline" className="text-muted-foreground">{t("common.disabled")}</Badge>;
  }
  if (value.expiresAt && new Date(value.expiresAt).getTime() <= referenceTime) {
    return <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("keys.statusExpired")}</Badge>;
  }
  return <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("keys.statusActive")}</Badge>;
}

function AccountScopeSummary({ providerScope, tierScope }: { providerScope: ProviderScopeValue[]; tierScope: TierScopeValue[] }) {
  const { t } = useTranslation();
  const providerLabels: Record<ProviderScopeValue, string> = { all: t("keys.allProviders"), grok_build: "Build", grok_web: "Web", grok_console: "Console" };
  const tierLabels: Record<TierScopeValue, string> = { all: t("keys.allTiers"), free: "Free", super: "Super" };
  return (
    <span className="inline-flex max-w-full flex-col items-start gap-0.5 text-left text-[10px] leading-4">
      <span className="max-w-full truncate text-foreground">{providerScope.map((value) => providerLabels[value]).join(" · ")}</span>
      <span className="max-w-full truncate text-muted-foreground">{tierScope.map((value) => tierLabels[value]).join(" · ")}</span>
    </span>
  );
}
