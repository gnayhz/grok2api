import { Button } from "@/shared/ui/button";
import { Label } from "@/shared/ui/label";
import { useRef, useState, type ComponentProps } from "react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/shared/ui/tooltip";
import {
	OperationsHelp,
	OperationsTooltip,
	type OperationsField,
} from "@/shared/ui/operations";
import { DialogContent, DialogHeader, DialogFooter } from "@/shared/ui/dialog";

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
