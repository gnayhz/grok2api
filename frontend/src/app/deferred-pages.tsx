import { lazy, Suspense, type ComponentType } from "react";

import { AccountsPageModule, AppShellModule, RequestAuditsPageModule, ClientKeysPageModule, CreativeConsolePageModule, DashboardPageModule, ApiDocsPageModule, GalleryPageModule, QualityConsoleModule, QualitySettingsPageModule, VideoGalleryPageModule, ModelsPageModule, ProxiesPageModule, SettingsPageModule } from "./page-modules";

import { Spinner } from "@/shared/ui/spinner";
import { GuardStatusBanner } from "@/features/guard/guard-status-banner";
import { VersionUpdateBanner, VersionUpdateSection } from "@/features/system/version-update";

const AccountsPage = lazy(AccountsPageModule.load);
const AppShell = lazy(AppShellModule.load);
const RequestAuditsPage = lazy(RequestAuditsPageModule.load);
const ClientKeysPage = lazy(ClientKeysPageModule.load);
const CreativeConsolePage = lazy(CreativeConsolePageModule.load);
const DashboardPage = lazy(DashboardPageModule.load);
const ApiDocsPage = lazy(ApiDocsPageModule.load);
const GalleryPage = lazy(GalleryPageModule.load);
const QualityConsole = lazy(QualityConsoleModule.load);
const QualitySettingsPage = lazy(QualitySettingsPageModule.load);
const VideoGalleryPage = lazy(VideoGalleryPageModule.load);
const ModelsPage = lazy(ModelsPageModule.load);
const ProxiesPage = lazy(ProxiesPageModule.load);
const SettingsPage = lazy(SettingsPageModule.load);

function DeferredPage({ page: Page }: { page: ComponentType }) {
  return <Suspense fallback={<PageLoadingFallback />}><Page /></Suspense>;
}

export function DeferredAccountsPage() {
  return <DeferredPage page={AccountsPage} />;
}

export function DeferredAppShell() {
  return <Suspense fallback={<PageLoadingFallback fullScreen />}><AppShell /></Suspense>;
}

export function DeferredDashboardPage() {
  // App 层组合:状态横幅来自 guard/system feature,页面本身保持 feature 无互赖。
  return (
    <Suspense fallback={<PageLoadingFallback />}>
      <DashboardPage bannerSlot={<><VersionUpdateBanner /><GuardStatusBanner /></>} />
    </Suspense>
  );
}

export function DeferredModelsPage() {
  return <DeferredPage page={ModelsPage} />;
}

export function DeferredProxiesPage() {
  return <DeferredPage page={ProxiesPage} />;
}

export function DeferredClientKeysPage() {
  return <DeferredPage page={ClientKeysPage} />;
}

export function DeferredCreativeConsolePage() {
  return <DeferredPage page={CreativeConsolePage} />;
}

export function DeferredRequestAuditsPage() {
  return <DeferredPage page={RequestAuditsPage} />;
}

export function DeferredGuardPage() {
  return <DeferredPage page={QualityConsole} />;
}

export function DeferredQualitySettingsPage() {
  return <DeferredPage page={QualitySettingsPage} />;
}

export function DeferredGalleryPage() {
  return <DeferredPage page={GalleryPage} />;
}

export function DeferredVideoGalleryPage() {
  return <DeferredPage page={VideoGalleryPage} />;
}

export function DeferredApiDocsPage() {
  return <DeferredPage page={ApiDocsPage} />;
}

export function DeferredSettingsPage() {
  return (
    <Suspense fallback={<PageLoadingFallback />}>
      <SettingsPage versionSection={<VersionUpdateSection />} />
    </Suspense>
  );
}

function PageLoadingFallback({ fullScreen = false }: { fullScreen?: boolean }) {
  return (
    <div className={fullScreen ? "flex min-h-screen items-center justify-center bg-background" : "flex min-h-[calc(100vh-7rem)] items-center justify-center lg:min-h-[calc(100vh-10rem)]"}>
      <Spinner className="size-5" />
    </div>
  );
}
