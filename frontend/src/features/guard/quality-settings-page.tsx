import {
	OperationsHeader,
	OperationsField as SettingsField,
	OperationsAlertDialogContent as AlertDialogContent,
	StatusPill,
	OperationsHelp,
} from "@/features/operations/operations-ui";
import { useMemo, useState } from "react";
import { Tabs, TabsContent } from "@/components/ui/tabs";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
	ArrowLeft,
	Gavel,
	RefreshCw,
	Search,
	ShieldCheck,
	Waypoints,
} from "lucide-react";
import { Controller } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { Link, useLocation, useNavigate } from "react-router-dom";

import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { OperationsButton as Button } from "@/features/operations/operations-ui";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { listModels } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import { QualitySection, QualityTabsList } from "@/features/guard/quality-ui";
import { toQualityGuardForm } from "@/features/guard/quality-guard-model";
import { useQualityGuardSettings } from "@/features/guard/use-quality-guard-settings";
import { DurationInput } from "@/features/settings/settings-ui";
import { SettingsApplicationStatus } from "@/features/settings/settings-status";
import { useSettings } from "@/features/settings/use-settings";
import { useLifetimeSignal } from "@/shared/hooks/use-lifetime-signal";
import {
	fetchQualitySettings,
	updateQualitySettings,
	type QualitySettings,
	type QualitySettingsInput,
} from "./quality-api";

// Guard scope and retry parameters share one guard draft and revision. Rotation
// edits use gateway settings; investigation tunables own a third versioned form.
// No save attempts to simulate a transaction across these independent domains.

type FieldKind = "duration" | "number";

const ALL_TUNABLE_FIELDS: Array<{ key: keyof QualitySettingsInput; kind: FieldKind }> = [
	{ key: "account_need_exits", kind: "number" },
	{ key: "account_span_nodes", kind: "number" },
	{ key: "exit_need_n", kind: "number" },
	{ key: "exit_need_k", kind: "number" },
	{ key: "differential_exits", kind: "number" },
	{ key: "jurors_per_exit", kind: "number" },
	{ key: "probe_budget", kind: "number" },
	{ key: "investigation_timeout", kind: "duration" },
	{ key: "retention", kind: "duration" },
	{ key: "evidence_window", kind: "duration" },
];

/** 可行性预校验(批10 后端同款约束的前置):
 * 门槛超出可派证据量会静默关闭定罪通道——保存前即拒,不走 400 往返。 */
function tunablesFeasibilityError(form: QualitySettings): string | null {
	if (form.exit_need_k > form.exit_need_n) {
		return "quality.settings.feasibility.kOverN";
	}
	if (form.account_span_nodes > form.account_need_exits) {
		return "quality.settings.feasibility.spanOverExits";
	}
	if (form.jurors_per_exit < form.exit_need_n) {
		return "quality.settings.feasibility.jurorsBelowWitnesses";
	}
	if (form.probe_budget < form.differential_exits + form.jurors_per_exit) {
		return "quality.settings.feasibility.budgetBelowCoreProbes";
	}
	return null;
}

const SETTINGS_VIEWS = [
	"jurisdiction",
	"retry",
	"rotation",
	"tunables",
] as const;
type SettingsView = (typeof SETTINGS_VIEWS)[number];

export function QualitySettingsPage() {
	const { t } = useTranslation();
 const { form, settingsQuery, updateMutation, resetRotationMutation, reset } = useSettings();
 const guard = useQualityGuardSettings();
 const guardForm = guard.form;
 const [resetDefaultsConfirm, setResetDefaultsConfirm] = useState<"guard" | "rotation" | null>(null);
	const location = useLocation(),
		navigate = useNavigate();
	const hash = location.hash.slice(1);
	const settingsView = (SETTINGS_VIEWS as readonly string[]).includes(hash)
		? (hash as SettingsView)
		: "jurisdiction";

 const fileGuard = guard.guardQuery.data?.file_defaults;
 const syncRequestRetryToFile = () => {
   if (!fileGuard) return;
   const defaults = toQualityGuardForm(fileGuard);
   for (const key of ["maxAttempts", "createdTimeout", "evidenceTimeout", "admissionTimeout", "toolAdmissionTimeout", "accountCooldown", "idleAccountCooldown"] as const) {
     guardForm.setValue(key, defaults[key], { shouldDirty: true, shouldTouch: true, shouldValidate: true });
   }
 };
 const loading = settingsQuery.isPending;
 const submitSettings = form.handleSubmit((values) => updateMutation.mutate(values));
 const submitGuard = guardForm.handleSubmit((values) => guard.saveMutation.mutate(values));
 const resetGuard = resetDefaultsConfirm === "guard";

	return (
		<div className="ops-workspace">
			<OperationsHeader
				title={t("ops.settings")}
				description={t("ops.qualitySettingsDescription")}
				action={
					<Button type="button" variant="outline" size="sm" asChild>
						<Link to="/guard">
							<ArrowLeft className="size-3.5" />
							{t("quality.settingsPage.backToConsole")}
						</Link>
					</Button>
				}
			/>
			{loading && settingsView === "rotation" ? (
				<div className="flex min-h-64 items-center justify-center">
					<Spinner />
				</div>
			) : null}

            {settingsView === "rotation" && settingsQuery.isError ? <p role="alert">{settingsQuery.error.message} <Button type="button" variant="outline" onClick={() => void settingsQuery.refetch()}>{t("common.retry")}</Button></p> : null}
            {settingsView === "rotation" && settingsQuery.data ? <SettingsApplicationStatus snapshot={settingsQuery.data} /> : null}
            {(settingsView === "jurisdiction" || settingsView === "retry") ? <div role="status" className="space-y-2">
              {guard.guardQuery.data ? <p>{t("quality.guardConfig.version", { revision: guard.guardQuery.data.revision })}</p> : <Spinner />}
              {guard.guardQuery.isError ? <p role="alert">{String(guard.guardQuery.error)} <Button type="button" variant="outline" onClick={() => void guard.guardQuery.refetch()}>{t("common.retry")}</Button></p> : null}
              {guard.saveMutation.isError || guard.resetMutation.isError ? <p role="alert">{String(guard.saveMutation.error ?? guard.resetMutation.error)}</p> : null}
              {guardForm.formState.errors.guardedModels ? <p role="alert">{t("quality.guardConfig.emptySelection")}</p> : null}
            </div> : null}
            <Tabs
					className="ops-settings-layout"
					activationMode="manual"
					value={settingsView}
					onValueChange={(next) => {
						navigate({ pathname: "/guard/settings", hash: next });
					}}
				>
					<QualityTabsList
						items={[
							{
								value: "jurisdiction",
								label: t("ops.settingsCoverage"),
								icon: ShieldCheck,
							},
							{
								value: "retry",
								label: t("ops.settingsRetry"),
								icon: RefreshCw,
							},
							{
								value: "rotation",
								label: t("ops.settingsRotation"),
								icon: Waypoints,
							},
							{
								value: "tunables",
								label: t("ops.settingsInvestigation"),
								icon: Gavel,
							},
						]}
                        end={settingsView === "jurisdiction" || settingsView === "retry" ? (
                          <div className="flex flex-wrap gap-2">
                            <Button type="button" variant="outline" size="sm" disabled={!guardForm.formState.isDirty || guard.saving} onClick={guard.reload}>{t("quality.guardConfig.reload")}</Button>
                            <Button type="button" variant="outline" size="sm" disabled={guard.saving || !fileGuard} onClick={() => setResetDefaultsConfirm("guard")}>{t("quality.guardConfig.reset")}</Button>
                          </div>
                        ) : settingsView === "rotation" ? (
                          <div className="flex flex-wrap gap-2">
                            <Button type="button" variant="outline" size="sm" disabled={!form.formState.isDirty || updateMutation.isPending} onClick={reset}>{t("quality.guardConfig.reload")}</Button>
                            <Button type="button" variant="outline" size="sm" disabled={loading || updateMutation.isPending || resetRotationMutation.isPending} onClick={() => setResetDefaultsConfirm("rotation")}>{t("quality.guardConfig.resetRotation")}</Button>
                          </div>
                        ) : undefined}
					/>
					<TabsContent
						forceMount
						value="jurisdiction"
						className="mt-0 min-w-0 data-[state=inactive]:hidden"
					>
                        <fieldset disabled={guard.saving} className="min-w-0"><QualityJurisdictionSection settings={guard} /></fieldset>
					</TabsContent>

					<TabsContent
						forceMount
						value="retry"
						className="mt-0 min-w-0 data-[state=inactive]:hidden"
					>
                        <form onSubmit={submitGuard}>
                        <fieldset disabled={guard.saving || !guard.guardQuery.data} className="min-w-0">
                        <QualitySection icon={RefreshCw} title={t("quality.settingsPage.retryTitle")} help={t("quality.settingsPage.retryHelp")}
                          action={<div className="flex items-center gap-2">
                            {fileGuard ? <Button type="button" variant="outline" size="sm" onClick={syncRequestRetryToFile}>{t("settings.requestRetry.syncFileButton")}</Button> : null}
                            <Button type="submit" size="sm" disabled={!guardForm.formState.isDirty}>{guard.saving ? <Spinner /> : null}{t("common.save")}</Button>
                          </div>}>
                          <div className="ops-form-grid">
                            {(["createdTimeout", "evidenceTimeout", "admissionTimeout", "toolAdmissionTimeout", "accountCooldown", "idleAccountCooldown"] as const).map((key) => (
                              <SettingsField key={key} controlId={`guard-${key}`} label={t(`settings.requestRetry.${key}`)} description={t(`settings.requestRetry.${key}Help`)} error={guardForm.formState.errors[key]?.message}>
                                <Controller control={guardForm.control} name={key} render={({ field }) => <DurationInput allowZero id={`guard-${key}`} value={field.value} onChange={field.onChange} />} />
                              </SettingsField>
                            ))}
                            <SettingsField controlId="request-retry-max-attempts" label={t("settings.requestRetry.maxAttempts")} description={t("settings.requestRetry.maxAttemptsHelp")} error={guardForm.formState.errors.maxAttempts?.message}>
                              <Input id="request-retry-max-attempts" type="number" min={1} max={100} {...guardForm.register("maxAttempts", { valueAsNumber: true })} />
                            </SettingsField>
                            <SettingsField controlId="request-retry-on-exhausted" label={t("settings.requestRetry.onExhausted")} description={t("settings.requestRetry.onExhaustedHelp")}><Badge>{t("settings.requestRetry.failClosed")}</Badge></SettingsField>
                          </div>
                        </QualitySection>
                        </fieldset>
                        </form>
					</TabsContent>

					<TabsContent
						forceMount
						value="rotation"
						className="mt-0 min-w-0 data-[state=inactive]:hidden"
					>
                        <form onSubmit={submitSettings}><fieldset disabled={loading || updateMutation.isPending || resetRotationMutation.isPending} className="min-w-0">
						<QualitySection
							icon={Waypoints}
							title={t("settings.egressRotation.title")}
							help={t("quality.settingsPage.rotationHelp")}
							action={
								<Button
									type="submit"
									size="sm"
									disabled={
										loading ||
										updateMutation.isPending ||
										!form.formState.isDirty
									}
								>
									{updateMutation.isPending ? <Spinner /> : null}
									{t("common.save")}
								</Button>
							}
						>
							<div className="ops-form-grid">
								<SettingsField
									controlId="egress-rotation-enabled"
									className="ops-field-wide"
									label={t("settings.egressRotation.enabled")}
									description={t("settings.egressRotation.enabledHelp")}
								>
									<Controller
										control={form.control}
										name="egressRotation.enabled"
										render={({ field }) => (
											<div className="flex h-8 items-center">
												<Switch
													id="egress-rotation-enabled"
													checked={field.value ?? false}
													onCheckedChange={field.onChange}
												/>
											</div>
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-min-interval"
									label={t("settings.egressRotation.minNodeInterval")}
									description={t("settings.egressRotation.minNodeIntervalHelp")}
									error={
										form.formState.errors.egressRotation?.minNodeInterval
											?.message
									}
								>
									<Controller
										control={form.control}
										name="egressRotation.minNodeInterval"
										render={({ field }) => (
											<DurationInput
                                                allowZero
												id="egress-rotation-min-interval"
												value={field.value}
												onChange={field.onChange}
											/>
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-max-attempts"
									label={t("settings.egressRotation.maxAttemptsPerQuarantine")}
									description={t(
										"settings.egressRotation.maxAttemptsPerQuarantineHelp",
									)}
									error={
										form.formState.errors.egressRotation
											?.maxAttemptsPerQuarantine?.message
									}
								>
									<Input
										id="egress-rotation-max-attempts"
										type="number"
										min={1}
										max={100}
										{...form.register(
											"egressRotation.maxAttemptsPerQuarantine",
											{ valueAsNumber: true },
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-settle-delay"
									label={t("settings.egressRotation.settleDelay")}
									description={t("settings.egressRotation.settleDelayHelp")}
									error={
										form.formState.errors.egressRotation?.settleDelay?.message
									}
								>
									<Controller
										control={form.control}
										name="egressRotation.settleDelay"
										render={({ field }) => (
											<DurationInput
                                                allowZero
												id="egress-rotation-settle-delay"
												value={field.value}
												onChange={field.onChange}
											/>
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-probe-timeout"
									label={t("settings.egressRotation.probeTimeout")}
									description={t("settings.egressRotation.probeTimeoutHelp")}
									error={
										form.formState.errors.egressRotation?.probeTimeout?.message
									}
								>
									<Controller
										control={form.control}
										name="egressRotation.probeTimeout"
										render={({ field }) => (
											<DurationInput
                                                allowZero
												id="egress-rotation-probe-timeout"
												value={field.value}
												onChange={field.onChange}
											/>
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-probe-interval"
									label={t("settings.egressRotation.probeInterval")}
									description={t("settings.egressRotation.probeIntervalHelp")}
									error={
										form.formState.errors.egressRotation?.probeInterval?.message
									}
								>
									<Controller
										control={form.control}
										name="egressRotation.probeInterval"
										render={({ field }) => (
											<DurationInput
                                                allowZero
												id="egress-rotation-probe-interval"
												value={field.value}
												onChange={field.onChange}
											/>
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-webhook-timeout"
									label={t("settings.egressRotation.webhookTimeout")}
									description={t("settings.egressRotation.webhookTimeoutHelp")}
									error={
										form.formState.errors.egressRotation?.webhookTimeout
											?.message
									}
								>
									<Controller
										control={form.control}
										name="egressRotation.webhookTimeout"
										render={({ field }) => (
											<DurationInput
                                                allowZero
												id="egress-rotation-webhook-timeout"
												value={field.value}
												onChange={field.onChange}
											/>
										)}
									/>
								</SettingsField>
								<SettingsField
									controlId="egress-rotation-webhook-retries"
									label={t("settings.egressRotation.webhookRetries")}
									description={t("settings.egressRotation.webhookRetriesHelp")}
									error={
										form.formState.errors.egressRotation?.webhookRetries
											?.message
									}
								>
									<Input
										id="egress-rotation-webhook-retries"
										type="number"
										min={0}
										max={10}
										{...form.register("egressRotation.webhookRetries", {
											valueAsNumber: true,
										})}
									/>
								</SettingsField>
							</div>
						</QualitySection>
                        </fieldset></form>
					</TabsContent>

					<TabsContent
						forceMount
						value="tunables"
						className="mt-0 min-w-0 data-[state=inactive]:hidden"
					>
						<QualityTunablesSection />
					</TabsContent>
				</Tabs>

			<AlertDialog
				open={resetDefaultsConfirm !== null}
				onOpenChange={(open) => { if (!open) setResetDefaultsConfirm(null); }}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							{t(resetGuard ? "quality.guardConfig.resetTitle" : "quality.guardConfig.resetRotationTitle")}
						</AlertDialogTitle>
						<AlertDialogDescription>
							{t(resetGuard ? "quality.guardConfig.resetDescription" : "quality.guardConfig.resetRotationDescription")}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							onClick={() => {
								if (resetGuard) guard.resetMutation.mutate();
                                else resetRotationMutation.mutate();
                                setResetDefaultsConfirm(null);
							}}
						>
							{t("settings.resetToDefaultsConfirm")}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</div>
	);
}

/** Guard scope, model selection and retry controls use the same policy draft. */
function QualityJurisdictionSection({ settings }: { settings: ReturnType<typeof useQualityGuardSettings> }) {
 const { t } = useTranslation();
 const { form, guardQuery, saveMutation } = settings;
 const masterEnabled = form.watch("enabled") ?? false;
 const watchedModels = form.watch("guardedModels");
 const selected = useMemo(() => new Set(watchedModels ?? guardQuery.data?.guarded_models ?? []), [watchedModels, guardQuery.data]);
 const loadedOnce = guardQuery.data !== undefined;
 const onMasterToggle = (next: boolean) => form.setValue("enabled", next, { shouldDirty: true, shouldTouch: true });
	const modelsQuery = useQuery({
		queryKey: ["quality", "guard", "models"],
		queryFn: ({ signal }) =>
			listModels({ page: 1, pageSize: 200, status: "enabled" }, signal),
	});

	// 候选=启用路由按 publicId 聚合(后端管辖按 publicId 判定:同名
	// 模型多渠道共用一个勾选键——一行多渠道徽章,如实呈现语义);
	// 已在管辖内但不在候选里的模型单独保留,避免保存时静默丢失。
	const candidateRoutes = useMemo(
		() => modelsQuery.data?.items ?? [],
		[modelsQuery.data],
	);
	const candidateIDs = useMemo(
		() => new Set(candidateRoutes.map((route) => route.publicId)),
		[candidateRoutes],
	);
	const staleIDs = useMemo(
		() =>
			[...selected]
				.filter((id) => !candidateIDs.has(id.split(":").pop() ?? id))
				.sort(),
		[selected, candidateIDs],
	);
	const [modelSearch, setModelSearch] = useState("");
	const [onlyEnabled, setOnlyEnabled] = useState(true);
	const modelEntries = useMemo(() => {
		const byID = new Map<string, ModelRouteDTO[]>();
		for (const route of candidateRoutes) {
			const list = byID.get(route.publicId) ?? [];
			list.push(route);
			byID.set(route.publicId, list);
		}
		const query = modelSearch.trim().toLowerCase();
		return [...byID.entries()]
			.map(([publicId, routes]) => ({
				publicId,
				routes: routes.sort((a, b) => a.provider.localeCompare(b.provider)),
				upstream:
					routes.find(
						(route) => route.upstreamModel && route.upstreamModel !== publicId,
					)?.upstreamModel ?? "",
			}))
			.filter(
				(entry) =>
					!query ||
					entry.publicId.toLowerCase().includes(query) ||
					entry.upstream.toLowerCase().includes(query),
			)
			.sort((a, b) => a.publicId.localeCompare(b.publicId));
	}, [candidateRoutes, modelSearch]);

	if (guardQuery.isError && !guardQuery.data) {
		return (
			<p role="alert" className="text-sm text-destructive">
				{String(guardQuery.error)}
			</p>
		);
	}
	if (guardQuery.isLoading || !loadedOnce) {
		return (
			<div className="flex min-h-16 items-center justify-center">
				<Spinner />
			</div>
		);
	}

	const selfCheck = guardQuery.data?.self_check;
	const selfCheckOk = selfCheck?.outcome === "ok";
	const emptySelection = masterEnabled && selected.size === 0;
 const dirty = form.formState.isDirty;
 const editSelected = (mutate: (next: Set<string>, base: Set<string>) => void) => {
   const base = new Set(form.getValues("guardedModels") ?? []);
   const next = new Set(base);
   mutate(next, base);
   form.setValue("guardedModels", [...next], { shouldDirty: true, shouldTouch: true });
 };

	const toggleSelected = (id: string, checked: boolean) => {
		editSelected((next) => {
			if (checked) {
				next.add(id);
			} else {
				next.delete(id);
			}
		});
	};

	// 渠道级切换:条目="渠道:公开名"(限定渠道)或裸公开名(通配=全部渠道)。
	// 通配态下点某个渠道=收窄(其余渠道转为显式条目);再点=恢复。
	const toggleChannel = (
		publicId: string,
		provider: string,
		allProviders: string[],
		active: boolean,
	) => {
		editSelected((next, base) => {
			if (base.has(publicId)) {
				next.delete(publicId);
				for (const other of allProviders) {
					if (other !== provider) {
						next.add(other + ":" + publicId);
					}
				}
				return;
			}
			const scoped = provider + ":" + publicId;
			if (active) {
				next.delete(scoped);
			} else {
				next.add(scoped);
			}
		});
	};

	// 行级全开/全关:勾=该标识全部可用渠道显式开启;取消=清掉通配与全部渠道条目。
	const toggleRowAll = (
		publicId: string,
		allProviders: string[],
		rowActive: boolean,
	) => {
		editSelected((next) => {
			if (!rowActive) {
				for (const provider of allProviders) {
					next.add(provider + ":" + publicId);
				}
				return;
			}
			next.delete(publicId);
			for (const provider of allProviders) {
				next.delete(provider + ":" + publicId);
			}
		});
	};

	return (
		<QualitySection
			icon={ShieldCheck}
			title={t("ops.settingsCoverage")}
			help={t("quality.settingsPage.jurisdictionHelp")}
			action={
				<Button
					type="button"
					size="sm"
					disabled={!guardQuery.data || !dirty || emptySelection || saveMutation.isPending}
                    onClick={form.handleSubmit((values) => saveMutation.mutate(values))}
				>
					{saveMutation.isPending ? <Spinner /> : null}
					{t("common.save")}
				</Button>
			}
		>
			<div className="space-y-5">
				{/* 总开关独立区块。 */}
				<div className="flex flex-wrap items-center justify-between gap-3 rounded-md border bg-muted/20 px-4 py-3.5">
					<div className="flex items-center gap-1">
						<label
							className="flex items-center gap-2.5 text-sm font-medium"
						>
							<Switch
								checked={masterEnabled}
								onCheckedChange={onMasterToggle}
								aria-label={t("quality.settingsPage.masterEnabled")}
							/>
							{t("quality.settingsPage.masterEnabled")}
						</label>
						<OperationsHelp label={t("quality.settingsPage.masterEnabled")}>
							{t("quality.settingsPage.masterEnabledHelp")}
						</OperationsHelp>
					</div>
					{selfCheck ? (
						<StatusPill tone={selfCheckOk ? "good" : "bad"}>
							{selfCheckOk
								? t("quality.guardConfig.selfCheckOk")
								: t("quality.guardConfig.selfCheckError", {
										detail: selfCheck.detail ?? "",
									})}
						</StatusPill>
					) : null}
				</div>
				{emptySelection ? (
					<p className="-mb-2 text-xs text-destructive">
						{t("quality.guardConfig.emptySelection")}
					</p>
				) : null}
				{saveMutation.isError ? (
					<p className="-mb-2 text-xs text-destructive">
						{String(saveMutation.error)}
					</p>
				) : null}
				{modelsQuery.isLoading ? (
					<div className="flex min-h-16 items-center">
						<Spinner />
					</div>
				) : (
					<div className="space-y-3">
						<div className="flex flex-wrap items-center gap-2">
							<span className="text-xs font-medium">
								{t("quality.guardConfig.title")}
							</span>
							<span className="font-mono text-[11px] tabular-nums text-muted-foreground">
								{t("quality.guardConfig.modelsCount", {
									selected: new Set(
										[...selected].map(
											(entry) => entry.split(":").pop() ?? entry,
										),
									).size,
									total: candidateIDs.size + staleIDs.length,
								})}
							</span>

							<div className="relative w-full sm:ml-auto sm:w-56">
								<Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
								<Input
									className="h-8 pl-8 text-xs"
									value={modelSearch}
									onChange={(event) => setModelSearch(event.target.value)}
									placeholder={t("quality.guardConfig.searchPlaceholder")}
									aria-label={t("quality.guardConfig.searchPlaceholder")}
								/>
							</div>
						</div>
						<div className="flex items-center justify-between gap-3">
							<label className="flex items-center gap-2 text-xs text-muted-foreground">
								<Checkbox
									checked={onlyEnabled}
									onCheckedChange={(v) => setOnlyEnabled(v === true)}
								/>
								{t("ops.activeModelsOnly")}
							</label>
							<OperationsHelp>{t("ops.modelChannelHelp")}</OperationsHelp>
						</div>
						<div className="max-h-[520px] overflow-auto">
							<table className="ops-model-table">
								<thead>
									<tr>
										<th>{t("ops.modelColumn")}</th>
										{["Build", "Web", "Console"].map((label) => (
											<th key={label}>{label}</th>
										))}
									</tr>
								</thead>
								<tbody>
									{modelEntries
										.filter(
											(entry) =>
												!onlyEnabled ||
												selected.has(entry.publicId) ||
												entry.routes.some((route) =>
													selected.has(route.provider + ":" + entry.publicId),
												),
										)
										.map((entry) => {
											const providers: string[] = [
												...new Set(entry.routes.map((route) => route.provider)),
											];
											const wildcard = selected.has(entry.publicId),
												activeProviders = new Set(
													providers.filter((provider) =>
														selected.has(provider + ":" + entry.publicId),
													),
												);
											const rowActive =
												wildcard ||
												(providers.length > 0 &&
													providers.every((provider) =>
														activeProviders.has(provider),
													));
											return (
												<tr key={entry.publicId}>
													<td>
														<label className="flex min-w-36 items-center gap-2.5">
															<Checkbox
																checked={
																	rowActive
																		? true
																		: activeProviders.size > 0
																			? "indeterminate"
																			: false
																}
																onCheckedChange={() =>
																	toggleRowAll(
																		entry.publicId,
																		providers,
																		rowActive,
																	)
																}
																aria-label={entry.publicId}
															/>
															<span className="text-xs font-medium">
																{entry.publicId}
															</span>
														</label>
													</td>
													{["grok_build", "grok_web", "grok_console"].map(
														(provider) => (
															<td key={provider}>
																{providers.includes(provider) ? (
																	<button
																		type="button"
																		className="ops-model-toggle"
																		aria-pressed={
																			wildcard || activeProviders.has(provider)
																		}
																		aria-label={provider + " " + entry.publicId}
																		onClick={() =>
																			toggleChannel(
																				entry.publicId,
																				provider,
																				providers,
																				wildcard ||
																					activeProviders.has(provider),
																			)
																		}
																	>
																		{t(
																			wildcard || activeProviders.has(provider)
																				? "ops.modelProtected"
																				: "ops.modelNotProtected",
																		)}
																	</button>
																) : (
																	<span className="text-muted-foreground">
																		—
																	</span>
																)}
															</td>
														),
													)}
												</tr>
											);
										})}
								</tbody>
							</table>
						</div>

						{modelEntries.length === 0 ? (
							<p className="rounded-md border border-dashed px-4 py-6 text-center text-sm text-muted-foreground">
								{t("quality.guardConfig.noMatch")}
							</p>
						) : null}
						{staleIDs.length > 0 ? (
							<div className="space-y-1.5 pt-1">
								<div className="px-1 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground/70">
									{t("quality.guardConfig.staleGroup")}
								</div>
								<div className="grid gap-1.5 sm:grid-cols-2">
									{staleIDs.map((id) => (
										<label
											key={id}
											className="flex items-center gap-2 rounded-md border border-dashed px-3 py-2 text-sm"
										>
											<Checkbox
												checked={selected.has(id)}
												onCheckedChange={(checked) =>
													toggleSelected(id, checked === true)
												}
											/>
											<span className="font-mono text-xs text-muted-foreground">
												{id}
											</span>
										</label>
									))}
								</div>
							</div>
						) : null}
					</div>
				)}
			</div>
		</QualitySection>
	);
}

/** 仲裁庭与证据局参数：独立保存，整体热应用。 */
function QualityTunablesSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const lifetimeSignal = useLifetimeSignal();

	const settingsQuery = useQuery({
		queryKey: ["quality", "settings"],
		queryFn: ({ signal }) => fetchQualitySettings(signal),
		refetchInterval: (query) => query.state.data?.apply_pending ? 3000 : false,
	});

	// Freeze the editing base and its revision so background refresh cannot
	// silently turn a stale edit into a write against a newer version.
	const [draft, setDraft] = useState<{ base: QualitySettings; edits: Partial<QualitySettingsInput> } | null>(null);
	const [savedNote, setSavedNote] = useState<string | null>(null);
	const base = draft?.base ?? settingsQuery.data;
	const edits = draft?.edits ?? {};
	const form = base ? { ...base, ...edits } : null;
	const feasibility = form ? tunablesFeasibilityError(form) : null;

	const saveMutation = useMutation({
		mutationFn: async (input: QualitySettings) => {
			const signal = lifetimeSignal();
			await queryClient.cancelQueries({ queryKey: ["quality", "settings"], exact: true });
			signal.throwIfAborted();
			return updateQualitySettings(input, signal);
		},
		onSuccess: async (applied) => {
			const signal = lifetimeSignal();
			if (signal.aborted) return;
			await queryClient.cancelQueries({ queryKey: ["quality", "settings"], exact: true });
			if (signal.aborted) return;
			setDraft(null);
			setSavedNote(applied.apply_pending ? null : t("quality.settings.savedNote"));
			queryClient.setQueryData(["quality", "settings"], applied);
			void queryClient.invalidateQueries({ queryKey: ["quality", "settings"] });
		},
		onError: () => { if (!lifetimeSignal().aborted) void queryClient.invalidateQueries({ queryKey: ["quality", "settings"] }); },
	});

	if (settingsQuery.isError) {
		return (
			<p role="alert" className="text-sm text-destructive">
				{String(settingsQuery.error)}
			</p>
		);
	}
	if (settingsQuery.isLoading || !form || !base) {
		return (
			<div className="flex min-h-16 items-center justify-center">
				<Spinner />
			</div>
		);
	}

	const dirty = ALL_TUNABLE_FIELDS.some(
		(field) =>
			edits[field.key] !== undefined && edits[field.key] !== base?.[field.key],
	);

	const setField = (
		key: keyof QualitySettingsInput,
		raw: string,
		kind: FieldKind,
	) => {
		setDraft((prev) => {
   const next = prev ?? { base, edits: {} };
   const parsed = Number(raw);
   const value = kind === "number" ? (Number.isFinite(parsed) ? parsed : 0) : raw;
   return { base: next.base, edits: { ...next.edits, [key]: value } };
  });
		setSavedNote(null);
	};

	return (
		<QualitySection
			icon={Gavel}
			title={t("ops.settingsInvestigation")}
			help={`${t("quality.settings.help")} ${t("quality.settings.capacityProjection", { count: settingsQuery.data?.max_rotations_per_hour })}`}
			action={
				<Button
					type="button"
					size="sm"
					disabled={!dirty || saveMutation.isPending || feasibility !== null}
					onClick={() => saveMutation.mutate(form)}
				>
					{saveMutation.isPending ? <Spinner /> : null}
					{t("common.save")}
				</Button>
			}
		>
			{feasibility ? (
				<p className="text-xs text-destructive">
					{t(feasibility, {
						k: form.exit_need_k,
						n: form.exit_need_n,
						span: form.account_span_nodes,
						exits: form.account_need_exits,
						jurors: form.jurors_per_exit,
						budget: form.probe_budget,
						differential: form.differential_exits,
					})}
				</p>
			) : null}
			{saveMutation.isError ? (
				<p className="text-xs text-destructive">{String(saveMutation.error)}</p>
			) : null}
			{savedNote ? (
				<p className="text-xs text-emerald-600 dark:text-emerald-400">
					{savedNote}
				</p>
			) : null}
   {settingsQuery.data?.apply_pending ? (
    <p role="status" className="text-sm text-amber-600">
     {t("quality.settings.pendingNote", { saved: settingsQuery.data.revision, applied: settingsQuery.data.applied_revision })}
     {settingsQuery.data.apply_error ? ` ${settingsQuery.data.apply_error}` : ""}
    </p>
   ) : null}
   {draft && settingsQuery.data?.revision !== draft.base.revision ? (
    <p role="alert" className="text-sm text-amber-600">{t("quality.settings.changedNote")}</p>
   ) : null}
   {draft ? (
    <Button type="button" variant="outline" size="sm" disabled={saveMutation.isPending}
     onClick={() => { setDraft(null); setSavedNote(null); saveMutation.reset(); }}>
     {t("common.cancel")}
    </Button>
   ) : null}
			<div className="ops-form-grid">
				{ALL_TUNABLE_FIELDS.map((field) => (
					<SettingsField
						controlId={`quality-tunable-${String(field.key)}`}
						key={field.key}
						label={t(`quality.settings.fields.${String(field.key)}`)}
						description={t(`quality.settings.helps.${String(field.key)}`)}
						current={
							form[field.key] !== base[field.key]
								? String(base[field.key])
								: undefined
						}
					>
						<Input
							disabled={saveMutation.isPending}
							id={`quality-tunable-${String(field.key)}`}
							type={field.kind === "number" ? "number" : "text"}
							min={
								field.kind === "number"
									? [
											"account_need_exits",
											"account_span_nodes",
											"exit_need_n",
											"exit_need_k",
										].includes(field.key)
										? 2
										: 1
									: undefined
							}
							value={String(form[field.key])}
							onChange={(event) =>
								setField(field.key, event.target.value, field.kind)
							}
						/>
					</SettingsField>
				))}
			</div>
		</QualitySection>
	);
}
