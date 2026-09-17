import { useQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Checkbox } from "@/shared/ui/checkbox";
import { Input } from "@/shared/ui/input";
import { Spinner } from "@/shared/ui/spinner";
import { listModelAccountOptions } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";

// Query ownership follows the visible picker. The form owns the complete
// selection, so changing pages or searching never drops hidden selections.
export function ModelAccountPicker({ provider, selectedIDs, onToggle }: { provider: ModelRouteDTO["provider"]; selectedIDs: string[]; onToggle: (id: string, checked: boolean) => void }) {
  const { t } = useTranslation();
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  const debouncedSearch = useDebouncedValue(search);
  const options = useQuery({
    queryKey: ["models", "account-options", provider, page, debouncedSearch],
    queryFn: ({ signal }) => listModelAccountOptions({ provider, page, pageSize: 50, search: debouncedSearch }, signal),
  });
  const items = options.data?.items ?? [];
  return (
    <div className="overflow-hidden rounded-md bg-background/55 p-1">
      <div className="relative">
        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input className="bg-transparent pl-8 shadow-none focus-visible:bg-background/70" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("models.searchAccounts")} />
      </div>
      <div className="mt-1 max-h-40 overflow-y-auto overscroll-contain sm:max-h-44">
        {options.isPending ? <div className="flex min-h-20 items-center justify-center"><Spinner /></div> : null}
        {options.isError ? <p className="p-3 text-center text-xs text-destructive">{options.error.message}</p> : null}
        {!options.isPending && !options.isError && items.length === 0 ? <p className="p-3 text-center text-xs text-muted-foreground">{t("models.noBindableAccounts")}</p> : null}
        {items.map((account) => {
          const controlId = `model-account-${account.id}`;
          const checked = selectedIDs.includes(account.id);
          return (
            <label key={account.id} htmlFor={controlId} className={cn("flex h-8 cursor-pointer items-center gap-2.5 rounded-md px-2 text-xs transition-colors hover:bg-accent/40", checked && "bg-accent/55")}>
              <Checkbox id={controlId} checked={checked} onCheckedChange={(value) => onToggle(account.id, value === true)} />
              <span className="min-w-0 flex-1 truncate" title={account.name}>{account.name}</span>
              <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">#{account.id}</span>
            </label>
          );
        })}
      </div>
      {options.data ? <Pagination className="mt-1" page={page} pageSize={50} total={options.data.total} onPageChange={setPage} /> : null}
    </div>
  );
}
