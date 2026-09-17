import { useTranslation } from "react-i18next";

import { Input } from "@/shared/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/shared/ui/select";
import { isDurationUnit, type DurationValue } from "@/shared/lib/duration";

// 设置域跨页字段组件:设置页与质量防护页共用同一套时长输入,避免样式漂移。

export function DurationInput({ id, value, onChange, disabled, allowZero = false }: { id: string; value?: DurationValue; onChange: (value: DurationValue) => void; disabled?: boolean; allowZero?: boolean }) {
  const { t } = useTranslation();
  const unit = value?.unit ?? "s";
  return (
    <div className="flex min-w-0">
      <Input
        id={id}
        type="number"
        min={allowZero ? "0" : "0.001"}
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
