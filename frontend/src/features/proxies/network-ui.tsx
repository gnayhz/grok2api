import { TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import type { LucideIcon } from "lucide-react";
import { useRef, useState, type ComponentProps } from "react";
import { CircleAlert } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
	OperationsHelp,
	OperationsTooltip,
	type OperationsField,
} from "@/features/operations/operations-ui";
import { DialogContent, DialogHeader, DialogFooter } from "@/components/ui/dialog";

import { cn } from "@/shared/lib/cn";
import "./network.css";

/** Supplementary information uses the same surface as OperationsHelp. */
export function NetworkTooltip(props: ComponentProps<typeof OperationsTooltip>) {
	return <OperationsTooltip {...props} />;
}

export function NetworkButton({ title, ...props }: ComponentProps<typeof Button>) {
	const hint = title ?? (props.size === "icon" ? props["aria-label"] : undefined);
	const button = <Button {...props} />;
	return hint ? <NetworkTooltip content={hint}>{button}</NetworkTooltip> : button;
}

/** Only truncated names need a second reading surface. Measure on interaction,
 * avoiding a resize observer for every virtualized row. */
export function NetworkText({ children, className }: { children: string; className?: string }) {
	const ref = useRef<HTMLSpanElement>(null);
	const [open, setOpen] = useState(false);
	return (
		<Tooltip
			open={open}
			onOpenChange={(next) =>
				setOpen(next && Boolean(ref.current && ref.current.scrollWidth > ref.current.clientWidth))
			}
		>
			<TooltipTrigger asChild>
				<span ref={ref} tabIndex={0} className={cn("block min-w-0 truncate", className)}>
					{children}
				</span>
			</TooltipTrigger>
			<TooltipContent className="max-w-80 whitespace-normal break-words text-xs leading-5">
				{children}
			</TooltipContent>
		</Tooltip>
	);
}

export function NetworkError({ message }: { message: string }) {
	return (
		<NetworkTooltip content={message} tapToOpen>
			<button
				type="button"
				className="inline-flex shrink-0 cursor-help text-destructive"
				aria-label={message}
			>
				<CircleAlert className="size-3.5" />
			</button>
		</NetworkTooltip>
	);
}

export function NetworkNavigation({
	items,
}: {
	items: { value: string; label: string; icon: LucideIcon; count?: number; onClick?: () => void }[];
}) {
	return (
		<TabsList className="network-navigation" showIndicator={false}>
			{items.map(({ value, label, icon: Icon, count, onClick }) => (
				<TabsTrigger key={value} value={value} onClick={onClick} className="gap-1.5">
					<Icon className="size-3.5" aria-hidden="true" />
					<span>{label}</span>
					{count !== undefined && (
						<span className="ml-1 text-[10px] tabular-nums text-muted-foreground">{count}</span>
					)}
				</TabsTrigger>
			))}
		</TabsList>
	);
}

export function NetworkDialogContent({
	layout = "dialog",
	className,
	overlayClassName,
	...props
}: ComponentProps<typeof DialogContent> & { layout?: "editor" | "compact" | "dialog" }) {
	return (
		<DialogContent
			{...props}
			className={cn(
				"net-dialog",
				layout === "editor" &&
					"flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 sm:max-w-[560px]",
				className,
			)}
			overlayClassName={overlayClassName}
		/>
	);
}

export function NetworkDialogHeader({ className, ...props }: ComponentProps<typeof DialogHeader>) {
	return <DialogHeader {...props} className={cn("net-dialog-header pr-8", className)} />;
}

export function NetworkDialogFooter({ className, ...props }: ComponentProps<typeof DialogFooter>) {
	return <DialogFooter {...props} className={cn("net-dialog-footer gap-2", className)} />;
}

/** Form fields share the same controls and labels as the other management pages. */
export function NetworkField({
	controlId,
	label,
	description,
	error,
	className,
	children,
}: ComponentProps<typeof OperationsField>) {
	return (
		<div className={cn("space-y-2", className)}>
			<div className="flex items-center gap-1">
				<Label htmlFor={controlId}>{label}</Label>
				{description && <OperationsHelp label={label}>{description}</OperationsHelp>}
			</div>
			<div>{children}</div>
			{error && (
				<p role="alert" className="text-xs text-destructive">
					{error}
				</p>
			)}
		</div>
	);
}
export function NetworkSelect<T extends string>({
	id,
	label,
	value,
	options,
	onChange,
	disabled = false,
}: {
	id: string;
	label: string;
	value: T;
	options: { value: T; label: string; icon?: LucideIcon; disabled?: boolean }[];
	onChange: (value: T) => void;
	disabled?: boolean;
	columns?: 2 | 3;
}) {
	return (
		<Select value={value} onValueChange={(next) => onChange(next as T)} disabled={disabled}>
			<SelectTrigger id={id} aria-label={label}>
				<SelectValue />
			</SelectTrigger>
			<SelectContent>
				{options.map((option) => (
					<SelectItem key={option.value} value={option.value} disabled={option.disabled}>
						{option.label}
					</SelectItem>
				))}
			</SelectContent>
		</Select>
	);
}
