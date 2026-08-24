import { Settings2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { IntervalInput } from "@/features/proxies/operations-context";
import { useEgressOperations } from "@/features/proxies/operations-shared";

/** Probe settings dialog: which IP echo service checks exits and how often.
 *  Lives in the nodes toolbar so the dedicated tab can go away. */
export function ProbeSettingsButton() {
  const { t } = useTranslation();
  const operations = useEgressOperations();
  const [open, setOpen] = useState(false);
  const form = operations.form;

  return (
    <>
      <Button type="button" size="sm" variant="secondary" disabled={operations.isPending} onClick={() => setOpen(true)}>
        <Settings2 />{t("proxies.automation.settingsButton")}
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("proxies.automation.title")}</DialogTitle>
            <DialogDescription>{t("proxies.automation.help")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="egress-probe-provider">{t("settings.egress.probeProvider")}</Label>
              <Select value={form.probeProvider} onValueChange={(probeProvider: "ipinfo" | "cloudflare") => operations.update((current) => ({ ...current, probeProvider }))}>
                <SelectTrigger id="egress-probe-provider" className="h-8 w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="ipinfo">IPinfo</SelectItem>
                  <SelectItem value="cloudflare">Cloudflare</SelectItem>
                </SelectContent>
              </Select>
              <p className="text-xs leading-5 text-muted-foreground">{t("settings.egress.probeProviderHelp")}</p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="egress-probe-interval">{t("settings.egress.probeInterval")}</Label>
              <IntervalInput id="egress-probe-interval" value={form.probeIntervalSeconds ? String(form.probeIntervalSeconds) : ""} onChange={(probeIntervalSeconds) => operations.update((current) => ({ ...current, probeIntervalSeconds: Number(probeIntervalSeconds) || 0 }))} />
              <p className="text-xs leading-5 text-muted-foreground">{t("settings.egress.probeIntervalHelp")}</p>
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" onClick={() => setOpen(false)}>{t("common.cancel")}</Button>
            <Button type="button" size="sm" disabled={!operations.isDirty || operations.savePending || form.probeIntervalSeconds < 60 || form.probeIntervalSeconds > 86400} onClick={() => { void operations.save().then((saved) => { if (saved) setOpen(false); }); }}>{operations.savePending ? <Spinner /> : null}{t("common.save")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
