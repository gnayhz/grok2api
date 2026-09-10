import { useTranslation } from "react-i18next";
import type { SettingsSnapshotDTO } from "./settings-api";

export function SettingsApplicationStatus({ snapshot }: { snapshot: SettingsSnapshotDTO }) {
  const { t } = useTranslation();
  const pending = snapshot.applyTargets?.filter((target) => target.pending) ?? [];
  return (
    <div role="status" className="space-y-1 rounded-md border px-3 py-2 text-sm">
      <p>{t("settings.application.savedRevision", { revision: snapshot.revision })} · {snapshot.appliedRevision === undefined
        ? t("settings.application.unknown")
        : t("settings.application.appliedRevision", { revision: snapshot.appliedRevision })}</p>
      {snapshot.applyPending ? <p className="text-amber-600 dark:text-amber-400">{t("settings.application.pending")}</p> : null}
      {pending.length ? <ul className="list-inside list-disc text-muted-foreground">{pending.map((target) => (
        <li key={target.name}>{t(`settings.application.targets.${target.name}`, { defaultValue: target.name })} · {t("settings.application.targetPending")}{target.lastAttemptAt ? ` · ${t("settings.application.lastAttempt", { time: new Date(target.lastAttemptAt).toLocaleString() })}` : ""}</li>
      ))}</ul> : null}
      {snapshot.notification ? <p className="text-muted-foreground">{t(`settings.application.notification.${snapshot.notification.state}`)}</p> : null}
      {snapshot.restartRequired.length ? <p>{t("settings.application.restart", { fields: snapshot.restartRequired.join(", ") })}</p> : null}
    </div>
  );
}
