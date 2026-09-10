import type { ComponentType, ReactNode } from "react";
import {
	OperationsSection,
	OperationsTabs,
	StatusPill,
} from "@/features/operations/operations-ui";
export function QualitySection({
	title,
	help,
	action,
	children,
	id,
}: {
	icon?: ComponentType<{ className?: string }>;
	title: string;
	help?: string;
	action?: ReactNode;
	children: ReactNode;
	id?: string;
}) {
	return (
		<div id={id}>
			<OperationsSection title={title} description={help} action={action}>
				{children}
			</OperationsSection>
		</div>
	);
}
export type Tone = "destructive" | "warning" | "ok" | "muted";

export function ToneBadge({
	tone,
	children,
	className,
}: {
	tone: Tone;
	children: ReactNode;
	className?: string;
}) {
	return (
		<span className={className}>
			<StatusPill
				tone={
					tone === "destructive"
						? "bad"
						: tone === "warning"
							? "warn"
							: tone === "ok"
								? "good"
								: "neutral"
				}
			>
				{children}
			</StatusPill>
		</span>
	);
}

export type QualityTabItem = {
	value: string;
	label: string;
	icon?: ComponentType<{ className?: string }>;
};

/** 吸顶 tab 列(TabsList 同款分段控件):图标+文案,tab 恒居中,行首/行尾可选动作区。 */
export function QualityTabsList({
	items,
	start,
	end,
}: {
	items: QualityTabItem[];
	start?: ReactNode;
	end?: ReactNode;
}) {
	return (
		<>
			{start}
			<OperationsTabs items={items} end={end} />
		</>
	);
}
