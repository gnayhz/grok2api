import { type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/shared/ui/badge";
import { Label } from "@/shared/ui/label";
import { TabsContent } from "@/shared/ui/tabs";
import { cn } from "@/shared/lib/cn";

export function SettingsPane({ value, children }: { value: string; children: ReactNode }) {
  return (
    <TabsContent value={value} forceMount className="m-0 space-y-8 data-[state=inactive]:hidden">
      {children}
    </TabsContent>
  );
}

export function SettingsSection({ title, action, children }: { title: string; action?: ReactNode; children: ReactNode }) {
  return (
    <section className="space-y-3">
      <div className="flex min-h-8 items-center justify-between gap-3 px-1">
        <h2 className="text-sm font-medium tracking-tight">{title}</h2>
        {action}
      </div>
      <div className="min-w-0 w-full">{children}</div>
    </section>
  );
}

export function SettingsField({ controlId, label, badge, description, error, className, children }: { controlId: string; label: string; badge?: string; description?: string; error?: string; className?: string; children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <div className={cn("min-w-0 py-4", className)}>
      <div className="grid min-w-0 gap-2.5 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8">
        <div className="min-w-0">
          <div className="flex min-h-5 items-center gap-2">
            <Label htmlFor={controlId} className="text-xs font-medium">{label}</Label>
            {badge ? <Badge variant="secondary" className="shrink-0 text-[10px]">{badge}</Badge> : null}
          </div>
          {description ? <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{description}</p> : null}
          {error ? <p className="mt-1 text-xs text-destructive">{t("settings.invalidValue")}</p> : null}
        </div>
        <div className="min-w-0">{children}</div>
      </div>
    </div>
  );
}
