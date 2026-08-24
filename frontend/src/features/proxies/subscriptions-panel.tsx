import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Inbox, MoreHorizontal, Pencil, Plus, RefreshCw, Search, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  createEgressSource,
  deleteEgressSource,
  getEgressSourceProxyURL,
  getEgressSourceURL,
  listEgressSources,
  syncEgressSource,
  updateEgressSource,
  type EgressSourceDTO,
  type EgressSourceInput,
} from "@/features/settings/settings-api";
import { Control, IntervalInput, OperationSectionHeader, SourceError, ToggleControl } from "@/features/proxies/operations-context";
import { showError } from "@/features/proxies/operations-shared";
import { validSubscriptionProxyURL, validSubscriptionURL } from "@/features/settings/settings-model";
import { formatDateTime } from "@/shared/lib/format";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { Pagination } from "@/shared/components/pagination";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";

type SourceForm = Omit<EgressSourceInput, "url" | "proxyURL" | "clearProxyURL"> & { url: string; proxyEnabled: boolean; proxyURL: string };
const emptySource: SourceForm = {
  name: "", enabled: true, url: "", proxyEnabled: false, proxyURL: "", refreshIntervalSeconds: 900,
};

/** Subscription sources are the upstream feed of the node inventory. */
export function SubscriptionsPanel({ showHeader = true }: { showHeader?: boolean }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [sourceEditing, setSourceEditing] = useState<EgressSourceDTO | null | undefined>(undefined);
  const [sourceForm, setSourceForm] = useState<SourceForm>(emptySource);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const sourcesQuery = useQuery({ queryKey: ["egress-sources"], queryFn: () => listEgressSources() });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-sources"] });
  };
  const saveSource = useMutation({
    mutationFn: () => {
      const input: EgressSourceInput = {
        name: sourceForm.name, enabled: sourceForm.enabled,
        url: sourceForm.url.trim() || undefined,
        proxyURL: sourceForm.proxyEnabled ? (sourceForm.proxyURL.trim() || undefined) : undefined,
        clearProxyURL: Boolean(sourceEditing?.proxyConfigured && !sourceForm.proxyEnabled),
        refreshIntervalSeconds: sourceForm.refreshIntervalSeconds,
      };
      return sourceEditing ? updateEgressSource(sourceEditing.id, input) : createEgressSource(input);
    },
    onSuccess: () => { if (!sourceEditing) setPage(1); invalidate(); setSourceEditing(undefined); toast.success(t("settings.egress.sourceSaved")); },
    onError: showError,
  });
  const removeSource = useMutation({
    mutationFn: deleteEgressSource,
    onSuccess: () => { if (page > 1 && pagedSources.length === 1) setPage(page - 1); invalidate(); toast.success(t("settings.egress.sourceDeleted")); },
    onError: showError,
  });
  const syncSource = useMutation({
    mutationFn: syncEgressSource,
    onSuccess: (value) => { invalidate(); toast.success(t("settings.egress.sourceSynced", value)); },
    onError: showError,
  });

  function openSource(value?: EgressSourceDTO) {
    if (!value) {
      setSourceForm(emptySource);
      setSourceEditing(null);
      return;
    }
    setSourceForm({
      name: value.name, enabled: value.enabled, url: "", refreshIntervalSeconds: value.refreshIntervalSeconds,
      proxyEnabled: value.proxyConfigured, proxyURL: "",
    });
    setSourceEditing(value);
    if (value.urlConfigured) {
      getEgressSourceURL(value.id)
        .then(({ url }) => setSourceForm((current) => (current.url === "" ? { ...current, url } : current)))
        .catch(() => undefined);
    }
    if (value.proxyConfigured) {
      getEgressSourceProxyURL(value.id)
        .then(({ proxyURL }) => setSourceForm((current) => (current.proxyURL === "" ? { ...current, proxyURL } : current)))
        .catch(() => undefined);
    }
  }

  const normalizedSearch = search.trim().toLocaleLowerCase();
  const sources = sourcesQuery.data?.items ?? [];
  const filteredSources = sources.filter((source) => {
    if (normalizedSearch && !source.name.toLocaleLowerCase().includes(normalizedSearch)) return false;
    return true;
  });
  const pageCount = Math.max(1, Math.ceil(filteredSources.length / pageSize));
  const currentPage = Math.min(page, pageCount);
  const pagedSources = filteredSources.slice((currentPage - 1) * pageSize, currentPage * pageSize);
  const hasActiveFilters = Boolean(normalizedSearch);
  const sourceProxyInvalid = sourceForm.proxyEnabled && Boolean(sourceForm.proxyURL.trim()) && !validSubscriptionProxyURL(sourceForm.proxyURL);
  const sourceURLInvalid = Boolean(sourceForm.url.trim()) && !validSubscriptionURL(sourceForm.url);
  // 后端强制 60-86400;此前清空输入会变成 0 提交, 只得到原始 400 错误。
  const sourceIntervalInvalid = (sourceForm.refreshIntervalSeconds ?? 0) < 60 || (sourceForm.refreshIntervalSeconds ?? 0) > 86400;

  return (
    <section className="space-y-3">
      {showHeader ? <OperationSectionHeader title={t("settings.egress.subscriptions")} help={t("settings.egress.subscriptionsHelp")} /> : null}

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("settings.egress.searchSubscriptions")} aria-label={t("settings.egress.searchSubscriptions")} />
              </div>
            </div>
            <Button type="button" size="sm" variant="secondary" onClick={() => openSource()}><Plus />{t("settings.egress.addSource")}</Button>
          </>
        )}
        footer={filteredSources.length > 0 ? <Pagination page={currentPage} pageSize={pageSize} total={filteredSources.length} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {sourcesQuery.isError ? <ErrorState message={sourcesQuery.error.message} onRetry={() => void sourcesQuery.refetch()} /> : null}
        {!sourcesQuery.isError ? <Table viewportRows={showHeader ? 10 : 6} rowHeight={48} className="table-fixed">
          <TableHeader><TableRow className="hover:bg-transparent"><TableHead className="text-center">{t("proxies.supply.name")}</TableHead><TableHead className="w-[88px] text-center">{t("proxies.supply.enabledCol")}</TableHead><TableHead className="w-[104px] whitespace-nowrap text-center">{t("settings.egress.refreshInterval")}</TableHead><TableHead className="w-[88px] text-center">{t("proxies.supply.viaProxy")}</TableHead><TableHead className="w-[128px] whitespace-nowrap text-center">{t("settings.egress.lastSync")}</TableHead><TableActionHead /></TableRow></TableHeader>
          {sourcesQuery.isPending ? <TableBody><TableLoadingRow colSpan={6} /></TableBody> : null}
          {!sourcesQuery.isPending && pagedSources.length === 0 ? <TableBody><TableRow><TableCell colSpan={6} className="p-0">
            <div className="flex min-h-44 flex-col items-center justify-center gap-2 py-6 text-center">
              <Inbox className="size-7 stroke-1 text-muted-foreground" />
              <p className="text-sm text-muted-foreground">{hasActiveFilters ? t("settings.egress.noSubscriptionMatches") : t("settings.egress.noSources")}</p>
              {!hasActiveFilters ? <Button type="button" size="sm" variant="secondary" className="mt-1" onClick={() => openSource()}><Plus />{t("settings.egress.addSource")}</Button> : null}
            </div>
          </TableCell></TableRow></TableBody> : null}
          {!sourcesQuery.isPending && pagedSources.length > 0 ? <VirtualTableBody items={pagedSources} colSpan={6} rowHeight={48} renderRow={(source) => (
            <TableRow className="group h-12" key={source.id}>
              <TableCell className="text-center"><div className="flex min-w-0 items-center justify-center gap-2"><span className="truncate text-xs font-medium">{source.name}</span>{source.lastSyncError ? <SourceError message={source.lastSyncError} /> : null}</div></TableCell>
              <TableCell className="text-center"><Badge variant={source.enabled ? "secondary" : "outline"} className={source.enabled ? "bg-emerald-500/10 text-[10px] text-emerald-700 dark:text-emerald-300" : "text-[10px] text-muted-foreground"}>{source.enabled ? t("proxies.supply.yes") : t("proxies.supply.no")}</Badge></TableCell>
              <TableCell className="text-center text-xs tabular-nums text-muted-foreground">{source.refreshIntervalSeconds}s</TableCell>
              <TableCell className="text-center"><Badge variant={source.proxyConfigured ? "secondary" : "outline"} className={source.proxyConfigured ? "bg-emerald-500/10 text-[10px] text-emerald-700 dark:text-emerald-300" : "text-[10px] text-muted-foreground"}>{source.proxyConfigured ? t("proxies.supply.yes") : t("proxies.supply.no")}</Badge></TableCell>
              <TableCell className="whitespace-nowrap text-center text-xs text-muted-foreground">{source.lastSyncedAt ? formatDateTime(source.lastSyncedAt, i18n.language) : t("settings.egress.never")}</TableCell>
              <TableActionCell>
                <DropdownMenu><DropdownMenuTrigger asChild><Button type="button" size="icon" variant="ghost" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger><DropdownMenuContent align="end">
                  <DropdownMenuItem disabled={syncSource.isPending} onClick={() => syncSource.mutate(source.id)}><RefreshCw />{t("settings.egress.sync")}</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => openSource(source)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem className="text-destructive focus:text-destructive" disabled={removeSource.isPending} onClick={() => removeSource.mutate(source.id)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                </DropdownMenuContent></DropdownMenu>
              </TableActionCell>
            </TableRow>
          )} /> : null}
        </Table> : null}
      </DataTableShell>

      <Dialog open={sourceEditing !== undefined} onOpenChange={(open) => { if (!open) setSourceEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[480px]">
          <DialogHeader className="pr-8"><DialogTitle>{sourceEditing ? t("settings.egress.editSource") : t("settings.egress.addSource")}</DialogTitle><DialogDescription>{t("settings.egress.sourceDialogDescription")}</DialogDescription></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); saveSource.mutate(); }}>
            <ToggleControl label={t("settings.egress.enabled")} checked={sourceForm.enabled} onChange={(enabled) => setSourceForm({ ...sourceForm, enabled })} />
            <Control label={t("settings.egress.name")}><Input maxLength={160} value={sourceForm.name} onChange={(event) => setSourceForm({ ...sourceForm, name: event.target.value })} /></Control>
                        <Control label={t("settings.egress.subscriptionURL")}><Input type="text" autoComplete="off" placeholder="https://..." value={sourceForm.url} onChange={(event) => setSourceForm({ ...sourceForm, url: event.target.value })} /></Control>
            <div className="grid grid-cols-2 items-end gap-3">
              <Control label={t("settings.egress.refreshInterval")}><IntervalInput id="egress-source-refresh-interval" value={sourceForm.refreshIntervalSeconds ? String(sourceForm.refreshIntervalSeconds) : ""} onChange={(refreshIntervalSeconds) => setSourceForm({ ...sourceForm, refreshIntervalSeconds: Number(refreshIntervalSeconds) || 0 })} />{sourceIntervalInvalid ? <p className="text-xs text-destructive">{t("settings.egress.invalidRefreshInterval")}</p> : null}</Control>
              <div className="flex h-8 items-center justify-between gap-3 rounded-md bg-secondary/55 px-3">
                <Label className="text-xs font-medium">{t("settings.egress.subscriptionProxy")}</Label>
                <Switch checked={sourceForm.proxyEnabled} onCheckedChange={(proxyEnabled) => setSourceForm({ ...sourceForm, proxyEnabled })} aria-label={t("settings.egress.subscriptionProxy")} />
              </div>
            </div>
            {sourceForm.proxyEnabled ? <Control label={t("settings.egress.subscriptionProxyURL")}><Input type="text" autoComplete="off" aria-invalid={sourceProxyInvalid} placeholder="http://proxy.example:8080" value={sourceForm.proxyURL} onChange={(event) => setSourceForm({ ...sourceForm, proxyURL: event.target.value })} />{sourceProxyInvalid ? <p className="text-xs text-destructive">{t("settings.egress.invalidSubscriptionProxy")}</p> : null}</Control> : null}
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setSourceEditing(undefined)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!sourceForm.name.trim() || (!sourceEditing && !sourceForm.url.trim()) || (sourceForm.proxyEnabled && !sourceEditing?.proxyConfigured && !sourceForm.proxyURL.trim()) || sourceProxyInvalid || sourceURLInvalid || sourceIntervalInvalid || saveSource.isPending}>{saveSource.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  );
}
