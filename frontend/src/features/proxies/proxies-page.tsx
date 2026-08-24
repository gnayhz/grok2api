import { Network, Settings2, Waypoints } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { NodesPanel } from "@/features/proxies/nodes-panel";
import { EgressOperationsProvider } from "@/features/proxies/operations-context";
import { useEgressOperations } from "@/features/proxies/operations-shared";
import { PoolsPanel } from "@/features/proxies/pools-panel";
import { RoutingPanel } from "@/features/proxies/routing-panel";

const sections = [
  { value: "nodes", icon: Waypoints, labelKey: "proxies.tabs.nodes" },
  { value: "pools", icon: Network, labelKey: "proxies.tabs.pools" },
  { value: "routing", icon: Settings2, labelKey: "proxies.tabs.routing" },
] as const;

/**
 * Route rules + automation share one operations payload; the save action sits
 * in the page header (settings-page pattern) and only lights up when the
 * shared draft is dirty.
 */
function ProxiesSaveAction() {
  const { t } = useTranslation();
  const operations = useEgressOperations();
  if (!operations.isDirty) return null;
  return (
    <div className="flex shrink-0 items-center gap-2">
      <Button type="button" variant="ghost" size="sm" disabled={operations.savePending} onClick={operations.discard}>
        {t("proxies.discard")}
      </Button>
      <Button type="button" size="sm" disabled={operations.savePending} onClick={operations.save}>
        {operations.savePending ? <Spinner /> : null}{t("common.save")}
      </Button>
    </div>
  );
}

export function ProxiesPage() {
  const { t } = useTranslation();
  return (
    <EgressOperationsProvider>
      <div className="w-full space-y-5">
        <header className="flex min-h-8 items-center justify-between gap-3">
          <h1 className="text-xl font-medium">{t("proxies.title")}</h1>
          <ProxiesSaveAction />
        </header>

        <Tabs defaultValue="nodes" className="gap-5">
          <div className="flex flex-wrap items-center gap-2">
            <TabsList>
              {sections.map(({ value, icon: Icon, labelKey }) => (
                <TabsTrigger key={value} value={value} className="gap-1.5">
                  <Icon className="size-3.5" />
                  <span>{t(labelKey)}</span>
                </TabsTrigger>
              ))}
            </TabsList>
          </div>

          <TabsContent value="nodes" className="mt-0"><NodesPanel /></TabsContent>
          <TabsContent value="pools" className="mt-0"><PoolsPanel /></TabsContent>
          <TabsContent value="routing" className="mt-0"><RoutingPanel /></TabsContent>
        </Tabs>
      </div>
    </EgressOperationsProvider>
  );
}
