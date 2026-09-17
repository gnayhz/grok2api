import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, ClipboardPaste, Compass, Download, ExternalLink, FileUp, Link, MoreHorizontal, Pencil, Plus, RefreshCw, RotateCw, Search, SquareTerminal, TimerOff, Trash2, TriangleAlert, Webhook } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
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
import { Tabs, TabsList, TabsTrigger } from "@/shared/ui/tabs";
import { Textarea } from "@/shared/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import { EmptyState, ErrorState, LoadingState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";
import {
  acceptWebAccountTerms,
  cleanupAccounts,
  clearAccountCooldown,
  deleteAccount,
  deleteAccounts,
  previewAccountDeletion,
  previewCleanup,
  enableWebAccountNSFW,
  convertWebAccountsToBuild,
  detectBuildAccounts,
  exportAccountBatch,
  exportSelectedAccounts,
  getAccountSummary,
  importAccounts,
  importConsoleAccounts,
  importWebAccounts,
  listAccounts,
  refreshAccountBilling,
  refreshAccountsQuota,
  resetAccountsQuota,
  resetAllAccountQuota,
  refreshAccountsTokens,
  refreshAccountToken,
  refreshAccountQuota,
  refreshAllAccountBilling,
  refreshAllAccountTokens,
  refreshAllConsoleAccountQuotas,
  refreshAllWebAccountQuotas,
  runWebAccountScripts,
  setWebAccountBirthDate,
  syncWebAccountsToConsole,
  updateAccountsEnabled,
  updateAccountsMaxConcurrent,
  type AccountDTO,
  type AccountCleanupStatus,
  type AccountProvider,
  type CleanupPreviewDTO,
  type AccountTaskProgressDTO,
  type BuildConversionInput,
  type BuildConversionStrategy,
  type BuildDetectItemDTO,
  type WebConsoleSyncInput,
  type WebAccountScriptActions,
  type WebAccountScriptsInput,
} from "@/entities/account/account-api";
import { AccountQuota, ConsoleQuota, WebQuota } from "./account-quota";
import { AccountNameCell } from "./account-name-cell";
import { WebAccountScriptsDialog } from "./web-account-scripts";
import { WebAccountSettingsDialogs, WebAccountSettingsMenu, type WebAccountConfirmationTarget } from "./web-account-settings";
import { AccountMetricPanel, AccountStatus, AccountType, AccountTypeText, WebAccountType } from "./accounts-page-views";
import { downloadAccountExport } from "./download-account-export";
import { AccountEditor } from "./account-editor";
import { useDeviceLogin } from "./use-device-login";
import { useImportController } from "./use-import-controller";
import { useAbortController } from "@/shared/lib/use-abort-controller";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { useLifetimeSignal } from "@/shared/hooks/use-lifetime-signal";
import { isAbortError } from "@/shared/lib/is-abort-error";
import { showErrorToast } from "@/shared/lib/show-error";


type WebConversionTarget = "build" | "console";
type BuildQuotaTask = "sync" | "reset";
type BuildDetectCounts = Record<BuildDetectItemDTO["outcome"], number>;

const emptyBuildDetectCounts = (): BuildDetectCounts => ({ ok: 0, invalid: 0, failed: 0 });

type AccountSelection = {
  provider: AccountProvider;
  ids: Set<string>;
};

export function AccountsPage() {
  const [provider, setProvider] = useState<AccountProvider>("grok_build");
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const view = { pageSize, setPageSize, search, setSearch, sort, setSort };
  // Provider-scoped forms, selection and work are destroyed together. List
  // preferences stay with the page; late work cannot mutate the next workspace.
  return <AccountWorkspace key={provider} provider={provider} onProviderChange={setProvider} view={view} />;
}

type ListView = {
  pageSize: number; setPageSize: (value: number) => void;
  search: string; setSearch: (value: string) => void;
  sort: TableSort; setSort: (value: TableSort | ((current: TableSort) => TableSort)) => void;
};

function AccountWorkspace({ provider, onProviderChange, view }: { provider: AccountProvider; onProviderChange: (value: AccountProvider) => void; view: ListView }) {
  const { pageSize, setPageSize, search, setSearch, sort, setSort } = view;
  const lifetimeSignal = useLifetimeSignal();
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const quickImportFileInputRef = useRef<HTMLInputElement>(null);
  const quotaSyncAbortRef = useRef<AbortController | null>(null);
  const detectAbortRef = useRef<AbortController | null>(null);
  const detectOutcomeByIDRef = useRef(new Map<string, BuildDetectItemDTO["outcome"]>());
  const renewalAbortRef = useRef<AbortController | null>(null);
  const conversionAbortRef = useRef<AbortController | null>(null);
  const webConsoleSyncAbortRef = useRef<AbortController | null>(null);
  const { ref: webAccountScriptsAbortRef } = useAbortController();
  const { begin: beginImportFileRead, cancel: cancelImportFileRead } = useAbortController();
  const { abortRef: importAbortRef, toastRef: importToastRef } = useImportController();
  const [page, setPage] = useState(1);
  const [typeFilter, setTypeFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [renewalFilter, setRenewalFilter] = useState("");
  const [riskFilter, setRiskFilter] = useState("");
  const [agreementFilter, setAgreementFilter] = useState("");
  const [associationFilter, setAssociationFilter] = useState("");
  const [selection, setSelection] = useState<AccountSelection>(() => ({ provider, ids: new Set() }));
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [batchConcurrencyOpen, setBatchConcurrencyOpen] = useState(false);
  const [batchMaxConcurrent, setBatchMaxConcurrent] = useState("1");
  const [batchQuotaTaskOpen, setBatchQuotaTaskOpen] = useState(false);
  const [batchQuotaTask, setBatchQuotaTask] = useState<BuildQuotaTask>("sync");
  const [cleanupOpen, setCleanupOpen] = useState(false);
  const [cleanupStatuses, setCleanupStatuses] = useState<Set<AccountCleanupStatus>>(() => new Set());
  // Cleanup preview + optional linked deletion (independent from the delete dialogs' state).
  const [cleanupLinkedTargets, setCleanupLinkedTargets] = useState<AccountProvider[]>([]);
  // Keyed preview: a stale result stays mounted while the next one loads, so the
  // dialog never reflows between "has counts" and "no counts".
  const [cleanupPreview, setCleanupPreview] = useState<{ key: string; data: CleanupPreviewDTO } | null>(null);
  const [cleanupPreviewError, setCleanupPreviewError] = useState(false);
  const [exportOpen, setExportOpen] = useState(false);
  const [exportLimit, setExportLimit] = useState("1000");
  const [exportCursor, setExportCursor] = useState("0");
  const [exportSnapshotMaxId, setExportSnapshotMaxId] = useState("0");
  const [exportBatchNumber, setExportBatchNumber] = useState(1);
  const [exportCompletedCount, setExportCompletedCount] = useState(0);
  const [syncAllOpen, setSyncAllOpen] = useState(false);
  const [detectDialogOpen, setDetectDialogOpen] = useState(false);
  const [detectMode, setDetectMode] = useState<"selected" | "all">("all");
  const [allQuotaTask, setAllQuotaTask] = useState<BuildQuotaTask>("sync");
  const [quotaSyncProgress, setQuotaSyncProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [detectProgress, setDetectProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [detectItems, setDetectItems] = useState<BuildDetectItemDTO[]>([]);
  const [detectCounts, setDetectCounts] = useState<BuildDetectCounts>(emptyBuildDetectCounts);
  const [webConversionTargets, setWebConversionTargets] = useState<string[] | "all" | null>(null);
  const [webConversionTarget, setWebConversionTarget] = useState<WebConversionTarget>("build");
  const [webConversionStrategy, setWebConversionStrategy] = useState<BuildConversionStrategy>("missing");
  const [conversionProgress, setConversionProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [webConsoleSyncProgress, setWebConsoleSyncProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [webAccountScriptsTargets, setWebAccountScriptsTargets] = useState<string[] | "all" | null>(null);
  const [webAccountScriptsProgress, setWebAccountScriptsProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [renewAllOpen, setRenewAllOpen] = useState(false);
  const [renewalProgress, setRenewalProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [editing, setEditing] = useState<AccountDTO | null>(null);
  const [deleting, setDeleting] = useState<AccountDTO | null>(null);
  const [linkedDeleteTargets, setLinkedDeleteTargets] = useState<AccountProvider[]>([]);
  const [linkedDeleteCounts, setLinkedDeleteCounts] = useState<Partial<Record<AccountProvider, number>>>({});
  // Preview failures must not be painted as +0 — block confirm until a successful recount.
  const [linkedDeletePreviewError, setLinkedDeletePreviewError] = useState(false);
  const [quickImportOpen, setQuickImportOpen] = useState(false);
  const [quickImportTokens, setQuickImportTokens] = useState("");
  const [webConfirmationTarget, setWebConfirmationTarget] = useState<WebAccountConfirmationTarget | null>(null);
  const debouncedSearch = useDebouncedValue(search);

  useEffect(() => () => {
    quotaSyncAbortRef.current?.abort();
    detectAbortRef.current?.abort();
    renewalAbortRef.current?.abort();
    conversionAbortRef.current?.abort();
    webConsoleSyncAbortRef.current?.abort();
  }, []);

  const selected = selection.provider === provider ? selection.ids : new Set<string>();
  const selectedIdsKey = Array.from(selected).sort().join(",");

  const accountsQuery = useQuery({
    // A previous workspace may have cancelled after the server accepted work.
    refetchOnMount: "always",
    queryKey: ["accounts", provider, page, pageSize, debouncedSearch, typeFilter, statusFilter, renewalFilter, riskFilter, agreementFilter, associationFilter, sort.field, sort.order],
    queryFn: ({ signal }) => listAccounts({
      provider, page, pageSize, search: debouncedSearch, type: typeFilter, status: statusFilter,
      renewal: provider === "grok_build" ? renewalFilter : undefined,
      risk: riskFilter || undefined,
      agreement: provider === "grok_web" ? agreementFilter : undefined,
      association: associationFilter || undefined,
      sortBy: sort.field, sortOrder: sort.order,
    }, signal),
  });

  const summaryQuery = useQuery({
    queryKey: ["accounts", "summary"],
    refetchOnMount: "always",
    queryFn: ({ signal }) => getAccountSummary(signal),
  });

  const invalidateAccountData = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    void queryClient.invalidateQueries({ queryKey: ["accounts", "summary"] });
  }, [queryClient]);

  const { open: deviceOpen, session: deviceSession, status: deviceStatus, start: startDeviceLogin, onOpenChange: setDeviceOpen } = useDeviceLogin(invalidateAccountData);

  useEffect(() => {
    if (!deleting && !batchDeleteOpen) return;
    const ids = deleting ? [deleting.id] : (selectedIdsKey ? selectedIdsKey.split(",") : []);
    if (linkedDeleteTargets.length === 0 || ids.length === 0) return;
    const controller = new AbortController();
    let cancelled = false;
    // Keep dialog height stable: never mount/unmount loading rows; only update counts in place.
    // Clear error/counts only inside the async path (not sync in effect body) to satisfy react-hooks/set-state-in-effect.
    const timer = window.setTimeout(() => {
      void previewAccountDeletion(ids, provider, linkedDeleteTargets, controller.signal)
        .then((preview) => {
          if (cancelled) return;
          // Always materialize a count for every selected target (including 0),
          // otherwise a missing key would leave the spinner forever.
          const next: Partial<Record<AccountProvider, number>> = {};
          for (const target of linkedDeleteTargets) {
            next[target] = preview.linkedByProvider?.[target] ?? 0;
          }
          setLinkedDeletePreviewError(false);
          setLinkedDeleteCounts(next);
        })
        .catch(() => {
          if (cancelled) return;
          // Do NOT write fake +0 — that understates destructive scope. Block confirm until retry succeeds.
          setLinkedDeleteCounts({});
          setLinkedDeletePreviewError(true);
          toast.error(t("accounts.linkedDeletePreviewFailed"));
        });
    }, 300);
    return () => {
      cancelled = true;
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [batchDeleteOpen, deleting, linkedDeleteTargets, provider, selectedIdsKey, t]);


  const linkedTargetOptions = (current: AccountProvider): AccountProvider[] =>
    (["grok_web", "grok_build", "grok_console"] as AccountProvider[]).filter((item) => item !== current);

  const linkedTargetLabel = (value: AccountProvider) => {
    if (value === "grok_build") return "Grok Build";
    if (value === "grok_console") return "Grok Console";
    return "Grok Web";
  };

  const linkedTargetIcon = (value: AccountProvider) => {
    if (value === "grok_build") return SquareTerminal;
    if (value === "grok_console") return Webhook;
    return Compass;
  };

  const linkedTargetIconClass = (value: AccountProvider) => {
    if (value === "grok_build") return "text-quota-product-1";
    if (value === "grok_console") return "text-quota-product-4";
    return "text-quota-product-2";
  };

  // Count is display-only. Until preview returns, show a tiny spinner — never flash +0 then +N.
  // On preview error keep targets checked but show failure (not +0) and block confirm.
  const linkedCountPending = (target: AccountProvider, checked: boolean) =>
    checked && !linkedDeletePreviewError && !(target in linkedDeleteCounts);

  const linkedCountFailed = (target: AccountProvider, checked: boolean) =>
    checked && linkedDeletePreviewError && !(target in linkedDeleteCounts);

  const linkedExtraLabel = (target: AccountProvider, checked: boolean) => {
    if (!checked) return "";
    if (linkedCountFailed(target, checked)) return t("accounts.linkedDeleteExtraFailed");
    if (linkedCountPending(target, checked)) return "";
    return t("accounts.linkedDeleteExtra", { count: linkedDeleteCounts[target] ?? 0 });
  };

  // When any linked target is checked, require a successful preview before confirm.
  const linkedPreviewBlocking =
    linkedDeleteTargets.length > 0 &&
    (linkedDeletePreviewError || linkedDeleteTargets.some((target) => !(target in linkedDeleteCounts)));

  const resetLinkedDeleteState = () => {
    setLinkedDeleteTargets([]);
    setLinkedDeleteCounts({});
    setLinkedDeletePreviewError(false);
  };

  const toggleLinkedDeleteTarget = (target: AccountProvider, checked: boolean) => {
    setLinkedDeleteTargets((current) => {
      const next = checked ? (current.includes(target) ? current : [...current, target]) : current.filter((item) => item !== target);
      if (next.length === 0) {
        setLinkedDeleteCounts({});
        setLinkedDeletePreviewError(false);
      } else if (!checked) {
        setLinkedDeleteCounts((counts) => {
          const copy = { ...counts };
          delete copy[target];
          return copy;
        });
      } else {
        // New selection: drop previous error and recount.
        setLinkedDeletePreviewError(false);
      }
      return next;
    });
  };

  const selectAllLinkedTargets = () => {
    const options = linkedTargetOptions(provider);
    const allSelected = options.every((item) => linkedDeleteTargets.includes(item));
    if (allSelected) {
      setLinkedDeleteTargets([]);
      setLinkedDeleteCounts({});
      return;
    }
    // Clear counts so newly selected targets show spinner until preview returns.
    setLinkedDeletePreviewError(false);
    setLinkedDeleteCounts({});
    setLinkedDeleteTargets(options);
  };

  const deleteMutation = useLifetimeMutation({
    // Snapshot id/targets in mutate() args so AlertDialog close/reset cannot clear linkedDeleteTargets mid-flight.
    mutationFn: (input: { id: string; provider: AccountProvider; linkedDeleteTargets: AccountProvider[] }, signal) =>
      deleteAccount(input.id, input.linkedDeleteTargets.length ? { provider: input.provider, linkedDeleteTargets: input.linkedDeleteTargets } : undefined, signal),
    onSuccess: () => {
      invalidateAccountData();
      setDeleting(null);
      resetLinkedDeleteState();
      toast.success(t("accounts.deleted"));
    },
    onError: showError,
  });

  // Batch paths skip groups that still have active media jobs; surface that instead of a bare success.
  const notifyDeleteResult = (result: { deleted?: number; skipped?: number } | { deleted: boolean }) => {
    const skipped = typeof result === "object" && "skipped" in result ? result.skipped ?? 0 : 0;
    if (skipped > 0) {
      const deleted = typeof result === "object" && typeof result.deleted === "number" ? result.deleted : 0;
      toast.warning(t("accounts.deletedWithSkipped", { deleted, skipped }));
      return;
    }
    toast.success(t("accounts.deleted"));
  };

  const billingMutation = useLifetimeMutation({
    mutationFn: refreshAccountBilling,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.billingRefreshed"));
    },
    onError: showError,
  });

  const tokenMutation = useLifetimeMutation({
    mutationFn: refreshAccountToken,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.authRefreshed"));
    },
    onError: showError,
  });

  const clearCooldownMutation = useLifetimeMutation({
    mutationFn: clearAccountCooldown,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.cooldownCleared"));
    },
    onError: showError,
  });

  const quotaMutation = useLifetimeMutation({
    mutationFn: refreshAccountQuota,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.billingRefreshed"));
    },
    onError: showError,
  });

  const webConfirmationMutation = useLifetimeMutation({
    mutationFn: ({ account, action }: WebAccountConfirmationTarget, signal) => {
      if (action === "acceptTerms") return acceptWebAccountTerms(account.id, signal);
      if (action === "setBirthDate") return setWebAccountBirthDate(account.id, signal);
      return enableWebAccountNSFW(account.id, signal);
    },
    onSuccess: (_, target) => {
      setWebConfirmationTarget(null);
      const messageKey = target.action === "acceptTerms"
        ? "webAccountSettings.termsAccepted"
        : target.action === "setBirthDate"
          ? "webAccountSettings.birthDateSaved"
          : "webAccountSettings.nsfwEnabled";
      toast.success(t(messageKey));
    },
    onError: showError,
    onSettled: invalidateAccountData,
  });

  const allTokenMutation = useLifetimeMutation({
    mutationFn: (_variables: void, signal) => {
      const controller = new AbortController();
      renewalAbortRef.current = controller;
      setRenewalProgress(null);
      return refreshAllAccountTokens(setRenewalProgress, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (result) => {
      setRenewAllOpen(false);
      toast.success(t("accounts.allTokensRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { renewalAbortRef.current = null; setRenewalProgress(null); invalidateAccountData(); },
  });

  const quotaSyncMutation = useLifetimeMutation({
    mutationFn: (targetProvider: AccountProvider, signal) => {
      const controller = new AbortController();
      quotaSyncAbortRef.current = controller;
      setQuotaSyncProgress(null);
      if (targetProvider === "grok_web") return refreshAllWebAccountQuotas(setQuotaSyncProgress, AbortSignal.any([signal, controller.signal]));
      if (targetProvider === "grok_console") return refreshAllConsoleAccountQuotas(setQuotaSyncProgress, AbortSignal.any([signal, controller.signal]));
      return refreshAllAccountBilling(setQuotaSyncProgress, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accounts.allBillingRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { quotaSyncAbortRef.current = null; setQuotaSyncProgress(null); invalidateAccountData(); },
  });

  const allQuotaResetMutation = useLifetimeMutation({
    mutationFn: (_variables: void, signal) => resetAllAccountQuota(signal),
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accountQuotaReset.completed", result));
    },
    onError: showError,
    onSettled: invalidateAccountData,
  });
  const conversionMutation = useLifetimeMutation({
    mutationFn: (input: BuildConversionInput, signal) => {
      const controller = new AbortController();
      conversionAbortRef.current = controller;
      setConversionProgress(null);
      return convertWebAccountsToBuild(input, setConversionProgress, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (conversion) => {
      setConversionProgress(null);
      setWebConversionTargets(null);
      clearSelection();
      toast.success(t("accounts.conversionCompleted", conversion));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => {
      conversionAbortRef.current = null;
      setConversionProgress(null);
      invalidateAccountData();
      void queryClient.invalidateQueries({ queryKey: ["models"] });
    },
  });

  const webConsoleSyncMutation = useLifetimeMutation({
    mutationFn: (input: WebConsoleSyncInput, signal) => {
      const controller = new AbortController();
      webConsoleSyncAbortRef.current = controller;
      setWebConsoleSyncProgress(null);
      return syncWebAccountsToConsole(input, setWebConsoleSyncProgress, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (result) => {
      setWebConversionTargets(null);
      clearSelection();
      toast.success(t("webConsoleSync.completed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => {
      webConsoleSyncAbortRef.current = null;
      setWebConsoleSyncProgress(null);
      invalidateAccountData();
      void queryClient.invalidateQueries({ queryKey: ["models"] });
    },
  });

  const webAccountScriptsMutation = useLifetimeMutation({
    mutationFn: (input: WebAccountScriptsInput, signal) => {
      const controller = new AbortController();
      webAccountScriptsAbortRef.current = controller;
      setWebAccountScriptsProgress(null);
      return runWebAccountScripts(input, setWebAccountScriptsProgress, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (result) => {
      setWebAccountScriptsTargets(null);
      clearSelection();
      if (result.failed > 0) {
        toast.warning(t("webAccountScripts.completedWithFailures", result));
      } else {
        toast.success(t("webAccountScripts.completed", result));
      }
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => {
      webAccountScriptsAbortRef.current = null;
      setWebAccountScriptsProgress(null);
      invalidateAccountData();
    },
  });

  const importMutation = useLifetimeMutation({
    mutationFn: (files: File[], signal) => {
      const controller = new AbortController();
      importAbortRef.current = controller;
      const toastID = toast.loading(t("common.importingProgress", { completed: 0, total: "…" }));
      importToastRef.current = toastID;
      const onProgress = (progress: AccountTaskProgressDTO) => {
        toast.loading(t(progress.phase === "syncing" ? "common.syncingProgress" : "common.importingProgress", progress), { id: toastID });
      };
      if (provider === "grok_web") return importWebAccounts(files, onProgress, AbortSignal.any([signal, controller.signal]));
      if (provider === "grok_console") return importConsoleAccounts(files, onProgress, AbortSignal.any([signal, controller.signal]));
      return importAccounts(files, onProgress, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (result) => {
      if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
      importToastRef.current = null;
      importAbortRef.current = null;
      setQuickImportOpen(false);
      setQuickImportTokens("");
      if (result.failed > 0) {
        toast.warning(t("accounts.importedWithFailures", result));
        return;
      }
      if (result.syncFailed > 0) {
        toast.warning(t("accounts.importedWithSyncFailures", result));
        return;
      }
      toast.success(t("accounts.imported", result));
    },
    onError: (error) => {
      if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
      importToastRef.current = null;
      importAbortRef.current = null;
      if (!isAbortError(error)) showError(error);
    },
    onSettled: () => {
      importAbortRef.current = null;
      invalidateAccountData();
    },
  });

  const exportMutation = useLifetimeMutation({
    mutationFn: async (input: { kind: "selected"; ids: string[] } | { kind: "batch"; limit: number; afterId: string; snapshotMaxId: string; batchNumber: number }, signal) => {
      if (input.kind === "selected") {
        return { kind: input.kind, blob: await exportSelectedAccounts(provider, input.ids, signal) } as const;
      }
      return { kind: input.kind, batchNumber: input.batchNumber, batch: await exportAccountBatch(provider, input.limit, input.afterId, input.snapshotMaxId, signal) } as const;
    },
    onSuccess: (result) => {
      if (result.kind === "selected") {
        downloadAccountExport(result.blob, provider, "selected");
        setExportOpen(false);
        toast.success(t("accounts.exported"));
        return;
      }
      downloadAccountExport(result.batch.blob, provider, `batch-${String(result.batchNumber).padStart(4, "0")}`);
      const completed = exportCompletedCount + result.batch.count;
      if (result.batch.hasMore) {
        setExportCursor(result.batch.nextId);
        setExportSnapshotMaxId(result.batch.snapshotMaxId);
        setExportBatchNumber(result.batchNumber + 1);
        setExportCompletedCount(completed);
        toast.success(t("accountExport.batchCompleted", { count: result.batch.count }));
        return;
      }
      setExportOpen(false);
      toast.success(t("accountExport.completed", { count: completed }));
    },
    onError: showError,
  });

  const batchUpdateMutation = useLifetimeMutation({
    mutationFn: (enabled: boolean, signal) => updateAccountsEnabled([...selected], enabled, provider, signal),
    onSuccess: () => {
      clearSelection();
      invalidateAccountData();
      toast.success(t("accounts.batchUpdated"));
    },
    onError: showError,
  });

  const batchConcurrencyMutation = useLifetimeMutation({
    mutationFn: (maxConcurrent: number, signal) => updateAccountsMaxConcurrent([...selected], maxConcurrent, provider, signal),
    onSuccess: () => {
      setBatchConcurrencyOpen(false);
      clearSelection();
      invalidateAccountData();
      toast.success(t("accounts.batchConcurrencyUpdated"));
    },
    onError: showError,
  });

  const batchBillingMutation = useLifetimeMutation({
    mutationFn: (_variables: void, signal) => refreshAccountsQuota([...selected], provider, signal),
    onSuccess: (result) => {
      clearSelection();
      setBatchQuotaTaskOpen(false);
      invalidateAccountData();
      toast.success(t("accounts.batchBillingRefreshed", result));
    },
    onError: showError,
  });

  const appendDetectItem = useCallback((item: BuildDetectItemDTO) => {
    const previousOutcome = detectOutcomeByIDRef.current.get(item.id);
    detectOutcomeByIDRef.current.set(item.id, item.outcome);
    if (previousOutcome !== item.outcome) {
      setDetectCounts((previous) => ({
        ...previous,
        ...(previousOutcome ? { [previousOutcome]: Math.max(0, previous[previousOutcome] - 1) } : {}),
        [item.outcome]: previous[item.outcome] + 1,
      }));
    }
    setDetectItems((prev) => {
      const next = prev.filter((entry) => entry.id !== item.id);
      next.unshift(item);
      return next.slice(0, 200);
    });
  }, []);

  const detectMutation = useLifetimeMutation({
    mutationFn: (mode: "selected" | "all", signal) => {
      const controller = new AbortController();
      detectAbortRef.current = controller;
      setDetectProgress(null);
      detectOutcomeByIDRef.current.clear();
      setDetectCounts(emptyBuildDetectCounts());
      setDetectItems([]);
      const handlers = {
        onProgress: setDetectProgress,
        onItem: appendDetectItem,
      };
      if (mode === "all") {
        return detectBuildAccounts({ all: true }, handlers, AbortSignal.any([signal, controller.signal]));
      }
      return detectBuildAccounts({ ids: [...selected] }, handlers, AbortSignal.any([signal, controller.signal]));
    },
    onSuccess: (result, mode) => {
      if (mode === "selected") clearSelection();
      toast.success(t(mode === "all" ? "accounts.allDetected" : "accounts.batchDetected", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => {
      detectAbortRef.current = null;
      invalidateAccountData();
    },
  });

  const openDetectDialog = (mode: "selected" | "all") => {
    setDetectMode(mode);
    setDetectProgress(null);
    detectOutcomeByIDRef.current.clear();
    setDetectCounts(emptyBuildDetectCounts());
    setDetectItems([]);
    setDetectDialogOpen(true);
  };

  const closeDetectDialog = (open: boolean) => {
    if (!open) {
      if (detectMutation.isPending) detectAbortRef.current?.abort();
      setDetectDialogOpen(false);
      setDetectProgress(null);
      // 保留结果列表直到下次打开，便于查看完成摘要；关闭后清空避免残留。
      if (!detectMutation.isPending) setDetectItems([]);
      return;
    }
    setDetectDialogOpen(true);
  };

  const batchQuotaResetMutation = useLifetimeMutation({
    mutationFn: (_variables: void, signal) => resetAccountsQuota([...selected], provider, signal),
    onSuccess: (result) => {
      clearSelection();
      setBatchQuotaTaskOpen(false);
      invalidateAccountData();
      toast.success(t("accountQuotaReset.completed", result));
    },
    onError: showError,
  });

  const batchTokenMutation = useLifetimeMutation({
    mutationFn: (_variables: void, signal) => refreshAccountsTokens([...selected], provider, signal),
    onSuccess: (result) => {
      clearSelection();
      invalidateAccountData();
      toast.success(t("accounts.allTokensRefreshed", result));
    },
    onError: showError,
  });

  const batchDeleteMutation = useLifetimeMutation({
    // Snapshot selection/targets at click time; dialog unmount/reset must not empty targets.
    mutationFn: (input: { ids: string[]; provider: AccountProvider; linkedDeleteTargets: AccountProvider[] }, signal) =>
      deleteAccounts(input.ids, input.provider, input.linkedDeleteTargets, signal),
    onSuccess: (result) => {
      clearSelection();
      setBatchDeleteOpen(false);
      resetLinkedDeleteState();
      invalidateAccountData();
      notifyDeleteResult(result);
    },
    onError: showError,
  });


  const resetCleanupState = () => {
    setCleanupStatuses(new Set());
    setCleanupLinkedTargets([]);
    setCleanupPreview(null);
    setCleanupPreviewError(false);
  };

  // Toggles never drop the previous preview: freshness is derived from the key below.
  const toggleCleanupTarget = (target: AccountProvider, checked: boolean) => {
    setCleanupLinkedTargets((current) => checked ? (current.includes(target) ? current : [...current, target]) : current.filter((item) => item !== target));
    setCleanupPreviewError(false);
  };

  const selectAllCleanupTargets = () => {
    const options = linkedTargetOptions(provider);
    const allSelected = options.every((item) => cleanupLinkedTargets.includes(item));
    setCleanupLinkedTargets(allSelected ? [] : options);
    setCleanupPreviewError(false);
  };

  const cleanupMutation = useLifetimeMutation({
    // Snapshot statuses/targets at click; dialog close/reset must not mutate an in-flight request.
    mutationFn: (input: { statuses: AccountCleanupStatus[]; targets: AccountProvider[] }, signal) =>
      cleanupAccounts(provider, input.statuses, input.targets, signal),
    onSuccess: (result) => {
      setCleanupOpen(false);
      resetCleanupState();
      invalidateAccountData();
      const linked = result.linkedDeleted ?? 0;
      const skipped = result.skipped ?? 0;
      if (linked > 0 || skipped > 0) {
        toast.success(t("accounts.cleanupCompletedDetailed", { deleted: result.deleted, linked, skipped }));
      } else {
        toast.success(t("accounts.cleanupCompleted", { deleted: result.deleted }));
      }
    },
    onError: showError,
  });

  // Debounced cleanup preview: counts refresh whenever statuses/targets change.
  // setState only inside timeout/promise callbacks (react-hooks/set-state-in-effect).
  const cleanupStatusesKey = [...cleanupStatuses].sort().join(",");
  const cleanupTargetsKey = [...cleanupLinkedTargets].sort().join(",");
  const cleanupPreviewKey = `${provider}|${cleanupStatusesKey}|${cleanupTargetsKey}`;
  // Fresh = the loaded preview matches the current selection; otherwise show spinners
  // in the fixed-size count slots and keep the confirm button disabled.
  const cleanupPreviewFresh = !cleanupPreviewError && cleanupPreview?.key === cleanupPreviewKey;
  const cleanupPreviewTotals = cleanupPreviewFresh ? cleanupPreview?.data ?? null : null;
  useEffect(() => {
    if (!cleanupOpen || cleanupStatusesKey === "") return;
    const controller = new AbortController();
    let cancelled = false;
    const previewKey = `${provider}|${cleanupStatusesKey}|${cleanupTargetsKey}`;
    const statuses = cleanupStatusesKey.split(",") as AccountCleanupStatus[];
    // Defense in depth: never send a target that is invalid for the current pool.
    const allowed = linkedTargetOptions(provider);
    const targets = (cleanupTargetsKey ? (cleanupTargetsKey.split(",") as AccountProvider[]) : []).filter((target) => allowed.includes(target));
    const timer = window.setTimeout(() => {
      void previewCleanup(provider, statuses, targets, controller.signal)
        .then((preview) => {
          if (cancelled) return;
          setCleanupPreviewError(false);
          setCleanupPreview({ key: previewKey, data: preview });
        })
        .catch(() => {
          if (cancelled) return;
          // No fake zeros: destructive scope must never be understated.
          setCleanupPreview(null);
          setCleanupPreviewError(true);
          toast.error(t("accounts.cleanupPreviewFailed"));
        });
    }, 300);
    return () => {
      cancelled = true;
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [cleanupOpen, cleanupStatusesKey, cleanupTargetsKey, provider, t]);

  function changeProvider(value: AccountProvider) {
    onProviderChange(value);
  }

  function submitQuickImport(): void {
    const value = quickImportTokens.trim();
    if (!value) return;
    const filename = provider === "grok_build" ? "grok-build-refresh-tokens.txt" : provider === "grok_console" ? "grok-console-sso-tokens.txt" : "grok-web-sso-tokens.txt";
    importMutation.mutate([new File([value], filename, { type: "text/plain" })]);
  }

  async function loadQuickImportFile(file: File | undefined): Promise<void> {
    if (!file) return;
    if (file.size > 30 * 1024 * 1024) {
      toast.error(t("apiErrors.accountImportFileTooLarge"));
      return;
    }
    try {
      const { signal } = beginImportFileRead();
      const text = await file.text();
      if (!signal.aborted) setQuickImportTokens(text);
    } catch {
      toast.error(t("errors.generic"));
    }
  }

  function openWebConversion(targets: string[] | "all"): void {
    setWebConversionTarget("build");
    setWebConversionStrategy("missing");
    setWebConversionTargets(targets);
  }

  function closeWebConversion(): void {
    conversionAbortRef.current?.abort();
    webConsoleSyncAbortRef.current?.abort();
    setWebConversionTargets(null);
  }

  function runWebConversion(): void {
    if (webConversionTargets === null) return;
    if (webConversionTarget === "build") {
      const input: BuildConversionInput = webConversionTargets === "all"
        ? { all: true, strategy: webConversionStrategy }
        : { ids: webConversionTargets, strategy: webConversionStrategy };
      conversionMutation.mutate(input);
      return;
    }
    const input: WebConsoleSyncInput = webConversionTargets === "all"
      ? { all: true, strategy: webConversionStrategy }
      : { ids: webConversionTargets, strategy: webConversionStrategy };
    webConsoleSyncMutation.mutate(input);
  }

  function runSelectedWebAccountScripts(actions: WebAccountScriptActions): void {
    if (webAccountScriptsTargets === "all") {
      webAccountScriptsMutation.mutate({ all: true, actions });
    } else if (webAccountScriptsTargets) {
      webAccountScriptsMutation.mutate({ ids: webAccountScriptsTargets, actions });
    }
  }

  function beginEdit(account: AccountDTO): void {
    setEditing(account);
  }

  const webConversionPending = conversionMutation.isPending || webConsoleSyncMutation.isPending;

  function showError(error: unknown): void {
    if (!lifetimeSignal().aborted) showErrorToast(error, t);
  }

  const result = accountsQuery.data;
  const pageIDs = result?.items.map((account) => account.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  function clearSelection(): void {
    setSelection((current) => ({ provider: current.provider, ids: new Set() }));
  }

  function resetExportProgress(): void {
    setExportCursor("0");
    setExportSnapshotMaxId("0");
    setExportBatchNumber(1);
    setExportCompletedCount(0);
  }

  function openProviderExport(): void {
    clearSelection();
    resetExportProgress();
    setExportOpen(true);
  }

  function openSelectedExport(): void {
    resetExportProgress();
    setExportOpen(true);
  }

  function togglePage(checked: boolean): void {
    setSelection((current) => {
      const next = new Set(current.provider === provider ? current.ids : []);
      for (const id of pageIDs) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return { provider, ids: next };
    });
  }

  function toggleAccount(id: string, checked: boolean): void {
    setSelection((current) => {
      const next = new Set(current.provider === provider ? current.ids : []);
      if (checked) next.add(id);
      else next.delete(id);
      return { provider, ids: next };
    });
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  const summary = summaryQuery.data;
  const recoveringAccounts = summary?.recovering ?? 0;
  const cooldownAccounts = summary?.recovery.cooldown ?? 0;
  const waitingResetAccounts = summary?.recovery.waitingReset ?? 0;
  const probingAccounts = summary?.recovery.probing ?? 0;
  const disabledAccounts = summary?.issues.disabled ?? 0;
  const invalidAccounts = summary?.issues.reauthRequired ?? 0;
  const riskAccounts = summary?.risk ?? 0;
  const abnormalAccounts = recoveringAccounts + disabledAccounts + invalidAccounts;
  const buildSummary = summary?.providers.grok_build ?? { total: 0, available: 0 };
  const webSummary = summary?.providers.grok_web ?? { total: 0, available: 0 };
  const consoleSummary = summary?.providers.grok_console ?? { total: 0, available: 0 };
  const summaryLoading = summaryQuery.isPending;
  const summaryUnavailable = summaryQuery.isError;
  const abnormalBreakdown = [
    { label: t("accounts.statusCooldown"), count: cooldownAccounts, tone: "bg-amber-500/10 text-amber-700 dark:text-amber-300" },
    { label: t("accounts.waitingReset"), count: waitingResetAccounts, tone: "bg-amber-500/10 text-amber-700 dark:text-amber-300" },
    { label: t("accounts.probing"), count: probingAccounts, tone: "bg-sky-500/10 text-sky-700 dark:text-sky-300" },
    { label: t("accounts.riskFilter"), count: riskAccounts, tone: "bg-orange-500/10 text-orange-700 dark:text-orange-300" },
    { label: t("accounts.statusDisabled"), count: disabledAccounts, tone: "bg-muted text-muted-foreground" },
    { label: t("accounts.statusReauthRequired"), count: invalidAccounts, tone: "bg-red-500/10 text-red-700 dark:text-red-300" },
  ];
  const abnormalDetail = abnormalBreakdown.map((item) => `${item.label} ${formatNumber(item.count, i18n.language, 0)}`).join(" · ");
  const abnormalDetailItems = summaryUnavailable
    ? [{ label: "-", value: "", tone: "bg-muted text-muted-foreground" }]
    : abnormalBreakdown
      .filter((item) => item.count > 0)
      .map((item) => ({ ...item, value: formatNumber(item.count, i18n.language, 0) }));
  if (!summaryUnavailable && abnormalDetailItems.length === 0) {
    abnormalDetailItems.push({ label: t("accounts.statusActive"), value: "", tone: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300", count: 0 });
  }
  const providerAccountTotal = provider === "grok_build" ? buildSummary.total : provider === "grok_web" ? webSummary.total : consoleSummary.total;
  const hasProviderAccounts = providerAccountTotal > 0 || (result?.total ?? 0) > 0;
  const bulkTaskPending = quotaSyncMutation.isPending
    || allQuotaResetMutation.isPending
    || allTokenMutation.isPending
    || conversionMutation.isPending
    || webConsoleSyncMutation.isPending
    || importMutation.isPending
    || batchUpdateMutation.isPending
    || batchConcurrencyMutation.isPending
    || batchBillingMutation.isPending
    || detectMutation.isPending
    || batchQuotaResetMutation.isPending
    || batchTokenMutation.isPending
    || batchDeleteMutation.isPending
    || cleanupMutation.isPending
    || webConfirmationMutation.isPending
    || webAccountScriptsMutation.isPending;

  const detectInvalidItems = detectItems.filter((item) => item.outcome === "invalid");
  const detectVisibleItems = detectMode === "selected" ? detectItems : detectInvalidItems;

  return (
    <div className="space-y-5">
      <header className="flex min-h-8 items-center">
        <h1 className="text-xl font-medium">{t("accounts.title")}</h1>
        <p className="sr-only">{t("console.accountsDescription")}</p>
      </header>
      <section className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
        <AccountMetricPanel tone="text-quota-product-1" icon={<SquareTerminal />} loading={summaryLoading} label={t("accounts.buildAccountCount")} value={summaryUnavailable ? "-" : formatNumber(buildSummary.total, i18n.language, 0)} detail={t("accounts.routableAccountCount", { count: formatNumber(buildSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel tone="text-quota-product-2" icon={<Compass />} loading={summaryLoading} label={t("accounts.webAccountCount")} value={summaryUnavailable ? "-" : formatNumber(webSummary.total, i18n.language, 0)} detail={t("accounts.routableAccountCount", { count: formatNumber(webSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel tone="text-quota-product-4" icon={<Webhook />} loading={summaryLoading} label={t("accounts.consoleAccountCount")} value={summaryUnavailable ? "-" : formatNumber(consoleSummary.total, i18n.language, 0)} detail={t("accounts.routableAccountCount", { count: formatNumber(consoleSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel
          tone={abnormalAccounts > 0 ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground"}
          icon={<TriangleAlert />}
          loading={summaryLoading}
          label={t("accounts.abnormalAccountCount")}
          value={summaryUnavailable ? "-" : formatNumber(abnormalAccounts, i18n.language, 0)}
          detail={abnormalDetail}
          detailItems={abnormalDetailItems}
        />
      </section>
      <div className="space-y-5">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Tabs value={provider} onValueChange={(value) => changeProvider(value as AccountProvider)}>
            <TabsList>
              <TabsTrigger value="grok_build" className="gap-1.5">
                <SquareTerminal className="size-3.5 text-quota-product-1" />
                <span>Grok Build</span>
              </TabsTrigger>
              <TabsTrigger value="grok_web" className="gap-1.5">
                <Compass className="size-3.5 text-quota-product-2" />
                <span>Grok Web</span>
              </TabsTrigger>
              <TabsTrigger value="grok_console" className="gap-1.5">
                <Webhook className="size-3.5 text-quota-product-4" />
                <span>Grok Console</span>
              </TabsTrigger>
            </TabsList>
          </Tabs>
          <DropdownMenu>
            <DropdownMenuTrigger asChild><Button size="sm"><Plus />{t("accounts.connectAccount")}</Button></DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {provider === "grok_build" ? <DropdownMenuItem onClick={() => void startDeviceLogin()}><ExternalLink />{t("accounts.deviceLogin")}</DropdownMenuItem> : null}
              <DropdownMenuItem disabled={bulkTaskPending} onClick={() => setQuickImportOpen(true)}><ClipboardPaste />{t(provider === "grok_build" ? "accounts.quickImportRT" : "accounts.quickImportSSO")}</DropdownMenuItem>
              <DropdownMenuItem disabled={bulkTaskPending} onClick={() => fileInputRef.current?.click()}><FileUp />{provider === "grok_build" ? t("accounts.importAuth") : provider === "grok_console" ? t("console.importFile") : t("accounts.importWebFile")}</DropdownMenuItem>
              {hasProviderAccounts ? (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem onClick={openProviderExport}><Download />{t("accounts.exportAuth")}</DropdownMenuItem>
                </>
              ) : null}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          accept="application/json,text/plain,.json,.txt"
          className="hidden"
          onChange={(event) => {
            const files = Array.from(event.target.files ?? []);
            if (files.length > 0) {
              importMutation.mutate(files);
            }
            event.target.value = "";
          }}
        />

        <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("accounts.search")} aria-label={t("accounts.search")} />
              </div>
              <DataTableFilters filters={[
                ...(provider === "grok_console" ? [] : [{ id: "type", label: t("accountType.label"), value: typeFilter, onChange: (value: string) => { setTypeFilter(value); setPage(1); }, options: provider === "grok_web" ? [
                  { value: "auto", label: t("accountType.auto") },
                  { value: "basic", label: t("accountType.free") },
                  { value: "super", label: t("accountType.super") },
                  { value: "heavy", label: t("accountType.heavy") },
                ] : [
                  { value: "free", label: t("accountType.free") },
                  { value: "paid", label: t("accountType.paid") },
                  { value: "unknown", label: t("accountType.pending") },
                ] }]),
                { id: "status", label: t("accounts.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "active", label: t("accounts.statusActive") },
                  { value: "disabled", label: t("accounts.statusDisabled") },
                  { value: "reauthRequired", label: t("accounts.statusReauthRequired") },
                  { value: "cooldown", label: t("accounts.statusCooldown") },
                  { value: "waitingReset", label: t("accounts.waitingReset") },
                  { value: "probing", label: t("accounts.probing") },
                ] },
                ...(provider === "grok_build" ? [{ id: "renewal", label: t("accountCredential.label"), value: renewalFilter, onChange: (value: string) => { setRenewalFilter(value); setPage(1); }, options: [
                  { value: "refreshable", label: t("accountCredential.autoRefresh") },
                  { value: "unrefreshable", label: t("accountCredential.noAutoRefresh") },
                ] }] : []),
                { id: "risk", label: t("accounts.riskFilter"), value: riskFilter, onChange: (value: string) => { setRiskFilter(value); setPage(1); }, options: [
                  { value: "flagged", label: t("accounts.botRisk") },
                  { value: "normal", label: t("accounts.riskNormal") },
                ] },
                ...(provider === "grok_web" ? [{ id: "agreement", label: t("accounts.agreementFilter"), value: agreementFilter, onChange: (value: string) => { setAgreementFilter(value); setPage(1); }, options: [
                  { value: "nsfwEnabled", label: t("accounts.agreementNsfwEnabled") },
                  { value: "nsfwDisabled", label: t("accounts.agreementNsfwDisabled") },
                  { value: "termsAccepted", label: t("accounts.agreementTermsAccepted") },
                  { value: "termsNotAccepted", label: t("accounts.agreementTermsNotAccepted") },
                  { value: "allAccepted", label: t("accounts.agreementAllAccepted") },
                  { value: "allNotAccepted", label: t("accounts.agreementAllNotAccepted") },
                ] }] : []),
                { id: "association", label: t("accounts.associationFilter"), value: associationFilter, onChange: (value: string) => { setAssociationFilter(value); setPage(1); }, options: provider === "grok_web" ? [
                  { value: "buildLinked", label: t("accounts.associationBuildLinked") },
                  { value: "buildUnlinked", label: t("accounts.associationBuildUnlinked") },
                  { value: "consoleLinked", label: t("accounts.associationConsoleLinked") },
                  { value: "consoleUnlinked", label: t("accounts.associationConsoleUnlinked") },
                  { value: "allLinked", label: t("accounts.associationAllLinked") },
                  { value: "allUnlinked", label: t("accounts.associationAllUnlinked") },
                ] : [
                  { value: "webLinked", label: t("accounts.associationWebLinked") },
                  { value: "webUnlinked", label: t("accounts.associationWebUnlinked") },
                ] },
              ]} />
            </div>
            {selected.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={openSelectedExport}><Download />{t("accounts.exportAuth")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => {
                  setBatchMaxConcurrent("1");
                  setBatchConcurrencyOpen(true);
                }}>{t("accounts.batchSetConcurrency")}</Button>
                {provider === "grok_web" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openWebConversion([...selected])}>{t("accountConversion.action")}</Button> : null}
                {provider === "grok_web" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => setWebAccountScriptsTargets([...selected])}>{t("webAccountScripts.action")}</Button> : null}
                {provider === "grok_build" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openDetectDialog("selected")}>{t("accountCredential.detectAction")}</Button> : null}
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => {
                  if (provider === "grok_build") {
                    setBatchQuotaTask("sync");
                    setBatchQuotaTaskOpen(true);
                    return;
                  }
                  batchBillingMutation.mutate();
                }}>{t("accountCredential.quotaSyncAction")}</Button>
                {provider === "grok_build" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchTokenMutation.mutate()}>{t("accountCredential.refreshAction")}</Button> : null}
                <Button variant="secondary" size="sm" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={bulkTaskPending} onClick={() => { resetLinkedDeleteState(); setBatchDeleteOpen(true); }}>{t("common.delete")}</Button>
              </div>
            ) : (
              <div className="flex flex-wrap items-center justify-end gap-1.5">
                {provider === "grok_web" && hasProviderAccounts ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openWebConversion("all")}>{t("accountConversion.action")}</Button> : null}
                {provider === "grok_web" && hasProviderAccounts ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => setWebAccountScriptsTargets("all")}>{t("webAccountScripts.action")}</Button> : null}
                {hasProviderAccounts && provider === "grok_build" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openDetectDialog("all")}>{t("accountCredential.detectAction")}</Button> : null}
                {hasProviderAccounts ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => { setAllQuotaTask("sync"); setSyncAllOpen(true); }}>{t("accountCredential.quotaSyncAction")}</Button> : null}
                {hasProviderAccounts && provider === "grok_build" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => setRenewAllOpen(true)}>{t("accountCredential.refreshAction")}</Button> : null}
                {hasProviderAccounts ? <Button variant="secondary" size="sm" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={bulkTaskPending} onClick={() => { resetCleanupState(); setCleanupOpen(true); }}><Trash2 />{t("accounts.cleanupAction")}</Button> : null}
              </div>
            )}
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {accountsQuery.isError ? <ErrorState message={accountsQuery.error.message} onRetry={() => void accountsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {accountsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={56} className="table-fixed border-collapse min-w-[780px] xl:min-w-[960px] 2xl:min-w-[1080px]">
            <colgroup>
              <col style={{ width: "3%" }} />
              <col style={{ width: "18%" }} />
              <col style={{ width: "7%" }} />
              <col style={{ width: "7%" }} />
              <col style={{ width: provider === "grok_build" ? "27%" : "43%" }} />
              {provider === "grok_build" ? <col style={{ width: "16%" }} /> : null}
              <col style={{ width: "18%" }} />
              <col style={{ width: "4%" }} />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="px-2"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("accounts.account")}</SortableTableHead>
                <SortableTableHead field="type" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accountType.label")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accounts.status")}</SortableTableHead>
                <TableHead className={cn("whitespace-nowrap", provider !== "grok_build" && "px-6")}>{t("accounts.quota")}</TableHead>
                {provider === "grok_build" ? <TableHead className="whitespace-nowrap pl-4">{t("accountCredential.label")}</TableHead> : null}
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort} className="whitespace-nowrap">{t("accounts.createdAt")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {accountsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={provider === "grok_build" ? 8 : 7} /></TableBody>
            ) : (
              <VirtualTableBody
                items={result?.items ?? []}
                colSpan={provider === "grok_build" ? 8 : 7}
                rowHeight={56}
                renderRow={(account) => (
	                  <TableRow className="group h-14 [&>td]:py-1.5" key={account.id} data-state={selected.has(account.id) ? "selected" : undefined}>
                    <TableCell className="px-2"><Checkbox checked={selected.has(account.id)} onCheckedChange={(checked) => toggleAccount(account.id, checked === true)} aria-label={t("common.selectItem", { name: account.name })} /></TableCell>
	                    <TableCell className="min-w-0"><AccountNameCell account={account} /></TableCell>
                    <TableCell className="text-center whitespace-nowrap">{provider === "grok_web" ? <WebAccountType tier={account.webTier} /> : provider === "grok_console" ? <AccountTypeText label={t("accountType.console")} variant="free" /> : <AccountType quota={account.quota} />}</TableCell>
                    <TableCell className="text-center whitespace-nowrap"><AccountStatus account={account} /></TableCell>
                    <TableCell className={provider === "grok_build" ? undefined : "px-6"}>{provider === "grok_web" ? <WebQuota windows={account.quotaWindows ?? []} locale={i18n.language} tier={account.webTier} /> : provider === "grok_console" ? <ConsoleQuota windows={account.quotaWindows ?? []} locale={i18n.language} /> : <AccountQuota quota={account.quota} billing={account.billing} locale={i18n.language} />}</TableCell>
                    {provider === "grok_build" ? <TableCell className="whitespace-nowrap pl-4 text-xs">
                      {account.refreshable ? (
                        <Tooltip>
                          <TooltipTrigger asChild><span tabIndex={0} className="cursor-help font-medium text-emerald-700 dark:text-emerald-300">{t("accountCredential.autoRefresh")}</span></TooltipTrigger>
                          <TooltipContent>{account.expiresAt ? t("accountCredential.expiresAt", { time: formatDateTime(account.expiresAt, i18n.language) }) : t("accountCredential.expiryUnknown")}</TooltipContent>
                        </Tooltip>
                      ) : <span className="font-medium text-amber-700 dark:text-amber-300">{t("accountCredential.noAutoRefresh")}</span>}
	                    </TableCell> : null}
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(account.createdAt, i18n.language)}</TableCell>
                    <TableActionCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => beginEdit(account)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                          {provider === "grok_web" ? <DropdownMenuItem onClick={() => openWebConversion([account.id])}><ArrowRight />{t("accountConversion.action")}</DropdownMenuItem> : null}
                          {provider === "grok_web" ? (
                            <WebAccountSettingsMenu
                              account={account}
                              disabled={bulkTaskPending}
                              onConfirm={setWebConfirmationTarget}
                            />
                          ) : null}
                          {provider === "grok_build" ? <DropdownMenuItem onClick={() => tokenMutation.mutate(account.id)}><RotateCw />{t("accounts.refreshToken")}</DropdownMenuItem> : null}
                          {account.cooldownUntil && new Date(account.cooldownUntil) > new Date() ? (
                            <DropdownMenuItem onClick={() => clearCooldownMutation.mutate(account.id)} disabled={clearCooldownMutation.isPending}>
                              <TimerOff />{t("accounts.clearCooldown")}
                            </DropdownMenuItem>
                          ) : null}
                          <DropdownMenuItem onClick={() => provider === "grok_build" ? billingMutation.mutate(account.id) : quotaMutation.mutate(account.id)}><RefreshCw />{provider === "grok_build" ? t("accounts.refreshBilling") : t("accounts.refreshModeQuota")}</DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => { resetLinkedDeleteState(); setDeleting(account); }}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableActionCell>
                  </TableRow>
                )}
              />
            )}
          </Table>
        ) : null}
        </DataTableShell>
      </div>

      <WebAccountSettingsDialogs
        confirmationTarget={webConfirmationTarget}
        confirmationPending={webConfirmationMutation.isPending}
        onConfirmationClose={() => setWebConfirmationTarget(null)}
        onConfirm={(target) => webConfirmationMutation.mutate(target)}
      />

      {webAccountScriptsTargets !== null ? (
        <WebAccountScriptsDialog
          targets={webAccountScriptsTargets}
          pending={webAccountScriptsMutation.isPending}
          progress={webAccountScriptsProgress}
          onClose={() => {
            webAccountScriptsAbortRef.current?.abort();
            setWebAccountScriptsTargets(null);
          }}
          onRun={runSelectedWebAccountScripts}
        />
      ) : null}

      <AlertDialog open={syncAllOpen} onOpenChange={(open) => {
        if (quotaSyncMutation.isPending || allQuotaResetMutation.isPending) return;
        if (!open) quotaSyncAbortRef.current?.abort();
        setSyncAllOpen(open);
      }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(provider === "grok_build" ? "accountQuotaTask.allTitle" : "accounts.syncAllTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t(provider === "grok_build" ? "accountQuotaTask.allDescription" : provider === "grok_web" ? "accounts.syncAllWebDescription" : "console.syncAllDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          {provider === "grok_build" ? (
            <div className="space-y-3">
              <Tabs value={allQuotaTask} onValueChange={(value) => setAllQuotaTask(value as BuildQuotaTask)}>
                <TabsList className="grid h-10 w-full grid-cols-2 p-1">
                  <TabsTrigger value="sync" className="h-8 font-normal" disabled={quotaSyncMutation.isPending || allQuotaResetMutation.isPending}>{t("accounts.refreshBilling")}</TabsTrigger>
                  <TabsTrigger value="reset" className="h-8 font-normal" disabled={quotaSyncMutation.isPending || allQuotaResetMutation.isPending}>{t("accountQuotaReset.action")}</TabsTrigger>
                </TabsList>
              </Tabs>
              <p className="min-h-10 text-xs leading-5 text-muted-foreground">{t(allQuotaTask === "sync" ? "accounts.syncAllDescription" : "accountQuotaTask.resetAllDescription")}</p>
            </div>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={quotaSyncMutation.isPending || allQuotaResetMutation.isPending} onClick={(event) => {
              event.preventDefault();
              if (provider === "grok_build" && allQuotaTask === "reset") allQuotaResetMutation.mutate();
              else quotaSyncMutation.mutate(provider);
            }}>
              {quotaSyncMutation.isPending ? <><Spinner />{quotaSyncProgress ? <span className="tabular-nums">{quotaSyncProgress.completed} / {quotaSyncProgress.total}</span> : t("common.loading")}</> : allQuotaResetMutation.isPending ? <Spinner /> : t(provider === "grok_build" ? "accountQuotaTask.execute" : "accounts.syncAll")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={detectDialogOpen} onOpenChange={closeDetectDialog}>
        <DialogContent className="max-w-xl gap-4 sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{detectMode === "all" ? t("accounts.detectAllTitle") : t("accounts.detectSelectedTitle", { count: selected.size })}</DialogTitle>
            <DialogDescription>{detectMode === "all" ? t("accounts.detectAllDescription") : t("accounts.detectSelectedDescription", { count: selected.size })}</DialogDescription>
          </DialogHeader>
          {(detectMutation.isPending || detectProgress || detectVisibleItems.length > 0) ? (
            <div className="space-y-3">
              <div className="flex items-center justify-between gap-3 rounded-md border bg-muted/30 px-3 py-2 text-sm">
                <span className="text-muted-foreground">{t("accounts.detectProgressLabel")}</span>
                <span className="tabular-nums font-medium">
                  {detectProgress ? `${detectProgress.completed} / ${detectProgress.total}` : detectMutation.isPending ? t("common.loading") : "—"}
                </span>
              </div>
              {detectMode === "all" && detectCounts.invalid > 0 ? (
                <p className="text-xs text-muted-foreground">{t("accounts.detectInvalidCount", { count: detectCounts.invalid })}</p>
              ) : null}
              {detectMode === "selected" && (detectCounts.ok + detectCounts.invalid + detectCounts.failed) > 0 ? (
                <p className="text-xs text-muted-foreground">
                  {t("accounts.detectSelectedSummary", {
                    ok: detectCounts.ok,
                    invalid: detectCounts.invalid,
                    failed: detectCounts.failed,
                  })}
                </p>
              ) : null}
              {(detectCounts.ok + detectCounts.invalid + detectCounts.failed) > detectVisibleItems.length ? (
                <p className="text-xs text-muted-foreground">{t("accounts.detectResultsLimited", { count: 200 })}</p>
              ) : null}
              <div className="max-h-64 overflow-y-auto rounded-md border">
                {detectVisibleItems.length === 0 ? (
                  <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                    {detectMutation.isPending
                      ? t(detectMode === "all" ? "accounts.detectWaitingInvalid" : "accounts.detectWaitingResults")
                      : t(detectMode === "all" ? "accounts.detectNoInvalid" : "accounts.detectNoResults")}
                  </div>
                ) : (
                  <ul className="divide-y">
                    {detectVisibleItems.map((item) => (
                      <li key={`${item.id}-${item.outcome}-${item.reason ?? ""}`} className="flex items-start gap-3 px-3 py-2 text-sm">
                        <Badge
                          variant="outline"
                          className={cn(
                            "mt-0.5 shrink-0",
                            item.outcome === "ok" && "border-emerald-500/40 text-emerald-700 dark:text-emerald-300",
                            item.outcome === "invalid" && "border-destructive/40 text-destructive",
                            item.outcome === "failed" && "border-amber-500/40 text-amber-700 dark:text-amber-300",
                          )}
                        >
                          {t(`accounts.detectOutcome.${item.outcome}`)}
                        </Badge>
                        <div className="min-w-0 flex-1">
                          <div className="truncate font-medium">{item.name || item.id}</div>
                          {item.email ? <div className="truncate text-xs text-muted-foreground">{item.email}</div> : null}
                          {item.reason ? <div className="mt-0.5 break-all text-xs text-muted-foreground">{item.reason}</div> : null}
                        </div>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          ) : null}
          <DialogFooter>
            <Button variant="outline" onClick={() => closeDetectDialog(false)}>{detectMutation.isPending ? t("common.cancel") : t("common.close")}</Button>
            <Button
              disabled={detectMutation.isPending || (detectMode === "selected" && selected.size === 0)}
              onClick={() => detectMutation.mutate(detectMode)}
            >
              {detectMutation.isPending ? (
                <>
                  <Spinner />
                  {detectProgress ? <span className="tabular-nums">{detectProgress.completed} / {detectProgress.total}</span> : t("common.loading")}
                </>
              ) : t("accounts.detectAll")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={webConversionTargets !== null} onOpenChange={(open) => { if (!open) closeWebConversion(); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("accountConversion.title")}</AlertDialogTitle>
            <AlertDialogDescription>{t(webConversionTargets === "all" ? "accountConversion.allDescription" : "accountConversion.selectedDescription", { count: Array.isArray(webConversionTargets) ? webConversionTargets.length : 0 })}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-2">
            <p id="web-conversion-target" className="text-xs font-medium">{t("accountConversion.target")}</p>
            <Tabs value={webConversionTarget} onValueChange={(value) => setWebConversionTarget(value as WebConversionTarget)}>
              <TabsList aria-labelledby="web-conversion-target" className="grid h-10 w-full grid-cols-2 p-1">
                <TabsTrigger value="build" className="h-8 gap-2 font-normal" disabled={webConversionPending}><SquareTerminal className="text-quota-product-1" />Grok Build</TabsTrigger>
                <TabsTrigger value="console" className="h-8 gap-2 font-normal" disabled={webConversionPending}><Webhook className="text-quota-product-4" />Grok Console</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
          <div className="space-y-2">
            <p id="web-conversion-strategy" className="text-xs font-medium">{t("accountConversion.strategy")}</p>
            <Tabs value={webConversionStrategy} onValueChange={(value) => setWebConversionStrategy(value as BuildConversionStrategy)}>
              <TabsList aria-labelledby="web-conversion-strategy" className="grid h-10 w-full grid-cols-2 p-1">
                <TabsTrigger value="missing" className="h-8 font-normal" disabled={webConversionPending}>{t("accountConversion.missing")}</TabsTrigger>
                <TabsTrigger value="all" className="h-8 font-normal" disabled={webConversionPending}>{t("accountConversion.all")}</TabsTrigger>
              </TabsList>
            </Tabs>
            <p className="min-h-8 text-xs text-muted-foreground">{t(webConversionTarget === "build"
              ? webConversionStrategy === "missing" ? "accountBulk.missingStrategyDescription" : "accountBulk.allStrategyDescription"
              : webConversionStrategy === "missing" ? "webConsoleSync.missingStrategyDescription" : "webConsoleSync.allStrategyDescription")}</p>
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={webConversionPending || webConversionTargets === null || (Array.isArray(webConversionTargets) && webConversionTargets.length === 0)} onClick={(event) => { event.preventDefault(); runWebConversion(); }}>
              {webConversionPending ? <><Spinner />{webConversionTarget === "build" && conversionProgress ? <span className="whitespace-nowrap tabular-nums">{t(conversionProgress.phase === "syncing" ? "accounts.syncingProgress" : "accounts.convertingProgress", conversionProgress)}</span> : webConversionTarget === "console" && webConsoleSyncProgress ? <span className="whitespace-nowrap tabular-nums">{t(webConsoleSyncProgress.phase === "syncing" ? "common.syncingProgress" : "common.importingProgress", webConsoleSyncProgress)}</span> : t("common.loading")}</> : t("accountConversion.start")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={renewAllOpen} onOpenChange={(open) => { if (!open) renewalAbortRef.current?.abort(); setRenewAllOpen(open); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.renewAllTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.renewAllDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction disabled={allTokenMutation.isPending} onClick={(event) => { event.preventDefault(); allTokenMutation.mutate(); }}>{allTokenMutation.isPending ? <><Spinner />{renewalProgress ? <span className="tabular-nums">{renewalProgress.completed} / {renewalProgress.total}</span> : t("common.loading")}</> : t("accounts.renewAll")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={exportOpen} onOpenChange={(open) => { if (!open && !exportMutation.isPending) setExportOpen(false); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.exportTitle", { provider: provider === "grok_build" ? "Grok Build" : provider === "grok_web" ? "Grok Web" : "Grok Console" })}</AlertDialogTitle><AlertDialogDescription>{t("accounts.exportDescription")}</AlertDialogDescription></AlertDialogHeader>
          {selected.size > 0 ? <p className="text-sm text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</p> : <div className="grid gap-2">
            <Label htmlFor="account-export-limit">{t("accounts.exportCount")}</Label>
            <Input id="account-export-limit" type="number" min={1} max={10000} value={exportLimit} disabled={exportSnapshotMaxId !== "0"} onChange={(event) => setExportLimit(event.target.value)} />
            <p className="text-xs text-muted-foreground">{t("accountExport.countDescription")}</p>
            {exportCompletedCount > 0 ? <p className="text-sm text-muted-foreground">{t("accountExport.batchProgress", { count: exportCompletedCount, batch: exportBatchNumber })}</p> : null}
          </div>}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={exportMutation.isPending}>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              disabled={exportMutation.isPending || (selected.size === 0 && (!Number.isInteger(Number(exportLimit)) || Number(exportLimit) < 1 || Number(exportLimit) > 10000))}
              onClick={(event) => {
                event.preventDefault();
                if (selected.size > 0) {
                  exportMutation.mutate({ kind: "selected", ids: [...selected] });
                  return;
                }
                exportMutation.mutate({ kind: "batch", limit: Number(exportLimit), afterId: exportCursor, snapshotMaxId: exportSnapshotMaxId, batchNumber: exportBatchNumber });
              }}
            >
              {exportMutation.isPending ? <Spinner /> : null}
              {selected.size === 0 && exportCompletedCount > 0 ? t("accountExport.nextBatch") : t("accounts.exportAuth")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={deviceOpen} onOpenChange={setDeviceOpen}>
        <DialogContent className="max-w-[460px] pb-6">
          <DialogHeader className="pr-7">
            <DialogTitle>{t("accounts.deviceTitle")}</DialogTitle>
            <DialogDescription>{t("accounts.deviceDescription")}</DialogDescription>
          </DialogHeader>
          {deviceStatus === "starting" ? <LoadingState className="min-h-28" /> : null}
          {deviceSession ? (
            <div className="space-y-4">
              <div className="rounded-lg bg-muted/50 px-3 py-2.5">
                <span className="text-[11px] text-muted-foreground">{t("accounts.userCode")}</span>
                <div className="mt-0.5 flex items-center justify-between gap-3">
                  <code className="min-w-0 select-all font-mono text-xl font-semibold tracking-[0.08em] tabular-nums">{deviceSession.userCode}</code>
                  <CopyButton value={deviceSession.userCode} className="-mr-1 size-7" onCopied={() => toast.success(t("common.copied"))} />
                </div>
                <p className="mt-2 text-[11px] leading-4 text-muted-foreground">{t("accounts.expiresAt", { time: formatDateTime(deviceSession.expiresAt, i18n.language) })}</p>
              </div>
              {deviceStatus === "pending" ? (
                <div className="flex min-h-10 items-center justify-between gap-4 pt-1" aria-live="polite">
                  <span className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground"><Spinner className="size-3.5" />{t("accounts.waiting")}</span>
                  <Button type="button" size="sm" className="shrink-0" onClick={() => window.open(deviceSession.verificationUriComplete || deviceSession.verificationUri, "_blank", "noopener,noreferrer")}>
                    <Link />{t("accounts.openVerification")}
                  </Button>
                </div>
              ) : null}
              {deviceStatus === "failed" ? (
                <div className="flex items-center justify-between gap-3">
                  <p className="text-xs text-muted-foreground">{t("apiErrors.deviceLoginFailed")}</p>
                  <Button type="button" variant="secondary" size="sm" className="shrink-0" onClick={() => void startDeviceLogin()}><RefreshCw />{t("common.retry")}</Button>
                </div>
              ) : null}
            </div>
          ) : null}
          {deviceStatus === "failed" && !deviceSession ? <Button type="button" variant="secondary" size="sm" className="justify-self-end" onClick={() => void startDeviceLogin()}><RefreshCw />{t("common.retry")}</Button> : null}
        </DialogContent>
      </Dialog>

      <Dialog open={quickImportOpen} onOpenChange={(open) => { setQuickImportOpen(open); if (!open) { cancelImportFileRead(); setQuickImportTokens(""); if (quickImportFileInputRef.current) quickImportFileInputRef.current.value = ""; } }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t(provider === "grok_build" ? "accounts.quickImportRTTitle" : provider === "grok_console" ? "console.quickImportTitle" : "accounts.quickImportTitle")}</DialogTitle>
            <DialogDescription>{t(provider === "grok_build" ? "accounts.quickImportRTDescription" : provider === "grok_console" ? "console.quickImportDescription" : "accounts.quickImportDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <div className="flex items-center justify-between gap-3">
              <Label htmlFor="quick-account-tokens">{t(provider === "grok_build" ? "accounts.refreshTokens" : "accounts.ssoTokens")}</Label>
              <Button type="button" variant="secondary" size="sm" disabled={importMutation.isPending} onClick={() => quickImportFileInputRef.current?.click()}><FileUp />{t("accounts.uploadTXT")}</Button>
              <input
                ref={quickImportFileInputRef}
                type="file"
                accept="text/plain,.txt"
                className="hidden"
                onChange={(event) => {
                  void loadQuickImportFile(event.target.files?.[0]);
                  event.target.value = "";
                }}
              />
            </div>
            <Textarea
              id="quick-account-tokens"
              className="min-h-56 font-mono"
              autoComplete="off"
              spellCheck={false}
              value={quickImportTokens}
              onChange={(event) => setQuickImportTokens(event.target.value)}
              placeholder={t(provider === "grok_build" ? "accounts.refreshTokenPlaceholder" : "accounts.ssoTokenPlaceholder")}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" onClick={() => { cancelImportFileRead(); setQuickImportOpen(false); setQuickImportTokens(""); }}>{t("common.cancel")}</Button>
            <Button type="button" size="sm" disabled={!quickImportTokens.trim() || importMutation.isPending} onClick={submitQuickImport}>{importMutation.isPending ? <Spinner /> : null}{t("accounts.importAction")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {editing ? <AccountEditor key={editing.id} account={editing} onClose={() => setEditing(null)} /> : null}

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => {
        if (!open) {
          // Do not clear linked targets while a delete request is in flight.
          if (deleteMutation.isPending) return;
          setDeleting(null);
          resetLinkedDeleteState();
        }
      }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>

            <div className="space-y-3 border-t pt-3">
              <div className="flex items-center justify-between gap-3">
                <p className="text-sm font-medium">{t("accounts.linkedDeleteTitle")}</p>
                <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={selectAllLinkedTargets}>
                  {linkedTargetOptions(provider).every((item) => linkedDeleteTargets.includes(item)) ? t("accounts.linkedDeleteClearAll") : t("accounts.linkedDeleteSelectAll")}
                </Button>
              </div>
              <div className="flex flex-wrap gap-x-6 gap-y-2">
                {linkedTargetOptions(provider).map((target) => {
                  const checked = linkedDeleteTargets.includes(target);
                  const TargetIcon = linkedTargetIcon(target);
                  const pending = linkedCountPending(target, checked);
                  const failed = linkedCountFailed(target, checked);
                  return (
                    <label key={target} className="flex min-h-6 items-center gap-2 text-sm">
                      <Checkbox checked={checked} onCheckedChange={(value) => toggleLinkedDeleteTarget(target, value === true)} />
                      <TargetIcon className={cn("size-3.5 shrink-0", linkedTargetIconClass(target))} aria-hidden />
                      <span className="inline-flex min-w-0 items-center gap-1.5">
                        <span>{linkedTargetLabel(target)}</span>
                        {/* Fixed slot: spinner while waiting, then +N — never show +0 as a fake result. */}
                        <span
                          className={cn(
                            "inline-flex h-4 min-w-[2.75rem] items-center justify-start tabular-nums text-xs",
                            failed ? "text-destructive" : "text-muted-foreground",
                            !checked && "invisible",
                          )}
                          aria-hidden={!checked}
                          aria-busy={pending}
                        >
                          {pending ? <Spinner className="size-3.5" /> : linkedExtraLabel(target, checked)}
                        </span>
                      </span>
                    </label>
                  );
                })}
              </div>
              <p className="min-h-4 text-xs text-muted-foreground">
                {linkedDeletePreviewError ? t("accounts.linkedDeletePreviewFailed") : t("accounts.linkedDeleteHint")}
              </p>
            </div>

          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending || !deleting || linkedPreviewBlocking} onClick={(event) => {
              event.preventDefault();
              if (!deleting || linkedPreviewBlocking) return;
              deleteMutation.mutate({ id: deleting.id, provider, linkedDeleteTargets: [...linkedDeleteTargets] });
            }}>{t("accounts.deleteConfirm")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={batchConcurrencyOpen} onOpenChange={(open) => {
        if (!open && batchConcurrencyMutation.isPending) return;
        setBatchConcurrencyOpen(open);
      }}>
        <DialogContent className="sm:max-w-[460px]">
          <DialogHeader>
            <DialogTitle>{t("accounts.batchConcurrencyTitle", { count: selected.size })}</DialogTitle>
            <DialogDescription>{t("accounts.batchConcurrencyDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="batch-account-concurrency">{t("accounts.maxConcurrent")}</Label>
            <Input
              id="batch-account-concurrency"
              type="number"
              min="1"
              max="256"
              value={batchMaxConcurrent}
              disabled={batchConcurrencyMutation.isPending}
              onChange={(event) => setBatchMaxConcurrent(event.target.value)}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" disabled={batchConcurrencyMutation.isPending} onClick={() => setBatchConcurrencyOpen(false)}>{t("common.cancel")}</Button>
            <Button
              type="button"
              size="sm"
              disabled={batchConcurrencyMutation.isPending || !Number.isInteger(Number(batchMaxConcurrent)) || Number(batchMaxConcurrent) < 1 || Number(batchMaxConcurrent) > 256}
              onClick={() => batchConcurrencyMutation.mutate(Number(batchMaxConcurrent))}
            >
              {batchConcurrencyMutation.isPending ? <Spinner /> : null}
              {t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={(open) => {
        if (!open && batchDeleteMutation.isPending) return;
        setBatchDeleteOpen(open);
        if (!open) resetLinkedDeleteState();
      }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle><AlertDialogDescription>{t("accounts.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>

            <div className="space-y-3 border-t pt-3">
              <div className="flex items-center justify-between gap-3">
                <p className="text-sm font-medium">{t("accounts.linkedDeleteTitle")}</p>
                <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={selectAllLinkedTargets}>
                  {linkedTargetOptions(provider).every((item) => linkedDeleteTargets.includes(item)) ? t("accounts.linkedDeleteClearAll") : t("accounts.linkedDeleteSelectAll")}
                </Button>
              </div>
              <div className="flex flex-wrap gap-x-6 gap-y-2">
                {linkedTargetOptions(provider).map((target) => {
                  const checked = linkedDeleteTargets.includes(target);
                  const TargetIcon = linkedTargetIcon(target);
                  const pending = linkedCountPending(target, checked);
                  const failed = linkedCountFailed(target, checked);
                  return (
                    <label key={target} className="flex min-h-6 items-center gap-2 text-sm">
                      <Checkbox checked={checked} onCheckedChange={(value) => toggleLinkedDeleteTarget(target, value === true)} />
                      <TargetIcon className={cn("size-3.5 shrink-0", linkedTargetIconClass(target))} aria-hidden />
                      <span className="inline-flex min-w-0 items-center gap-1.5">
                        <span>{linkedTargetLabel(target)}</span>
                        <span
                          className={cn(
                            "inline-flex h-4 min-w-[2.75rem] items-center justify-start tabular-nums text-xs",
                            failed ? "text-destructive" : "text-muted-foreground",
                            !checked && "invisible",
                          )}
                          aria-hidden={!checked}
                          aria-busy={pending}
                        >
                          {pending ? <Spinner className="size-3.5" /> : linkedExtraLabel(target, checked)}
                        </span>
                      </span>
                    </label>
                  );
                })}
              </div>
              <p className="min-h-4 text-xs text-muted-foreground">
                {linkedDeletePreviewError ? t("accounts.linkedDeletePreviewFailed") : t("accounts.linkedDeleteHint")}
              </p>
            </div>

          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={batchDeleteMutation.isPending || selected.size === 0 || linkedPreviewBlocking} onClick={(event) => {
              event.preventDefault();
              if (linkedPreviewBlocking) return;
              batchDeleteMutation.mutate({ ids: [...selected], provider, linkedDeleteTargets: [...linkedDeleteTargets] });
            }}>{t("accounts.deleteConfirm")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchQuotaTaskOpen} onOpenChange={(open) => {
        if (batchBillingMutation.isPending || batchQuotaResetMutation.isPending) return;
        setBatchQuotaTaskOpen(open);
      }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("accountQuotaTask.title", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription>{t("accountQuotaTask.description")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-3">
            <Tabs value={batchQuotaTask} onValueChange={(value) => setBatchQuotaTask(value as BuildQuotaTask)}>
              <TabsList className="grid h-10 w-full grid-cols-2 p-1">
                <TabsTrigger value="sync" className="h-8 font-normal" disabled={batchBillingMutation.isPending || batchQuotaResetMutation.isPending}>{t("accounts.refreshBilling")}</TabsTrigger>
                <TabsTrigger value="reset" className="h-8 font-normal" disabled={batchBillingMutation.isPending || batchQuotaResetMutation.isPending}>{t("accountQuotaReset.action")}</TabsTrigger>
              </TabsList>
            </Tabs>
            <p className="min-h-10 text-xs leading-5 text-muted-foreground">{t(batchQuotaTask === "sync" ? "accountQuotaTask.syncDescription" : "accountQuotaReset.description")}</p>
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={batchBillingMutation.isPending || batchQuotaResetMutation.isPending} onClick={(event) => {
              event.preventDefault();
              if (batchQuotaTask === "reset") batchQuotaResetMutation.mutate();
              else batchBillingMutation.mutate();
            }}>
              {batchBillingMutation.isPending || batchQuotaResetMutation.isPending ? <Spinner /> : null}
              {t("accountQuotaTask.execute")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <Dialog open={cleanupOpen} onOpenChange={(open) => { if (!cleanupMutation.isPending) { setCleanupOpen(open); if (!open) resetCleanupState(); } }}>
        <DialogContent className="max-w-[440px]">
          <DialogHeader>
            <DialogTitle>{t("accounts.cleanupTitle", { provider: provider === "grok_build" ? "Grok Build" : provider === "grok_web" ? "Grok Web" : "Grok Console" })}</DialogTitle>
            <DialogDescription>{t("accounts.cleanupDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5">
            {([
              ["cooldown", t("accounts.statusCooldown")],
              ["disabled", t("accounts.statusDisabled")],
              ["reauthRequired", t("accounts.statusReauthRequired")],
            ] as const).map(([status, label]) => {
              const checked = cleanupStatuses.has(status);
              const pending = checked && !cleanupPreviewError && !cleanupPreviewFresh;
              return (
                <label key={status} className="flex cursor-pointer items-center gap-3 rounded-md bg-muted/40 px-3 py-2.5 text-xs">
                  <Checkbox
                    checked={checked}
                    disabled={cleanupMutation.isPending}
                    onCheckedChange={(value) => {
                      setCleanupStatuses((current) => {
                        const next = new Set(current);
                        if (value === true) next.add(status); else next.delete(status);
                        return next;
                      });
                      setCleanupPreviewError(false);
                    }}
                  />
                  <span>{label}</span>
                  {/* Fixed count slot: spinner while previewing, then the matched root count. */}
                  <span
                    className={cn(
                      "ml-auto inline-flex h-4 min-w-[2.5rem] items-center justify-end tabular-nums text-xs",
                      cleanupPreviewError ? "text-destructive" : "text-muted-foreground",
                      !checked && "invisible",
                    )}
                    aria-hidden={!checked}
                    aria-busy={pending}
                  >
                    {!checked ? null : cleanupPreviewError ? "!" : pending ? <Spinner className="size-3.5" /> : cleanupPreviewTotals?.rootsByStatus?.[status] ?? 0}
                  </span>
                </label>
              );
            })}
          </div>

          {/* Smooth-expand linked deletion block, shown once any status is selected. */}
          <div
            className={cn(
              "grid transition-all duration-300 ease-in-out",
              cleanupStatuses.size > 0 ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0",
            )}
            aria-hidden={cleanupStatuses.size === 0}
          >
            <div className="overflow-hidden">
              <div className="space-y-3 border-t pt-3">
                <div className="flex items-center justify-between gap-3">
                  <p className="text-sm font-medium">{t("accounts.linkedDeleteTitle")}</p>
                  <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" disabled={cleanupMutation.isPending} onClick={selectAllCleanupTargets}>
                    {linkedTargetOptions(provider).every((item) => cleanupLinkedTargets.includes(item)) ? t("accounts.linkedDeleteClearAll") : t("accounts.linkedDeleteSelectAll")}
                  </Button>
                </div>
                <div className="flex flex-wrap gap-x-6 gap-y-2">
                  {linkedTargetOptions(provider).map((target) => {
                    const checked = cleanupLinkedTargets.includes(target);
                    const TargetIcon = linkedTargetIcon(target);
                    const pending = checked && !cleanupPreviewError && !cleanupPreviewFresh;
                    return (
                      <label key={target} className="flex min-h-6 items-center gap-2 text-sm">
                        <Checkbox checked={checked} disabled={cleanupMutation.isPending} onCheckedChange={(value) => toggleCleanupTarget(target, value === true)} />
                        <TargetIcon className={cn("size-3.5 shrink-0", linkedTargetIconClass(target))} aria-hidden />
                        <span className="inline-flex min-w-0 items-center gap-1.5">
                          <span>{linkedTargetLabel(target)}</span>
                          <span
                            className={cn(
                              "inline-flex h-4 min-w-[2.75rem] items-center justify-start tabular-nums text-xs",
                              cleanupPreviewError ? "text-destructive" : "text-muted-foreground",
                              !checked && "invisible",
                            )}
                            aria-hidden={!checked}
                            aria-busy={pending}
                          >
                            {!checked ? "" : cleanupPreviewError ? t("accounts.linkedDeleteExtraFailed") : pending ? <Spinner className="size-3.5" /> : t("accounts.linkedDeleteExtra", { count: cleanupPreviewTotals?.linkedByProvider?.[target] ?? 0 })}
                          </span>
                        </span>
                      </label>
                    );
                  })}
                </div>
                {/* Stacked messages: the container keeps the tallest variant's height,
                    so switching hint/warning/error never resizes the dialog. */}
                <div className="grid text-xs">
                  {([
                    ["error", cleanupPreviewError, t("accounts.cleanupPreviewFailed"), "text-destructive"],
                    ["warning", !cleanupPreviewError && cleanupLinkedTargets.length > 0, t("accounts.cleanupLinkedWarning"), "text-destructive"],
                    ["hint", !cleanupPreviewError && cleanupLinkedTargets.length === 0, t("accounts.linkedDeleteHint"), "text-muted-foreground"],
                  ] as const).map(([key, visible, text, tone]) => (
                    <p key={key} aria-hidden={!visible} className={cn("col-start-1 row-start-1", tone, !visible && "invisible")}>{text}</p>
                  ))}
                </div>
                {/* Always rendered so the total line never unmounts between refreshes. */}
                <p className="flex min-h-4 items-center gap-1.5 text-xs text-muted-foreground" aria-busy={!cleanupPreviewFresh && !cleanupPreviewError}>
                  {cleanupPreviewError ? t("accounts.cleanupPreviewFailed") : !cleanupPreviewFresh ? <Spinner className="size-3.5" /> : t("accounts.cleanupPreviewTotal", { total: cleanupPreviewTotals?.total ?? 0 })}
                </p>
              </div>
            </div>
          </div>

          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" disabled={cleanupMutation.isPending} onClick={() => setCleanupOpen(false)}>{t("common.cancel")}</Button>
            <Button
              type="button"
              variant="destructive"
              size="sm"
              disabled={cleanupMutation.isPending || cleanupStatuses.size === 0 || cleanupPreviewError || !cleanupPreviewFresh}
              onClick={() => cleanupMutation.mutate({ statuses: [...cleanupStatuses], targets: [...cleanupLinkedTargets] })}
            >
              {cleanupMutation.isPending ? <Spinner /> : null}
              {t("accounts.cleanupStart")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
