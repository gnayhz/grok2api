import { ArrowUpRight, CircleAlert, CircleHelp, RefreshCw } from "lucide-react";
import { useRef, useState, type ComponentProps, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import * as TabsPrimitive from "@radix-ui/react-tabs";
import { Button } from "@/components/ui/button";
import { DialogContent } from "@/components/ui/dialog";
import { AlertDialogContent } from "@/components/ui/alert-dialog";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/shared/lib/cn";
import "./operations.css";

export type StatusTone = "good" | "warn" | "bad" | "neutral";
export function StatusPill({
	tone = "neutral",
	children,
}: {
	tone?: StatusTone;
	children: ReactNode;
}) {
	return (
		<span className={cn("ops-status", `ops-status-${tone}`)}>
			<span className="ops-status-dot" />
			{children}
		</span>
	);
}
export function OperationsTooltip({
	children,
	content,
	tapToOpen = false,
}: {
	children: ReactNode;
	content: ReactNode;
	tapToOpen?: boolean;
}) {
	const [open, setOpen] = useState(false);
	const pointer = useRef("");
	return (
		<Tooltip open={open} onOpenChange={setOpen}>
			<TooltipTrigger
				asChild
				onPointerDown={(event) => {
					pointer.current = event.pointerType;
				}}
				onClick={() => {
					if (tapToOpen && pointer.current !== "mouse") setOpen(true);
				}}
			>
				{children}
			</TooltipTrigger>
			<TooltipContent className="max-w-80 whitespace-normal break-words text-xs leading-5">
				{content}
			</TooltipContent>
		</Tooltip>
	);
}

export function OperationsHelp({ children, label }: { children: ReactNode; label?: string }) {
	const { t } = useTranslation();
	return (
		<OperationsTooltip content={children} tapToOpen>
			<button
				type="button"
				className="ops-help"
				aria-label={label ? `${label} · ${t("ops.parameterHelp")}` : t("ops.parameterHelp")}
			>
				<CircleHelp className="size-3.5" />
			</button>
		</OperationsTooltip>
	);
}
export function OperationsHeader({
	title,
	description,
	status,
	action,
}: {
	title: string;
	description?: string;
	status?: ReactNode;
	action?: ReactNode;
}) {
	return (
		<header className="ops-page-header">
			<div className="flex min-w-0 flex-wrap items-center gap-3">
				<h1 className="ops-title">{title}</h1>
				{status}
				{description && <OperationsHelp>{description}</OperationsHelp>}
			</div>
			{action}
		</header>
	);
}
export function OperationsTabs({
	items,
	end,
}: {
	items: { value: string; label: string; count?: number }[];
	end?: ReactNode;
}) {
	return (
		<div className="ops-navigation">
			<TabsPrimitive.List className="ops-tabs">
				{items.map((item) => (
					<TabsPrimitive.Trigger
						type="button"
						className="ops-tab"
						value={item.value}
						key={item.value}
					>
						{item.label}
						{item.count !== undefined && <span className="ops-tab-count">{item.count}</span>}
					</TabsPrimitive.Trigger>
				))}
			</TabsPrimitive.List>
			{end && <div className="ops-navigation-end">{end}</div>}
		</div>
	);
}
export function MetricRail({ children }: { children: ReactNode }) {
	return <div className="ops-metric-rail">{children}</div>;
}
export function OperationalMetric({
	label,
	value,
	detail,
	tone,
	onClick,
}: {
	label: string;
	value: ReactNode;
	detail: string;
	tone?: StatusTone;
	onClick?: () => void;
}) {
	const content = (
		<>
			<span className="ops-metric-label">
				{label}
				{onClick && <ArrowUpRight className="size-3" />}
			</span>
			<strong className={cn("ops-metric-value", tone && `ops-text-${tone}`)}>{value}</strong>
			<span className="sr-only">{detail}</span>
		</>
	);
	return (
		<Tooltip>
			<TooltipTrigger asChild>
				{onClick ? (
					<button type="button" className="ops-metric ops-metric-interactive" onClick={onClick}>
						{content}
					</button>
				) : (
					<div className="ops-metric" tabIndex={0}>
						{content}
					</div>
				)}
			</TooltipTrigger>
			<TooltipContent>{detail}</TooltipContent>
		</Tooltip>
	);
}
export function OperationsSection({
	title,
	description,
	action,
	children,
	className,
}: {
	title: string;
	description?: string;
	action?: ReactNode;
	children: ReactNode;
	className?: string;
}) {
	return (
		<section className={cn("ops-panel", className)}>
			<header className="ops-panel-header">
				<div className="flex items-center gap-2">
					<h2>{title}</h2>
					{description && <OperationsHelp>{description}</OperationsHelp>}
				</div>
				{action}
			</header>
			<div className="ops-panel-body">{children}</div>
		</section>
	);
}
export function OperationsError({ message, retry }: { message?: string; retry?: () => void }) {
	const { t } = useTranslation();
	return (
		<div role="alert" className="ops-error">
			<CircleAlert className="size-4 shrink-0" />
			<span className="flex-1">{message || t("ops.unavailable")}</span>
			{retry && (
				<Button size="sm" variant="outline" onClick={retry}>
					<RefreshCw className="size-3" />
					{t("common.retry")}
				</Button>
			)}
		</div>
	);
}
export function OperationsDialogContent({
	className,
	overlayClassName,
	...props
}: ComponentProps<typeof DialogContent>) {
	return (
		<DialogContent
			{...props}
			className={cn("ops-dialog", className)}
			overlayClassName={cn("ops-dialog-overlay", overlayClassName)}
		/>
	);
}
export function OperationsAlertDialogContent({
	className,
	overlayClassName,
	...props
}: ComponentProps<typeof AlertDialogContent>) {
	return (
		<AlertDialogContent
			{...props}
			className={cn("ops-dialog", className)}
			overlayClassName={cn("ops-dialog-overlay", overlayClassName)}
		/>
	);
}
export function OperationsField({
	controlId,
	label,
	description,
	error,
	className,
	children,
	current,
}: {
	controlId: string;
	label: string;
	description?: string;
	error?: string;
	className?: string;
	children: ReactNode;
	current?: ReactNode;
}) {
	const { t } = useTranslation();
	return (
		<div className={cn("ops-field", className)}>
			<div className="ops-field-heading">
				<label htmlFor={controlId} className="ops-field-label">
					{label}
				</label>
				{description && <OperationsHelp label={label}>{description}</OperationsHelp>}
			</div>
			<div className="ops-field-control">{children}</div>
			{(current !== undefined || error) && (
				<div className="ops-field-note">
					{current !== undefined && (
						<p className="ops-field-current">
							{t("ops.appliedValue")} <span>{current}</span>
						</p>
					)}
					{error && (
						<p role="alert" className="mt-1 text-xs text-destructive">
							{error}
						</p>
					)}
				</div>
			)}
		</div>
	);
}

export function OperationsButton({ className, ...props }: ComponentProps<typeof Button>) {
	return <Button {...props} className={cn("rounded-md", className)} />;
}
