import type { AccountProvider } from "@/entities/account/account-api";

export function downloadAccountExport(blob: Blob, provider: AccountProvider, suffix: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `grok2api-${provider.replaceAll("_", "-")}-accounts-${suffix}-${new Date().toISOString().slice(0, 10)}.json`;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}
