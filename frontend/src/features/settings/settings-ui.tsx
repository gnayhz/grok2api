import { type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TabsContent } from "@/components/ui/tabs";
import { cn } from "@/shared/lib/cn";
import { isDurationUnit, type DurationValue } from "@/features/settings/settings-model";

// 从 settings-page 抽出的表单原子组件:设置页与质量防护页共用同一套
// 字段布局/时长输入,避免两处样式漂移。

export function DurationInput({ id, value, onChange, disabled }: { id: string; value?: DurationValue; onChange: (value: DurationValue) => void; disabled?: boolean }) {
  const { t } = useTranslation();
  const unit = value?.unit ?? "s";
  return (
    <div className="flex min-w-0">
      <Input
        id={id}
        type="number"
        min="0.001"
        step="any"
        disabled={disabled}
        className="min-w-0 rounded-r-none"
        value={Number.isFinite(value?.value) ? value?.value : ""}
        onChange={(event) => onChange({ value: event.target.value === "" ? Number.NaN : Number(Number(event.target.value)), unit })}
      />
      <Select value={unit} disabled={disabled} onValueChange={(nextUnit) => { if (isDurationUnit(nextUnit)) onChange({ value: value?.value ?? 1, unit: nextUnit }); }}>
        <SelectTrigger className="w-24 shrink-0 rounded-l-none bg-secondary/55" aria-label={t("settings.durationUnit")}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="s">{t("settings.units.seconds")}</SelectItem>
          <SelectItem value="m">{t("settings.units.minutes")}</SelectItem>
          <SelectItem value="h">{t("settings.units.hours")}</SelectItem>
          <SelectItem value="d">{t("settings.units.days")}</SelectItem>
        </SelectContent>
      </Select>
    </div>
  );
}

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
