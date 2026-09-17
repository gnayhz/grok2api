// 集中注册各 feature 的文案包(i18next 深合并,feature 同名键优先于
// shared 基础包)。注意:文件名沿用 feature 语义,但这里同时注册少数
// entities 层文案(console=entities/account、accountBulk=entities/settings),
// 它们与 feature 包同批次注册,避免拆出第二个注册入口。
import { creativeConsoleEn, creativeConsoleZh } from "@/features/creative-console/creative-console-translations";
import { dashboardEn, dashboardZh } from "@/features/dashboard/dashboard-translations";
import { settingsEn, settingsZh } from "@/features/settings/settings-translations";
import { consoleProviderEn, consoleProviderZh } from "@/entities/account/console-translations";
import { batchTasksEn, batchTasksZh } from "@/entities/settings/batch-translations";
import { accountQuotaEn, accountQuotaZh } from "@/features/accounts/account-quota-translations";
import { accountsEn, accountsZh } from "@/features/accounts/accounts-translations";
import { modelsEn, modelsZh } from "@/features/models/models-translations";
import { mediaEn, mediaZh } from "@/features/media/media-translations";
import { clientkeysEn, clientkeysZh } from "@/features/client-keys/keys-translations";
import { auditsEn, auditsZh } from "@/features/audits/audits-translations";
import { docsEn, docsZh } from "@/features/docs/docs-translations";
import { experimentEn, experimentZh } from "@/features/guard/experiment-translations";
import { qualityEn, qualityZh } from "@/features/guard/quality-translations";
import { probeEn, probeZh } from "@/features/guard/probe-translations";
import { operationsEn, operationsZh } from "@/features/operations/operations-translations";
import { networkEn, networkZh } from "@/features/proxies/network-translations";
import { poolsEn, poolsZh } from "@/features/proxies/pools-translations";
import { resourcesEn, resourcesZh } from "@/features/proxies/resources-translations";
import { routingEn, routingZh } from "@/features/proxies/routing-translations";
import { i18n } from "@/shared/i18n";

export const featureTranslationBundles = {
  "zh-CN": {
    experiment: experimentZh,
    creativeConsole: creativeConsoleZh,
    dashboard: dashboardZh,
    settings: settingsZh,
    console: consoleProviderZh,
    accountBulk: batchTasksZh,
    accountExport: accountQuotaZh.accountExport,
    accountQuotaReset: accountQuotaZh.accountQuotaReset,
    accountQuotaTask: accountQuotaZh.accountQuotaTask,
    models: modelsZh,
    quality: qualityZh,
    audits: auditsZh,
    media: mediaZh,
    accounts: accountsZh,
    keys: clientkeysZh,
    docs: docsZh,
    guardProbes: probeZh,
    ops: operationsZh,
    network: networkZh,
    networkResources: resourcesZh,
    networkPools: poolsZh,
    networkRouting: routingZh,
  },
  en: {
    experiment: experimentEn,
    creativeConsole: creativeConsoleEn,
    dashboard: dashboardEn,
    settings: settingsEn,
    console: consoleProviderEn,
    accountBulk: batchTasksEn,
    accountExport: accountQuotaEn.accountExport,
    accountQuotaReset: accountQuotaEn.accountQuotaReset,
    accountQuotaTask: accountQuotaEn.accountQuotaTask,
    models: modelsEn,
    quality: qualityEn,
    audits: auditsEn,
    media: mediaEn,
    accounts: accountsEn,
    keys: clientkeysEn,
    docs: docsEn,
    guardProbes: probeEn,
    ops: operationsEn,
    network: networkEn,
    networkResources: resourcesEn,
    networkPools: poolsEn,
    networkRouting: routingEn,
  },
};

export function registerFeatureI18n(): void {
  i18n.addResourceBundle("zh-CN", "translation", featureTranslationBundles["zh-CN"], true, true);
  i18n.addResourceBundle("en", "translation", featureTranslationBundles.en, true, true);
}

registerFeatureI18n();
