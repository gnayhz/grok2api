import { useQuery } from "@tanstack/react-query";
import { listAllAccounts } from "@/entities/account/account-api";

/** Directory of accounts for cross-page lookup tables (names, ids). */
export function useAccountDirectory() {
	return useQuery({
		queryKey: ["quality", "account-names"],
		queryFn: ({ signal }) => listAllAccounts({}, signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
}
