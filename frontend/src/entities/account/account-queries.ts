import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { getAccountIdentities, listAccounts } from "@/entities/account/account-api";

/** Bounded identity lookup shares the account mutation invalidation namespace. */
export function useAccountDirectory(accountIDs: readonly number[]) {
	const ids = [...new Set(accountIDs.filter((id) => id > 0).map(String))].sort();
	return useQuery({
		queryKey: ["accounts", "identities", ids],
		queryFn: ({ signal }) => getAccountIdentities(ids, signal),
		placeholderData: keepPreviousData,
		staleTime: 10000,
		refetchInterval: 10000,
		refetchOnMount: "always",
	});
}

/** COUNT is filtered on the server and excludes accounts that no longer exist. */
export function useRestrictedAccountCount() {
	return useQuery({
		queryKey: ["accounts", "restricted-count"],
		queryFn: ({ signal }) => listAccounts({ page: 1, pageSize: 1, quality: "restricted" }, signal),
		staleTime: 10000,
		refetchInterval: 10000,
		refetchOnMount: "always",
	});
}
