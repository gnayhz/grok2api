import { useSettings } from "@/features/settings/use-settings";
import { QualitySettingsPage } from "@/features/guard/quality-settings-page";

/** App 层组合:/guard/settings 复用设置表单运行时,
 * 使 guard feature 不需要 import settings feature。 */
export function QualitySettingsRoute() {
  const settings = useSettings();
  return <QualitySettingsPage settings={settings} />;
}
