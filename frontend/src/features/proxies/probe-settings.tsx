import { NetworkField as OperationsField } from "./network-ui";
import {
	NetworkDialogContent as DialogContent,
	NetworkDialogHeader as DialogHeader,
	NetworkDialogFooter as DialogFooter,
} from "./network-ui";
import { Settings2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { NetworkButton as Button } from "./network-ui";
import { Dialog, DialogTitle } from "@/components/ui/dialog";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { IntervalInput } from "./operations-context";
import { useEgressOperations } from "./operations-shared";

/** Edits stay local until explicitly applied to the shared configuration draft.
 * Closing or cancelling this dialog never modifies the routing draft. */
export function ProbeSettingsButton({ compact = false }: { compact?: boolean }) {
	const { t } = useTranslation();
	const operations = useEgressOperations();
	const [open, setOpen] = useState(false);
	const [provider, setProvider] = useState(operations.form.probeProvider);
	const [interval, setInterval] = useState(String(operations.form.probeIntervalSeconds));
	const seconds = Number(interval),
		invalid = !interval.trim() || !Number.isInteger(seconds) || seconds < 60 || seconds > 86400;
	return (
		<>
			<Button
				type="button"
				size="sm"
				variant="outline"
				aria-label={t("proxies.automation.settingsButton")}
				title={compact ? t("proxies.automation.settingsButton") : undefined}
				className={compact ? "size-8 p-0" : undefined}
				disabled={operations.isPending}
				onClick={() => {
					setProvider(operations.form.probeProvider);
					setInterval(String(operations.form.probeIntervalSeconds));
					setOpen(true);
				}}
			>
				<Settings2 />
				{!compact && t("proxies.automation.settingsButton")}
			</Button>
			<Dialog open={open} onOpenChange={setOpen}>
				<DialogContent layout="compact" aria-describedby={undefined}>
					<DialogHeader>
						<DialogTitle>{t("proxies.automation.title")}</DialogTitle>
					</DialogHeader>
					<div className="net-inline-fields">
						<OperationsField
							controlId="egress-probe-provider"
							label={t("settings.egress.probeProvider")}
							description={t("settings.egress.probeProviderHelp")}
						>
							<Select
								value={provider}
								onValueChange={(value: "ipinfo" | "cloudflare") => setProvider(value)}
							>
								<SelectTrigger id="egress-probe-provider" autoFocus>
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									<SelectItem value="ipinfo">IPinfo</SelectItem>
									<SelectItem value="cloudflare">Cloudflare</SelectItem>
								</SelectContent>
							</Select>
						</OperationsField>
						<OperationsField
							controlId="egress-probe-interval"
							label={t("settings.egress.probeInterval")}
							description={t("settings.egress.probeIntervalHelp")}
							error={invalid ? t("ops.probeIntervalInvalid") : undefined}
						>
							<IntervalInput id="egress-probe-interval" value={interval} onChange={setInterval} />
						</OperationsField>
					</div>
					<DialogFooter>
						<Button type="button" variant="outline" size="sm" onClick={() => setOpen(false)}>
							{t("common.cancel")}
						</Button>
						<Button
							type="button"
							size="sm"
							disabled={invalid}
							onClick={() => {
								operations.update((current) => ({
									...current,
									probeProvider: provider,
									probeIntervalSeconds: seconds,
								}));
								setOpen(false);
							}}
						>
							{t("ops.applyDraft")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</>
	);
}
