import { cn } from "@/shared/lib/cn";
import {
	qualityAccountDisplay,
	qualityExitDisplay,
	type QualityAccountIdentity,
	type QualityExitIPIndex,
	type QualityNodeIdentity,
} from "./quality-view";

type IdentityProps = {
	className?: string;
};

/** 质量案件中的账号引用：名称为主，邮箱/稳定 ID 为辅，避免只剩内部编号。 */
export function QualityAccountReference({
	id,
	accounts,
	className,
}: IdentityProps & {
	id: number;
	accounts: Map<number, QualityAccountIdentity>;
}) {
	const display = qualityAccountDisplay(id, accounts.get(id));
	return (
		<span
			className={cn(
				"inline-flex min-w-0 max-w-full flex-col align-middle leading-tight",
				className,
			)}
			title={display.title}
		>
			<span className="truncate font-medium">{display.primary}</span>
			{display.secondary ? (
				<span className="truncate font-mono text-[10px] text-muted-foreground">
					{display.secondary}
				</span>
			) : null}
		</span>
	);
}

/** 质量案件中的出口引用：节点名称为主，node@epoch 与实际 IP 为辅。 */
export function QualityExitReference({
	node,
	epoch,
	nodes,
	ipByNode,
	className,
}: IdentityProps & {
	node: number;
	epoch: number;
	nodes: Map<number, QualityNodeIdentity>;
	ipByNode?: QualityExitIPIndex;
}) {
	const display = qualityExitDisplay(node, epoch, nodes.get(node), ipByNode);
	return (
		<span
			className={cn(
				"inline-flex min-w-0 max-w-full flex-col align-middle leading-tight",
				className,
			)}
			title={display.title}
		>
			<span className="truncate font-medium">{display.primary}</span>
			<span className="truncate font-mono text-[10px] text-muted-foreground">
				{display.secondary}
			</span>
		</span>
	);
}
