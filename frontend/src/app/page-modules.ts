import type { ComponentType } from "react";

import { ensureFeatureI18n, type FeatureTranslation } from "./register-feature-i18n";

function preloadableNamed<T extends Record<K, ComponentType>, K extends keyof T>(loader: () => Promise<T>, exportName: K, translations: readonly FeatureTranslation[] = []) {
  let pending: Promise<{ default: T[K] }> | undefined;
  const load = () => pending ??= Promise.all([loader(), ensureFeatureI18n(translations)]).then(([module]) => ({ default: module[exportName] })).catch(error => {
    pending = undefined;
    throw error;
  });
  return { load, preload: () => { void load().catch(() => {}); } };
}

export const AccountsPageModule = preloadableNamed(() => import("@/features/accounts/accounts-page"), "AccountsPage", ["resourceChecks", "accounts", "accountQuota", "console", "accountBulk", "models", "settings"]);
export const AppShellModule = preloadableNamed(() => import("@/app/app-shell"), "AppShell");
export const RequestAuditsPageModule = preloadableNamed(() => import("@/features/audits/request-audits-page"), "RequestAuditsPage", ["audits", "network", "settings"]);
export const ClientKeysPageModule = preloadableNamed(() => import("@/features/client-keys/client-keys-page"), "ClientKeysPage", ["keys"]);
export const CreativeConsolePageModule = preloadableNamed(() => import("@/features/creative-console/creative-console-page"), "CreativeConsolePage", ["creativeConsole"]);
export const DashboardPageModule = preloadableNamed(() => import("@/features/dashboard/dashboard-page"), "DashboardPage", ["dashboard", "audits", "models", "console", "settings"]);
export const ApiDocsPageModule = preloadableNamed(() => import("@/features/docs/api-docs-page"), "ApiDocsPage", ["docs"]);
export const GalleryPageModule = preloadableNamed(() => import("@/features/media/gallery-page"), "GalleryPage", ["media"]);
export const QualityConsoleModule = preloadableNamed(() => import("@/features/guard/quality-console"), "QualityConsole", ["quality", "experiment", "guardProbes", "ops", "settings", "network", "settingsForm"]);
export const QualitySettingsPageModule = preloadableNamed(() => import("@/app/quality-settings-route"), "QualitySettingsRoute", ["quality", "experiment", "guardProbes", "ops", "settings", "network", "models", "settingsForm"]);
export const VideoGalleryPageModule = preloadableNamed(() => import("@/features/media/video-gallery-page"), "VideoGalleryPage", ["media"]);
export const ModelsPageModule = preloadableNamed(() => import("@/features/models/models-page"), "ModelsPage", ["models", "console"]);
export const ProxiesPageModule = preloadableNamed(() => import("@/features/proxies/proxies-page"), "ProxiesPage", ["resourceChecks", "network", "networkResources", "networkPools", "networkRouting", "ops", "settings", "settingsForm"]);
export const SettingsPageModule = preloadableNamed(() => import("@/features/settings/settings-page"), "SettingsPage", ["settings", "models", "console", "accountBulk", "settingsForm"]);

const routeModules = {
  "/dashboard": DashboardPageModule,
  "/accounts": AccountsPageModule,
  "/client-keys": ClientKeysPageModule,
  "/models": ModelsPageModule,
  "/gallery": GalleryPageModule,
  "/video-gallery": VideoGalleryPageModule,
  "/guard": QualityConsoleModule,
  "/guard/settings": QualitySettingsPageModule,
  "/proxies": ProxiesPageModule,
  "/request-audits": RequestAuditsPageModule,
  "/creative-console": CreativeConsolePageModule,
  "/settings": SettingsPageModule,
};

// Load code on navigation intent; API queries still belong to the mounted page.
export function preloadPage(path: string): void {
  if (path.startsWith("/docs/")) ApiDocsPageModule.preload();
  else routeModules[path as keyof typeof routeModules]?.preload();
}
