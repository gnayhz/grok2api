import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Input } from "@/shared/ui/input";
import { OperationsHelp } from "@/shared/ui/operations";
import {
	getEgressOperationsConfig,
	updateEgressOperationsConfig,
} from "@/entities/egress/egress-api";
import {
	EgressOperationsContext,
	operationsFormFrom,
	type EgressOperationsDraft,
	type EgressOperationsValue,
} from "./operations-shared";
import { showErrorToast } from "@/shared/lib/show-error";

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
	const query = useQuery({
		queryKey: ["egress-operations"],
		queryFn: ({ signal }) => getEgressOperationsConfig(signal),
	});
	const baseline = useMemo(() => operationsFormFrom(query.data), [query.data]);
	const form = draft ?? baseline;
	const [saving, setSaving] = useState(false);
	const saveOwner = useRef<AbortController | null>(null);
	useEffect(() => {
		const owner = new AbortController();
		saveOwner.current = owner;
		return () => owner.abort();
	}, []);

	const save = useCallback(async (): Promise<boolean> => {
		const owner = saveOwner.current;
		if (!owner || owner.signal.aborted) return false;
		const submitted = form;
		setSaving(true);
		try {
			await queryClient.cancelQueries({ queryKey: ["egress-operations"], exact: true });
			const saved = await updateEgressOperationsConfig(submitted, owner.signal);
			// The response acknowledges this submitted draft. A newer local edit
			// remains dirty, and an older GET cannot replace the accepted value.
			await queryClient.cancelQueries({ queryKey: ["egress-operations"], exact: true });
			if (owner.signal.aborted) return false;
			queryClient.setQueryData(["egress-operations"], saved);
			setDraft((current) => (current === submitted ? null : current));
			void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
			toast.success(t("proxies.routing.saved"));
			return true;
		} catch (error) {
			if (!owner.signal.aborted) showErrorToast(error, t);
			return false;
		} finally {
			setSaving(false);
		}
	}, [form, queryClient, t]);
	const update = useCallback<EgressOperationsValue["update"]>(
		(updater) => {
			setDraft((current) => updater(current ?? operationsFormFrom(query.data)));
		},
		[query.data],
	);
	const refetch = query.refetch;

	const value = useMemo<EgressOperationsValue>(
		() => ({
			form,
			isPending: query.isPending,
			isError: query.isError,
			errorMessage: query.error instanceof Error ? query.error.message : undefined,
			isDirty: draft !== null,
			update,
			save,
			savePending: saving,
			discard: () => setDraft(null),
			retry: () => void refetch(),
		}),
		[form, draft, saving, query.isPending, query.isError, query.error, refetch, save, update],
	);

	return (
		<EgressOperationsContext.Provider value={value}>{children}</EgressOperationsContext.Provider>
	);
}

export function OperationSectionHeader({
	title,
	help,
	children,
}: {
	title: string;
	help?: string;
	children?: ReactNode;
}) {
	return (
		<div className="flex min-h-8 flex-wrap items-center justify-between gap-3 px-1">
			<div className="flex items-center gap-1.5">
				<h3 className="text-sm font-medium tracking-tight">{title}</h3>
				{help && <OperationsHelp label={title}>{help}</OperationsHelp>}
			</div>
			{children ? <div className="flex flex-wrap items-center gap-1.5">{children}</div> : null}
		</div>
	);
}

/** 单位不单独占位：标签已写明（秒），单位块只会在数字和控件边缘之间留大片空白。
 * 受控值必须是 string：number 型受控值在清空输入框时会把空串强转回 0，
 * 框里永远留着删不掉的 "0"；这里保持用户敲的原文，失焦时才解析。 */
export function IntervalInput({
	id,
	value,
	onChange,
}: {
	id: string;
	value: string;
	onChange: (value: string) => void;
}) {
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
			onBlur={(event) => {
				const parsed = Number(event.target.value);
				if (Number.isFinite(parsed) && event.target.value.trim() !== "") onChange(String(parsed));
			}}
			onChange={(event) => onChange(event.target.value)}
		/>
	);
}
