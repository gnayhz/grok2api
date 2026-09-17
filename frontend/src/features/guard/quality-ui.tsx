import type { ComponentType, ReactNode } from "react";
import {
	OperationsSection,
	OperationsTabs,
} from "@/shared/ui/operations";
export function QualitySection({
	title,
	help,
	action,
	children,
	id,
	icon,
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
			<OperationsSection title={title} description={help} action={action} icon={icon}>
				{children}
			</OperationsSection>
		</div>
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
