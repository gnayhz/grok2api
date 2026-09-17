import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, Eye, RefreshCw, Search, X } from "lucide-react";
import { memo, useCallback, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/shared/ui/button";
import { Input } from "@/shared/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/shared/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/shared/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { listClientKeys } from "@/entities/client-key/client-key-api";
import { listAccounts } from "@/entities/account/account-api";
import { RequestAuditDetailDialog } from "./request-audit-detail-dialog";
import { AuditSummary } from "./audit-summary";
import { AuditRow, AuditMobileCard } from "./audit-row";
import { AuditResultLegend } from "./audit-result-mark";
import { auditProviderLabel } from "./audit-presentation";
import { getDashboard } from "@/entities/dashboard/dashboard-api";
import { cn } from "@/shared/lib/cn";
import { getRequestAudits, getRequestAuditSummary, type AuditDTO, type AuditPeriod } from "@/entities/audit/audit-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { CursorPagination } from "@/shared/components/pagination";
import { PeriodSelector } from "@/shared/components/period-selector";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { useAfterPaint } from "@/shared/hooks/use-after-paint";
import { toPeriodValue, type PeriodDays } from "@/shared/lib/period";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";
import "./audits.css";

const AUDIT_PAGE_CACHE_TIME_MS = 60_000;
const AUDIT_SUMMARY_CACHE_TIME_MS = 120_000;
// 筛选名单始终限制在服务器搜索后的前 50 条，避免大账号池把大量选项累积到浏览器。
const AUDIT_FILTER_PAGE_SIZE = 50;
// 名单高度约 5 行，超出后内部滚动。
const AUDIT_FILTER_MAX_HEIGHT = "max-h-56 overflow-y-auto py-0.5";

type AuditCursorState = { scope: string; values: string[] };

function subscribeAuditLayout(onChange: () => void) {
  const query = window.matchMedia("(min-width: 768px)");
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}

function auditTableLayout() {
  return window.matchMedia("(min-width: 768px)").matches;
}

export function RequestAuditsPage() {
  const [selectedAudit, setSelectedAudit] = useState<AuditDTO | null>(null);
  const [detailOpen, setDetailOpen] = useState(false);
  const detailTrigger = useRef<HTMLElement | null>(null);
  const openAudit = useCallback((audit: AuditDTO) => {
    detailTrigger.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setSelectedAudit(audit);
    setDetailOpen(true);
  }, []);
  const returnFocus = useCallback(() => {
    if (detailTrigger.current?.isConnected) detailTrigger.current.focus({ preventScroll: true });
  }, []);

  return <>
    <AuditWorkspace openAudit={openAudit} />
    <RequestAuditDetailDialog key={selectedAudit?.id ?? "closed"} audit={selectedAudit} open={detailOpen} onOpenChange={setDetailOpen} onReturnFocus={returnFocus} />
  </>;
}

// Opening diagnostics should not reconcile the filters, summary and list.
const AuditWorkspace = memo(function AuditWorkspace({ openAudit }: { openAudit: (audit: AuditDTO) => void }) {
  const { t, i18n } = useTranslation();
  const rowsReady = useAfterPaint();
  const tableLayout = useSyncExternalStore(subscribeAuditLayout, auditTableLayout, () => true);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [modelFilter, setModelFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [modeFilter, setModeFilter] = useState("");
  const [errorCodeFilter, setErrorCodeFilter] = useState("");
  const [keyFilter, setKeyFilter] = useState("");
  const [accountFilter, setAccountFilter] = useState("");
  const [periodDays, setPeriodDays] = useState<PeriodDays>(1);
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const forceSummaryRefresh = useRef(false);
  const debouncedSearch = useDebouncedValue(search);
  const debouncedKeyFilter = useDebouncedValue(keyFilter);
  const debouncedAccountFilter = useDebouncedValue(accountFilter);
  const period: AuditPeriod = toPeriodValue(periodDays);
  const cursorScope = useMemo(() => JSON.stringify([
    pageSize, debouncedSearch, modelFilter, statusFilter, errorCodeFilter, modeFilter,
    debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order,
  ]), [pageSize, debouncedSearch, modelFilter, statusFilter, errorCodeFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order]);
  const [cursorState, setCursorState] = useState<AuditCursorState>(() => ({ scope: cursorScope, values: [""] }));
  if (cursorState.scope !== cursorScope) {
    setCursorState({ scope: cursorScope, values: [""] });
  }
  const cursors = cursorState.scope === cursorScope ? cursorState.values : [""];
  const cursor = cursors[cursors.length - 1];

  const updateCursors = useCallback((update: (values: string[]) => string[]) => {
    setCursorState((current) => {
      const values = current.scope === cursorScope ? current.values : [""];
      return { scope: cursorScope, values: update(values) };
    });
  }, [cursorScope]);

  const auditsQuery = useQuery({
    queryKey: ["request-audits", "cursor", cursorScope, cursor],
    queryFn: ({ signal }) => getRequestAudits({ cursor, pageSize, search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, errorCode: errorCodeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period, sortBy: sort.field, sortOrder: sort.order }, signal),
    // Keep existing rows mounted while the next scope loads.
    placeholderData: keepPreviousData,
    gcTime: AUDIT_PAGE_CACHE_TIME_MS,
    structuralSharing: false,
  });
  const dashboardTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const periodStatsQuery = useQuery({
    queryKey: ["audit-degraded-withholds", period, dashboardTimezone],
    queryFn: () => getDashboard(period, dashboardTimezone),
    placeholderData: keepPreviousData,
    refetchInterval: 30_000,
  });
  const summaryQuery = useQuery({
    queryKey: ["request-audits", "summary", debouncedSearch, modelFilter, statusFilter, modeFilter, errorCodeFilter, debouncedKeyFilter, debouncedAccountFilter, period],
    queryFn: ({ signal }) => getRequestAuditSummary({ search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, errorCode: errorCodeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period }, forceSummaryRefresh.current, signal),
    placeholderData: keepPreviousData,
    gcTime: AUDIT_SUMMARY_CACHE_TIME_MS,
  });
  const modelOptionsQuery = useQuery({
    queryKey: ["models", "audit-filter"],
    queryFn: () => listModels({ page: 1, pageSize: 100 }),
    staleTime: 60_000,
  });
  // 密钥/账号筛选名单只在对应三级菜单展开时懒加载，输入后重新按匹配查询。
  const [keyFilterOptionsOpen, setKeyFilterOptionsOpen] = useState(false);
  const [accountFilterOptionsOpen, setAccountFilterOptionsOpen] = useState(false);
  const [keyFilterOptionsSearch, setKeyFilterOptionsSearch] = useState("");
  const [accountFilterOptionsSearch, setAccountFilterOptionsSearch] = useState("");
  const debouncedKeyFilterOptionsSearch = useDebouncedValue(keyFilterOptionsSearch);
  const debouncedAccountFilterOptionsSearch = useDebouncedValue(accountFilterOptionsSearch);
  const keyFilterOptionsQuery = useQuery({
    queryKey: ["client-keys", "audit-filter", debouncedKeyFilterOptionsSearch],
    queryFn: () => listClientKeys({ page: 1, pageSize: AUDIT_FILTER_PAGE_SIZE, search: auditFilterOptionSearch(debouncedKeyFilterOptionsSearch) }),
    enabled: keyFilterOptionsOpen,
    staleTime: 60_000,
  });
  const accountFilterOptionsQuery = useQuery({
    queryKey: ["accounts", "audit-filter", debouncedAccountFilterOptionsSearch],
    queryFn: () => listAccounts({ page: 1, pageSize: AUDIT_FILTER_PAGE_SIZE, search: auditFilterOptionSearch(debouncedAccountFilterOptionsSearch) }),
    enabled: accountFilterOptionsOpen,
    staleTime: 60_000,
  });
  const keyFilterOptionsFailed = keyFilterOptionsQuery.isError;
  const keyFilterOptionsFetching = keyFilterOptionsQuery.isFetching;
  const accountFilterOptionsFailed = accountFilterOptionsQuery.isError;
  const accountFilterOptionsFetching = accountFilterOptionsQuery.isFetching;
  // 账号范围覆盖三种 provider，审计记录可能来自任一 provider 的账号。
  const keyFilterOptions = keyFilterOptionsQuery.data?.items ?? [];
  const accountFilterOptions = accountFilterOptionsQuery.data?.items ?? [];
  const keyFilterGroups = [
    {
      id: "keys", label: t("audits.key"),
      emptyLabel: keyFilterOptionsFailed ? t("audits.filterOptionsLoadFailed") : keyFilterOptionsFetching ? t("common.loading") : t("audits.filterOptionsEmpty"),
      options: keyFilterOptions.map((key) => ({
        value: String(key.id),
        label: key.name || key.prefix,
        description: `#${key.id} · ${key.prefix}`,
      })),
      loading: keyFilterOptionsFetching, hasMore: keyFilterOptionsFailed,
      actionLabel: t("common.retry"), onAction: () => { void keyFilterOptionsQuery.refetch(); },
      noteLabel: !keyFilterOptionsFailed && (keyFilterOptionsQuery.data?.total ?? 0) > keyFilterOptions.length ? t("audits.filterOptionsTruncated") : undefined,
      hideLabel: true,
      maxHeightClassName: AUDIT_FILTER_MAX_HEIGHT,
    },
  ];
  const accountFilterGroups = [
    {
      id: "accounts", label: t("audits.account"),
      emptyLabel: accountFilterOptionsFailed ? t("audits.filterOptionsLoadFailed") : accountFilterOptionsFetching ? t("common.loading") : t("audits.filterOptionsEmpty"),
      options: accountFilterOptions.map((account) => ({
        value: String(account.id),
        label: account.name || account.email || `#${account.id}`,
        description: `#${account.id}`,
        badge: auditProviderLabel(account.provider),
      })),
      loading: accountFilterOptionsFetching, hasMore: accountFilterOptionsFailed,
      actionLabel: t("common.retry"), onAction: () => { void accountFilterOptionsQuery.refetch(); },
      noteLabel: !accountFilterOptionsFailed && (accountFilterOptionsQuery.data?.total ?? 0) > accountFilterOptions.length ? t("audits.filterOptionsTruncated") : undefined,
      hideLabel: true,
      maxHeightClassName: AUDIT_FILTER_MAX_HEIGHT,
    },
  ];
  const result = auditsQuery.data;
  const nextCursor = result?.nextCursor ?? "";
  const summary = summaryQuery.data;
  const summaryLoading = summaryQuery.isPending;
  const refreshing = manualRefreshing || auditsQuery.isFetching || summaryQuery.isFetching || periodStatsQuery.isFetching;
  const modelOptions = useMemo(() => [...new Map((modelOptionsQuery.data?.items ?? []).map((model) => [model.publicId, { value: model.publicId, label: model.publicId }])).values()], [modelOptionsQuery.data?.items]);
  const renderAuditRow = useCallback((audit: AuditDTO) => <AuditRow key={audit.id} audit={audit} locale={i18n.language} onOpen={openAudit} />, [i18n.language, openAudit]);
  const renderMobileRow = useCallback((audit: AuditDTO) => (
    <TableRow key={audit.id} className="hover:bg-transparent">
      <TableCell className="p-0"><AuditMobileCard audit={audit} locale={i18n.language} onOpen={openAudit} /></TableCell>
    </TableRow>
  ), [i18n.language, openAudit]);
  const hasFilters = Boolean(search || modelFilter || statusFilter || modeFilter || errorCodeFilter || keyFilter || accountFilter);

  function clearFilters(): void {
    setSearch("");
    setModelFilter("");
    setStatusFilter("");
    setModeFilter("");
    setErrorCodeFilter("");
    setKeyFilter("");
    setAccountFilter("");
  }

  function refreshAll(): void {
    setManualRefreshing(true);
    forceSummaryRefresh.current = true;
    void Promise.all([
      auditsQuery.refetch(),
      summaryQuery.refetch(),
      periodStatsQuery.refetch(),
      new Promise<void>((resolve) => window.setTimeout(resolve, 400)),
    ]).finally(() => {
      forceSummaryRefresh.current = false;
      setManualRefreshing(false);
    });
  }

  const changeSort = useCallback((field: string, initialOrder: SortOrder): void => {
    setSort((current) => nextTableSort(current, field, initialOrder));
  }, []);

  return (
    <div className="audit-page space-y-4">
      {/* Top Bar: Brand, Status, and Action Hub (Command Deck Style) */}
      <div className="flex flex-wrap items-center justify-between gap-4 py-1">
        {/* Title and Live status */}
        <div className="flex items-center gap-3">
          <div className="relative flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/20">
            <Eye className="size-5 animate-pulse text-primary" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h1 className="text-xl font-bold tracking-tight text-foreground">
                {t("audits.title")}
              </h1>
              <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/10 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
                <span className="size-1.5 rounded-full bg-emerald-500 animate-ping" />
                {t("network.pulseLive")}
              </span>
            </div>
            <p className="text-xs text-muted-foreground mt-0.5">
              {summary?.usage?.requests !== undefined ? `${summary.usage.requests} ${t("audits.requestRecords")}` : t("audits.summaryRequests")}
              {summary?.usage?.successRate !== undefined ? ` · ${summary.usage.successRate.toFixed(1)}% ${t("audits.successRate")}` : ""}
              {summary?.usage?.averageDurationMs !== undefined ? ` · ${Math.round(summary.usage.averageDurationMs)}ms` : ""}
            </p>
          </div>
        </div>

        {/* Quick Action Center */}
        <div className="flex flex-wrap items-center gap-2">
          <PeriodSelector value={periodDays} onChange={setPeriodDays} ariaLabel={t("audits.usageSummary")} />
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="size-8"
                disabled={refreshing}
                onClick={refreshAll}
              >
                <RefreshCw
                  className={cn(
                    "size-4 text-muted-foreground",
                    refreshing && "animate-spin text-primary"
                  )}
                />
              </Button>
            </TooltipTrigger>
            <TooltipContent>{t("network.refresh")}</TooltipContent>
          </Tooltip>
        </div>
      </div>

      {summaryQuery.isError ? <ErrorState message={summaryQuery.error.message} onRetry={() => void summaryQuery.refetch()} /> : <AuditSummary
        summary={summary}
        loading={summaryLoading}
        updating={summaryQuery.isFetching || periodStatsQuery.isFetching}
        filtered={hasFilters}
        periodStats={periodStatsQuery.isError ? undefined : periodStatsQuery.data}
        periodLoading={periodStatsQuery.isPending}
      />}

      <DataTableShell
        className="audit-records gap-0 overflow-hidden rounded-xl border bg-background"
        toolbar={(
          <>
            <div className="flex items-center gap-2">
              <h2 className="text-sm font-medium leading-5">{t("audits.requestRecords")}</h2>
              <AuditResultLegend />
              <span role="status" className="text-[11px] leading-5 tabular-nums text-muted-foreground">{result ? t("audits.pageRecordCount", { count: result.items.length }) : null}</span>
            </div>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-72 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("audits.search")} aria-label={t("audits.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "model", label: t("audits.model"), value: modelFilter, onChange: setModelFilter, options: modelOptions },
                { id: "status", label: t("audits.status"), value: statusFilter, onChange: setStatusFilter, options: [
                  { value: "2xx", label: `2xx · ${t("audits.statusSuccess")}` },
                  { value: "4xx", label: `4xx · ${t("audits.statusClientError")}` },
                  { value: "5xx", label: `5xx · ${t("audits.statusServerError")}` },
                  { value: "other", label: t("audits.statusOtherError") },
                ] },
                { id: "mode", label: t("audits.mode"), value: modeFilter, onChange: setModeFilter, options: [
                  { value: "stream", label: t("audits.stream") },
                  { value: "nonStream", label: t("audits.nonStream") },
                ] },

    // errorCode filter: the quality_degraded preset isolates degraded-withhold
    // records in one click (per-attempt 503s and the request-level terminal
    // record share the same error_code; see recordQualityDegraded).
    { id: "errorCode", label: t("audits.errorCodeFilter"), value: errorCodeFilter, onChange: setErrorCodeFilter, options: [
     { value: "quality_degraded", label: t("audits.errorCodeQualityDegraded") },
     { value: "quality_evidence_timeout", label: t("audits.errorCodeQualityEvidenceTimeout") },
     { value: "quality_created_timeout", label: t("audits.errorCodeQualityCreatedTimeout") },
     { value: "upstream_stream_empty", label: t("audits.errorCodeUpstreamStreamEmpty") },
    ] },
                {
                  id: "key", label: t("audits.key"), value: keyFilter,
                  onChange: setKeyFilter, options: [
                    {
                      value: "any", label: t("audits.key"), groups: keyFilterGroups,
                      onGroupsOpenChange: setKeyFilterOptionsOpen,
                      groupSearch: { value: keyFilterOptionsSearch, placeholder: t("audits.keyFilterPlaceholder"), onChange: (value) => {
                        setKeyFilterOptionsSearch(value);
                      } },
                    },
                  ],
                },
                {
                  id: "account", label: t("audits.account"), value: accountFilter,
                  onChange: setAccountFilter, options: [
                    {
                      value: "any", label: t("audits.account"), groups: accountFilterGroups,
                      onGroupsOpenChange: setAccountFilterOptionsOpen,
                      groupSearch: { value: accountFilterOptionsSearch, placeholder: t("audits.accountFilterPlaceholder"), onChange: (value) => {
                        setAccountFilterOptionsSearch(value);
                      } },
                    },
                  ],
                },
              ]} />
              {hasFilters ? <Button variant="ghost" size="icon" className="size-8" aria-label={t("audits.clearFilters")} onClick={clearFilters}><X className="size-4" /></Button> : null}
            </div>
          </>
        )}
        footer={(result?.items.length ?? 0) > 0 || cursors.length > 1 ? (
          <CursorPagination
            page={cursors.length}
            pageSize={pageSize}
            hasMore={Boolean(result?.hasMore && nextCursor)}
            disabled={auditsQuery.isFetching}
            onFirstPage={() => updateCursors(() => [""])}
            onPreviousPage={() => updateCursors((values) => values.length > 1 ? values.slice(0, -1) : values)}
            onNextPage={() => { if (nextCursor) updateCursors((values) => [...values, nextCursor]); }}
            onPageSizeChange={setPageSize}
          />
        ) : undefined}
      >
        {auditsQuery.isError ? <ErrorState message={auditsQuery.error.message} onRetry={() => void auditsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <div className="px-4 py-2"><EmptyState message={t(hasFilters ? "audits.noMatchingRequestsHint" : "audits.noRequestsHint")} /></div> : null}
        {(result?.items.length ?? 0) > 0 ? <div className="flex items-center justify-end gap-2 border-b bg-muted/15 px-4 py-2 md:hidden">
          <Select value={sort.field} onValueChange={(field) => setSort({ field, order: "desc" })}>
            <SelectTrigger className="h-8 w-36 text-xs" aria-label={t("audits.sortBy")}><SelectValue /></SelectTrigger>
            <SelectContent>
              {([["createdAt", "createdAt"], ["model", "model"], ["status", "requestResult"], ["duration", "duration"], ["tokens", "tokens"], ["billing", "billing"]] as const).map(([field, label]) => <SelectItem key={field} value={field}>{t("audits." + label)}</SelectItem>)}
            </SelectContent>
          </Select>
          <Button variant="ghost" size="icon" className="size-8" aria-label={t(sort.order === "desc" ? "audits.sortAscending" : "audits.sortDescending")} onClick={() => setSort((current) => ({ ...current, order: current.order === "desc" ? "asc" : "desc" }))}>{sort.order === "desc" ? <ArrowDown className="size-4" /> : <ArrowUp className="size-4" />}</Button>
        </div> : null}
        {tableLayout && (auditsQuery.isPending || (result && result.items.length > 0)) ? (
          <div>
          <Table viewportRows={20} rowHeight={80} aria-label={t("audits.requestRecords")} aria-busy={auditsQuery.isFetching} className="audit-table table-fixed text-xs">
            <colgroup>
              <col className="audit-col-result" />
              <col className="audit-col-request" />
              <col className="audit-col-connection" />
              <col className="audit-col-performance" />
              <col className="audit-col-usage" />
              <col className="audit-col-time" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort} align="center" className="audit-status-column">{t("audits.resultShort")}</SortableTableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.request")}</SortableTableHead>
                <TableHead>{t("audits.connection")}</TableHead>
                <SortableTableHead field="duration" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.duration")}</SortableTableHead>
                <SortableTableHead field="tokens" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.usageAndCost")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.createdAt")}</SortableTableHead>
              </TableRow>
            </TableHeader>
            {auditsQuery.isPending || !rowsReady ? (
              <TableBody><TableLoadingRow colSpan={6} /></TableBody>
            ) : (
              <AuditTableBody items={result?.items ?? []} colSpan={6} rowHeight={80} virtualizeAfter={8} overscan={3} renderRow={renderAuditRow} />
            )}
          </Table>
          </div>
        ) : null}
        {!tableLayout && (auditsQuery.isPending || (result && result.items.length > 0)) ? (
          <Table key="mobile" viewportRows={20} rowHeight={313} aria-label={t("audits.requestRecords")} aria-busy={auditsQuery.isFetching} className="table-fixed">
            <thead><tr><th className="h-0 p-0"><span className="sr-only">{t("audits.request")}</span></th></tr></thead>
            {auditsQuery.isPending || !rowsReady ? <TableBody><TableLoadingRow colSpan={1} /></TableBody> : <AuditTableBody items={result?.items ?? []} colSpan={1} rowHeight={313} virtualizeAfter={3} overscan={1} renderRow={renderMobileRow} />}
          </Table>
        ) : null}
      </DataTableShell>
    </div>
  );
});

const AuditTableBody = memo(VirtualTableBody<AuditDTO>);

function auditFilterOptionSearch(value: string): string {
  const trimmed = value.trim();
  return /^\d+$/.test(trimmed) ? `#${trimmed}` : trimmed;
}
