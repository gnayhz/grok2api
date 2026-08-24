import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleHelp } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
	getEgressOperationsConfig,
	updateEgressOperationsConfig,
} from "@/features/settings/settings-api";
import { EgressOperationsContext, showError, operationsFormFrom, type EgressOperationsDraft, type EgressOperationsValue } from "@/features/proxies/operations-shared";

/**
 * Shared draft state for the unified routing configuration (总出口 / 作用域
 * 出口 / 语义路由 + 检测设置). The proxies page owns one draft so every tab
 * edits the same object and a single sticky save button in the page header
 * commits it — separate save paths over one payload invite "saved one tab,
 * lost the other".
 *
 * 非组件共享面(枚举/键表/谓词/toast/hook)在 operations-shared.ts——组件
 * 文件只导出组件,保住 react-refresh 的 HMR 边界(auth-state.ts 同款切分)。
 */

export function EgressOperationsProvider({ children }: { children: ReactNode }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [draft, setDraft] = useState<EgressOperationsDraft | null>(null);
	const query = useQuery({ queryKey: ["egress-operations"], queryFn: getEgressOperationsConfig });
	const form = draft ?? operationsFormFrom(query.data);
	const [saving, setSaving] = useState(false);

	async function save(): Promise<boolean> {
		setSaving(true);
		try {
			await updateEgressOperationsConfig(form);
			setDraft(null);
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			void queryClient.invalidateQueries({ queryKey: ["egress-operations"] });
			toast.success(t("proxies.routing.saved"));
			return true;
		} catch (error) {
			showError(error);
			return false;
		} finally {
			setSaving(false);
		}
	}

	const value = useMemo<EgressOperationsValue>(() => ({
		form,
		isPending: query.isPending,
		isError: query.isError,
		errorMessage: query.error instanceof Error ? query.error.message : undefined,
		isDirty: draft !== null,
		update: (updater) => setDraft(updater(form)),
		save,
		savePending: saving,
		discard: () => setDraft(null),
		retry: () => void query.refetch(),
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}), [form, draft, saving, query.isPending, query.isError, query.error]);

	return <EgressOperationsContext.Provider value={value}>{children}</EgressOperationsContext.Provider>;
}

export function OperationSectionHeader({ title, help, children }: { title: string; help?: string; children?: ReactNode }) {
	return (
		<div className="flex min-h-8 flex-wrap items-center justify-between gap-3 px-1">
			<div className="flex items-center gap-1.5">
				<h3 className="text-sm font-medium tracking-tight">{title}</h3>
				{help ? (
					<Tooltip>
						<TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={help}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
						<TooltipContent className="max-w-80">{help}</TooltipContent>
					</Tooltip>
				) : null}
			</div>
			{children ? <div className="flex flex-wrap items-center gap-1.5">{children}</div> : null}
		</div>
	);
}

/** 单位不单独占位：标签已写明（秒），单位块只会在数字和控件边缘之间留大片空白。
 * 受控值必须是 string：number 型受控值在清空输入框时会把空串强转回 0，
 * 框里永远留着删不掉的 "0"；这里保持用户敲的原文，失焦时才解析。 */
export function IntervalInput({ id, value, onChange }: { id: string; value: string; onChange: (value: string) => void }) {
	return (
		<Input
			id={id}
			className="text-left tabular-nums [appearance:textfield] [-moz-appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none"
			type="number"
			inputMode="numeric"
			min={60}
			max={86400}
			value={value}
			onFocus={(event) => event.target.select()}
			onBlur={(event) => { const parsed = Number(event.target.value); if (Number.isFinite(parsed) && event.target.value.trim() !== "") onChange(String(parsed)); }}
			onChange={(event) => onChange(event.target.value)}
		/>
	);
}

export function SourceError({ message }: { message: string }) {
	return (
		<Tooltip>
			<TooltipTrigger asChild><span className="inline-flex shrink-0 cursor-help text-destructive" tabIndex={0} aria-label={message}><CircleAlert className="size-3.5" /></span></TooltipTrigger>
			<TooltipContent className="max-w-80">{message}</TooltipContent>
		</Tooltip>
	);
}

export function Control({ label, children }: { label: string; children: ReactNode }) {
	return <div className="space-y-2"><Label className="text-xs font-medium">{label}</Label>{children}</div>;
}

export function ToggleControl({ label, checked, onChange }: { label: string; checked: boolean; onChange: (value: boolean) => void }) {
	return <div className="flex min-h-10 items-center justify-between gap-4 rounded-md bg-muted/45 px-3"><Label className="text-xs font-medium">{label}</Label><Switch checked={checked} onCheckedChange={onChange} /></div>;
}