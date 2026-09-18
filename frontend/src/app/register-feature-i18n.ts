import { i18n } from "@/shared/i18n";

type TranslationTree = { [key: string]: string | TranslationTree };
type TranslationBundle = Record<"zh-CN" | "en", TranslationTree>;

// The app owns loading, while each feature/entity owns its vocabulary.
// Literal imports keep the dependency graph and build chunks statically visible.
export const featureTranslationLoaders = {
  resourceChecks: () => import("@/entities/guard/resource-check-translations").then(m => ({ "zh-CN": { resourceChecks: m.resourceChecksZh }, en: { resourceChecks: m.resourceChecksEn } })),
  creativeConsole: () => import("@/features/creative-console/creative-console-translations").then(m => ({ "zh-CN": { creativeConsole: m.creativeConsoleZh }, en: { creativeConsole: m.creativeConsoleEn } })),
  dashboard: () => import("@/features/dashboard/dashboard-translations").then(m => ({ "zh-CN": { dashboard: m.dashboardZh }, en: { dashboard: m.dashboardEn } })),
  settings: () => import("@/features/settings/settings-translations").then(m => ({ "zh-CN": { settings: m.settingsZh }, en: { settings: m.settingsEn } })),
  console: () => import("@/entities/account/console-translations").then(m => ({ "zh-CN": { console: m.consoleProviderZh }, en: { console: m.consoleProviderEn } })),
  settingsForm: () => import("@/entities/settings/form-translations").then(m => ({ "zh-CN": { settingsForm: m.settingsFormZh }, en: { settingsForm: m.settingsFormEn } })),
  accountBulk: () => import("@/entities/settings/batch-translations").then(m => ({ "zh-CN": { accountBulk: m.batchTasksZh }, en: { accountBulk: m.batchTasksEn } })),
  accountQuota: () => import("@/features/accounts/account-quota-translations").then(m => ({ "zh-CN": { accountExport: m.accountQuotaZh.accountExport, accountQuotaReset: m.accountQuotaZh.accountQuotaReset, accountQuotaTask: m.accountQuotaZh.accountQuotaTask }, en: { accountExport: m.accountQuotaEn.accountExport, accountQuotaReset: m.accountQuotaEn.accountQuotaReset, accountQuotaTask: m.accountQuotaEn.accountQuotaTask } })),
  accounts: () => import("@/features/accounts/accounts-translations").then(m => ({ "zh-CN": { accounts: m.accountsZh }, en: { accounts: m.accountsEn } })),
  models: () => import("@/features/models/models-translations").then(m => ({ "zh-CN": { models: m.modelsZh }, en: { models: m.modelsEn } })),
  media: () => import("@/features/media/media-translations").then(m => ({ "zh-CN": { media: m.mediaZh }, en: { media: m.mediaEn } })),
  keys: () => import("@/features/client-keys/keys-translations").then(m => ({ "zh-CN": { keys: m.clientkeysZh }, en: { keys: m.clientkeysEn } })),
  audits: () => import("@/features/audits/audits-translations").then(m => ({ "zh-CN": { audits: m.auditsZh }, en: { audits: m.auditsEn } })),
  docs: () => import("@/features/docs/docs-translations").then(m => ({ "zh-CN": { docs: m.docsZh }, en: { docs: m.docsEn } })),
  experiment: () => import("@/features/guard/experiment-translations").then(m => ({ "zh-CN": { experiment: m.experimentZh }, en: { experiment: m.experimentEn } })),
  quality: () => import("@/features/guard/quality-translations").then(m => ({ "zh-CN": { quality: m.qualityZh }, en: { quality: m.qualityEn } })),
  guardProbes: () => import("@/features/guard/probe-translations").then(m => ({ "zh-CN": { guardProbes: m.probeZh }, en: { guardProbes: m.probeEn } })),
  ops: () => import("@/features/operations/operations-translations").then(m => ({ "zh-CN": { ops: m.operationsZh }, en: { ops: m.operationsEn } })),
  network: () => import("@/features/proxies/network-translations").then(m => ({ "zh-CN": { network: m.networkZh }, en: { network: m.networkEn } })),
  networkPools: () => import("@/features/proxies/pools-translations").then(m => ({ "zh-CN": { networkPools: m.poolsZh }, en: { networkPools: m.poolsEn } })),
  networkResources: () => import("@/features/proxies/resources-translations").then(m => ({ "zh-CN": { networkResources: m.resourcesZh }, en: { networkResources: m.resourcesEn } })),
  networkRouting: () => import("@/features/proxies/routing-translations").then(m => ({ "zh-CN": { networkRouting: m.routingZh }, en: { networkRouting: m.routingEn } })),
} satisfies Record<string, () => Promise<TranslationBundle>>;

export type FeatureTranslation = keyof typeof featureTranslationLoaders;

const pending = new Map<FeatureTranslation, Promise<void>>();

// Register both languages before rendering the page. Language changes are then
// synchronous, concurrent navigation shares a load, and failures remain retryable.
export async function ensureFeatureI18n(features: readonly FeatureTranslation[]): Promise<void> {
  await Promise.all(features.map(feature => {
    let load = pending.get(feature);
    if (!load) {
      load = featureTranslationLoaders[feature]().then(bundle => {
        for (const language of ["zh-CN", "en"] as const) {
          i18n.addResourceBundle(language, "translation", bundle[language], true, true);
        }
      }).catch(error => {
        pending.delete(feature);
        throw error;
      });
      pending.set(feature, load);
    }
    return load;
  }));
}
