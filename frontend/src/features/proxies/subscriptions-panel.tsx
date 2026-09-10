import { NetworkField as OperationsField } from "./network-ui";
import {
	NetworkDialogContent as DialogContent,
	NetworkDialogHeader as DialogHeader,
	NetworkDialogFooter as DialogFooter,
	NetworkButton as Button,
	NetworkText,
} from "./network-ui";
import { StatusPill } from "@/features/operations/operations-ui";
import { AlertDialogContent } from "@/components/ui/alert-dialog";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Inbox, Rss, MoreHorizontal, Pencil, Plus, RefreshCw, Search, Trash2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Dialog, DialogTitle } from "@/components/ui/dialog";
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
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
import { IntervalInput, OperationSectionHeader } from "./operations-context";
import { showError } from "./operations-shared";
import {
	validSubscriptionProxyURL,
	validSubscriptionURL,
} from "@/features/settings/settings-model";
import { formatDateTime } from "@/shared/lib/format";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import "./resources-panel.css";
import { Pagination } from "@/shared/components/pagination";

type SourceForm = Omit<EgressSourceInput, "url" | "proxyURL" | "clearProxyURL"> & {
	url: string;
	proxyEnabled: boolean;
	proxyURL: string;
};
type SourceSession = { controller: AbortController; urlEdited: boolean; proxyEdited: boolean };
const emptySource: SourceForm = {
	name: "",
	enabled: true,
	url: "",
	proxyEnabled: false,
	proxyURL: "",
	refreshIntervalSeconds: 900,
};

/** Sources are maintained inside the node inventory; changes take effect on save. */
export function SubscriptionsPanel({ showHeader = true }: { showHeader?: boolean }) {
	const { t, i18n } = useTranslation();
	const queryClient = useQueryClient();
	const [sourceEditing, setSourceEditing] = useState<EgressSourceDTO | null | undefined>();
	const [sourceForm, setSourceForm] = useState<SourceForm>(emptySource);
	const [deleting, setDeleting] = useState<EgressSourceDTO | null>(null);
	const [page, setPage] = useState(1),
		[pageSize, setPageSize] = useState(12),
		[search, setSearch] = useState("");
	const editor = useRef<SourceSession | null>(null);
	useEffect(() => () => { editor.current?.controller.abort(); editor.current = null; }, []);
	const sourcesQuery = useQuery({
		queryKey: ["egress-sources"],
		queryFn: () => listEgressSources(),
		staleTime: 15_000,
		refetchInterval: 30_000,
	});
	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
		void queryClient.invalidateQueries({ queryKey: ["egress-sources"] });
	};
	function closeSource() {
		editor.current?.controller.abort();
		editor.current = null;
		setSourceEditing(undefined);
	}
	const saveSource = useMutation({
		mutationFn: ({
			id,
			input,
		}: {
			id?: string;
			input: EgressSourceInput;
			session: SourceSession;
		}) => (id ? updateEgressSource(id, input) : createEgressSource(input)),
		onSuccess: (_, submission) => {
			invalidate();
			if (editor.current === submission.session) {
				if (!submission.id) setPage(1);
				closeSource();
			}
			toast.success(t("settings.egress.sourceSaved"));
		},
		onError: showError,
	});
	const removeSource = useMutation({
		mutationFn: deleteEgressSource,
		onSuccess: () => {
			if (page > 1 && pagedSources.length === 1) setPage(page - 1);
			setDeleting(null);
			invalidate();
			toast.success(t("settings.egress.sourceDeleted"));
		},
		onError: showError,
	});
	const syncSource = useMutation({
		mutationFn: syncEgressSource,
		onSuccess: (value) => {
			invalidate();
			toast.success(t("settings.egress.sourceSynced", value));
		},
		onError: showError,
	});
	function openSource(value?: EgressSourceDTO) {
		editor.current?.controller.abort();
		const session: SourceSession = {
			controller: new AbortController(),
			urlEdited: false,
			proxyEdited: false,
		};
		editor.current = session;
		setSourceForm(
			value
				? {
						name: value.name,
						enabled: value.enabled,
						url: "",
						refreshIntervalSeconds: value.refreshIntervalSeconds,
						proxyEnabled: value.proxyConfigured,
						proxyURL: "",
					}
				: emptySource,
		);
		setSourceEditing(value ?? null);
		if (value?.urlConfigured)
			void getEgressSourceURL(value.id, session.controller.signal)
				.then(({ url }) => {
					if (editor.current === session && !session.controller.signal.aborted && !session.urlEdited)
						setSourceForm((current) => ({ ...current, url }));
				})
				.catch(() => undefined);
		if (value?.proxyConfigured)
			void getEgressSourceProxyURL(value.id, session.controller.signal)
				.then(({ proxyURL }) => {
					if (editor.current === session && !session.controller.signal.aborted && !session.proxyEdited)
						setSourceForm((current) => ({ ...current, proxyURL }));
				})
				.catch(() => undefined);
	}
	const normalizedSearch = search.trim().toLocaleLowerCase();
	const filteredSources = (sourcesQuery.data?.items ?? []).filter((source) =>
		source.name.toLocaleLowerCase().includes(normalizedSearch),
	);
	const currentPage = Math.min(page, Math.max(1, Math.ceil(filteredSources.length / pageSize)));
	const pagedSources = filteredSources.slice((currentPage - 1) * pageSize, currentPage * pageSize);
	const sourceProxyInvalid =
		sourceForm.proxyEnabled &&
		Boolean(sourceForm.proxyURL.trim()) &&
		!validSubscriptionProxyURL(sourceForm.proxyURL);
	const sourceURLInvalid = Boolean(sourceForm.url.trim()) && !validSubscriptionURL(sourceForm.url);
	const sourceIntervalInvalid =
		!Number.isInteger(sourceForm.refreshIntervalSeconds) ||
		(sourceForm.refreshIntervalSeconds ?? 0) < 60 ||
		(sourceForm.refreshIntervalSeconds ?? 0) > 86400;
	function submitSource() {
		const session = editor.current;
		if (
			!session ||
			saveSource.isPending ||
			!sourceForm.name.trim() ||
			(!sourceEditing && !sourceForm.url.trim()) ||
			(sourceForm.proxyEnabled && !sourceEditing?.proxyConfigured && !sourceForm.proxyURL.trim()) ||
			sourceURLInvalid ||
			sourceProxyInvalid ||
			sourceIntervalInvalid
		)
			return;
		// Capture the submitted editor. A later save response must not close another source.
		saveSource.mutate({
			id: sourceEditing?.id,
			session,
			input: {
				name: sourceForm.name,
				enabled: sourceForm.enabled,
				url: sourceForm.url.trim() || undefined,
				proxyURL: sourceForm.proxyEnabled ? sourceForm.proxyURL.trim() || undefined : undefined,
				clearProxyURL: Boolean(sourceEditing?.proxyConfigured && !sourceForm.proxyEnabled),
				refreshIntervalSeconds: sourceForm.refreshIntervalSeconds,
			},
		});
	}
	return (
		<section className="nres-sources">
			{showHeader && (
				<OperationSectionHeader
					title={t("settings.egress.subscriptions")}
					help={t("ops.netSourcesHelp")}
				/>
			)}
			<div className="nres-source-search">
				<Search aria-hidden="true" />
				<Input value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("settings.egress.searchSubscriptions")} aria-label={t("settings.egress.searchSubscriptions")} />
				<Button type="button" size="sm" className="nres-source-add" onClick={() => openSource()}>
					<Plus />{t("settings.egress.addSource")}
				</Button>
			</div>
			{sourcesQuery.isError && <ErrorState message={sourcesQuery.error.message} onRetry={() => void sourcesQuery.refetch()} />}
			{sourcesQuery.isPending ? <LoadingState /> : !sourcesQuery.data ? null : pagedSources.length === 0 ? (
				<div className="nres-empty">
					<Inbox aria-hidden="true" />
					<p>{t(normalizedSearch ? "settings.egress.noSubscriptionMatches" : "settings.egress.noSources")}</p>
					{normalizedSearch && <Button type="button" variant="outline" size="sm" onClick={() => setSearch("")}>{t("networkResources.clearSearch")}</Button>}
				</div>
			) : (
				<div className="nres-source-grid">
					{pagedSources.map((source) => (
						<article key={source.id} className="nres-source-card">
							<header>
								<div className="nres-source-name"><Rss aria-hidden="true" /><h3><NetworkText>{source.name}</NetworkText></h3></div>
								<StatusPill tone={!source.enabled ? "neutral" : source.lastSyncError ? "warn" : "good"}>
									{t(!source.enabled ? "ops.netSourcePaused" : source.lastSyncError ? "ops.netSyncFailed" : "ops.netSourceAuto")}
								</StatusPill>
							</header>
							<dl className="nres-source-facts">
								<div><dt>{t("settings.egress.lastSync")}</dt><dd>{source.lastSyncedAt ? <time dateTime={source.lastSyncedAt}>{formatDateTime(source.lastSyncedAt, i18n.language)}</time> : t("settings.egress.never")}</dd></div>
								<div><dt>{t("networkResources.nextSync")}</dt><dd>{source.enabled && source.nextSyncAt ? <time dateTime={source.nextSyncAt}>{formatDateTime(source.nextSyncAt, i18n.language)}</time> : t("networkResources.notScheduled")}</dd></div>
								<div><dt>{t("ops.netLastImported")}</dt><dd>{source.lastSyncedAt ? t("networkResources.nodeCount", { count: source.lastSyncImported }) : "—"}</dd></div>
								<div><dt>{t("networkResources.refreshInterval")}</dt><dd>{source.refreshIntervalSeconds % 60 === 0 ? t("networkResources.minutes", { count: source.refreshIntervalSeconds / 60 }) : t("networkResources.seconds", { count: source.refreshIntervalSeconds })}</dd></div>
								<div><dt>{t("settings.egress.subscriptionProxy")}</dt><dd>{t(source.proxyConfigured ? "common.enable" : "ops.netDirect")}</dd></div>
							</dl>
							{source.lastSyncError && <div className="nres-source-error"><strong>{t("ops.netSyncFailed")}</strong><p>{source.lastSyncError}</p></div>}
							<footer>
								<Button type="button" variant="outline" size="sm" disabled={syncSource.isPending} onClick={() => syncSource.mutate(source.id)}>
									{syncSource.isPending && syncSource.variables === source.id ? <Spinner /> : <RefreshCw />}
									{t(source.lastSyncError ? "networkResources.retrySync" : "networkResources.syncNow")}
								</Button>
								<Button type="button" variant="ghost" size="sm" onClick={() => openSource(source)}><Pencil />{t("common.edit")}</Button>
								<DropdownMenu>
									<DropdownMenuTrigger asChild><Button variant="ghost" size="icon" aria-label={`${t("common.actions")} · ${source.name}`}><MoreHorizontal /></Button></DropdownMenuTrigger>
									<DropdownMenuContent align="end"><DropdownMenuItem className="text-destructive" onClick={() => setDeleting(source)}><Trash2 />{t("common.delete")}</DropdownMenuItem></DropdownMenuContent>
								</DropdownMenu>
							</footer>
						</article>
					))}
				</div>
			)}
			{filteredSources.length > 0 && <Pagination page={currentPage} pageSize={pageSize} pageSizeOptions={[12, 24, 48]} total={filteredSources.length} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} />}
			<AlertDialog
				open={Boolean(deleting)}
				onOpenChange={(open) => {
					if (!open && !removeSource.isPending) setDeleting(null);
				}}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							{t("ops.netDeleteSource", { name: deleting?.name })}
						</AlertDialogTitle>
						<AlertDialogDescription>{t("ops.netDeleteSourceHelp")}</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={removeSource.isPending}>
							{t("common.cancel")}
						</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-destructive-foreground"
							disabled={removeSource.isPending}
							onClick={(event) => {
								event.preventDefault();
								if (deleting) removeSource.mutate(deleting.id);
							}}
						>
							{removeSource.isPending && <Spinner />}
							{t("common.delete")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
			<Dialog
				open={sourceEditing !== undefined}
				onOpenChange={(open) => {
					if (!open) closeSource();
				}}
			>
				<DialogContent layout="editor" aria-describedby={undefined}>
					<DialogHeader className="pr-8">
						<DialogTitle>
							<Rss aria-hidden="true" />
							{sourceEditing ? t("settings.egress.editSource") : t("settings.egress.addSource")}
						</DialogTitle>
					</DialogHeader>
					<form
						className="net-editor-form"
						onSubmit={(event) => {
							event.preventDefault();
							event.stopPropagation();
							submitSource();
						}}
					>
						<fieldset className="net-editor-body nres-editor-fields" disabled={saveSource.isPending}>
							<div className="net-identity">
								<OperationsField controlId="source-name" label={t("settings.egress.name")}>
									<Input
										id="source-name"
										placeholder={t("ops.netSourceExample")}
										maxLength={160}
										value={sourceForm.name}
										onChange={(event) => setSourceForm({ ...sourceForm, name: event.target.value })}
									/>
								</OperationsField>
								<OperationsField controlId="source-enabled" label={t("settings.egress.enabled")}>
									<Switch
										id="source-enabled"
										checked={sourceForm.enabled}
										onCheckedChange={(enabled) => setSourceForm({ ...sourceForm, enabled })}
									/>
								</OperationsField>
							</div>
							<OperationsField
								controlId="source-url"
								label={t("settings.egress.subscriptionURL")}
								error={sourceURLInvalid ? t("ops.subscriptionInvalid") : undefined}
							>
								<Input
									id="source-url"
									className="net-address-input"
									type="text"
									autoComplete="off"
									aria-invalid={sourceURLInvalid}
									placeholder={
										sourceEditing?.urlConfigured ? t("ops.netKeepStoredAddress") : "https://..."
									}
									value={sourceForm.url}
									onChange={(event) => {
										if (editor.current) editor.current.urlEdited = true;
										setSourceForm({ ...sourceForm, url: event.target.value });
									}}
								/>
							</OperationsField>
							<OperationsField
								controlId="egress-source-refresh-interval"
								className="net-field-number"
								label={t("settings.egress.refreshInterval")}
								error={
									sourceIntervalInvalid ? t("settings.egress.invalidRefreshInterval") : undefined
								}
							>
								<IntervalInput
									id="egress-source-refresh-interval"
									value={
										sourceForm.refreshIntervalSeconds
											? String(sourceForm.refreshIntervalSeconds)
											: ""
									}
									onChange={(value) =>
										setSourceForm({
											...sourceForm,
											refreshIntervalSeconds: Number(value) || 0,
										})
									}
								/>
							</OperationsField>
							<OperationsField
								controlId="source-proxy"
								label={t("settings.egress.subscriptionProxy")}
							>
								<Switch
									id="source-proxy"
									checked={sourceForm.proxyEnabled}
									onCheckedChange={(proxyEnabled) => setSourceForm({ ...sourceForm, proxyEnabled })}
								/>
							</OperationsField>
							{sourceForm.proxyEnabled ? (
								<OperationsField
									controlId="source-proxy-url"
									label={t("settings.egress.subscriptionProxyURL")}
									error={
										sourceProxyInvalid ? t("settings.egress.invalidSubscriptionProxy") : undefined
									}
								>
									<Input
										id="source-proxy-url"
										type="text"
										autoComplete="off"
										aria-invalid={sourceProxyInvalid}
										placeholder={
											sourceEditing?.proxyConfigured
												? t("ops.netKeepStoredAddress")
												: "http://proxy.example:8080"
										}
										value={sourceForm.proxyURL}
										onChange={(event) => {
											if (editor.current) editor.current.proxyEdited = true;
											setSourceForm({
												...sourceForm,
												proxyURL: event.target.value,
											});
										}}
									/>
								</OperationsField>
							) : null}
						</fieldset>
						<DialogFooter>
							<Button type="button" size="sm" variant="secondary" onClick={closeSource}>
								{t(saveSource.isPending ? "common.close" : "common.cancel")}
							</Button>
							<Button
								type="submit"
								size="sm"
								disabled={
									!sourceForm.name.trim() ||
									(!sourceEditing && !sourceForm.url.trim()) ||
									(sourceForm.proxyEnabled &&
										!sourceEditing?.proxyConfigured &&
										!sourceForm.proxyURL.trim()) ||
									sourceProxyInvalid ||
									sourceURLInvalid ||
									sourceIntervalInvalid ||
									saveSource.isPending
								}
							>
								{saveSource.isPending ? <Spinner /> : null}
								{t("common.save")}
							</Button>
						</DialogFooter>
					</form>
				</DialogContent>
			</Dialog>
		</section>
	);
}
