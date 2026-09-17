import type { ComponentType } from "react";

function preloadableNamed<T extends Record<K, ComponentType>, K extends keyof T>(loader: () => Promise<T>, exportName: K) {
  let pending: Promise<{ default: T[K] }> | undefined;
  const load = () => pending ??= loader().then(module => ({ default: module[exportName] })).catch(error => {
    pending = undefined;
    throw error;
  });
  return { load, preload: () => { void load().catch(() => {}); } };
}

export const AccountsPageModule = preloadableNamed(() => import("@/features/accounts/accounts-page"), "AccountsPage");
export const AppShellModule = preloadableNamed(() => import("@/app/app-shell"), "AppShell");
export const RequestAuditsPageModule = preloadableNamed(() => import("@/features/audits/request-audits-page"), "RequestAuditsPage");
export const ClientKeysPageModule = preloadableNamed(() => import("@/features/client-keys/client-keys-page"), "ClientKeysPage");
export const CreativeConsolePageModule = preloadableNamed(() => import("@/features/creative-console/creative-console-page"), "CreativeConsolePage");
export const DashboardPageModule = preloadableNamed(() => import("@/features/dashboard/dashboard-page"), "DashboardPage");
export const ApiDocsPageModule = preloadableNamed(() => import("@/features/docs/api-docs-page"), "ApiDocsPage");
export const GalleryPageModule = preloadableNamed(() => import("@/features/media/gallery-page"), "GalleryPage");
export const QualityConsoleModule = preloadableNamed(() => import("@/features/guard/quality-console"), "QualityConsole");
export const QualitySettingsPageModule = preloadableNamed(() => import("@/app/quality-settings-route"), "QualitySettingsRoute");
export const VideoGalleryPageModule = preloadableNamed(() => import("@/features/media/video-gallery-page"), "VideoGalleryPage");
export const ModelsPageModule = preloadableNamed(() => import("@/features/models/models-page"), "ModelsPage");
export const ProxiesPageModule = preloadableNamed(() => import("@/features/proxies/proxies-page"), "ProxiesPage");
export const SettingsPageModule = preloadableNamed(() => import("@/features/settings/settings-page"), "SettingsPage");

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
