import { useQuery } from "@tanstack/react-query";
import { listAllAccounts } from "@/features/accounts/accounts-api";
import {
	listAllEgressNodes,
	listEgressPools,
} from "@/features/settings/settings-api";
import { getGuardStats } from "@/features/guard/guard-stats-api";
import {
	fetchQualityCases,
	fetchQualityGuard,
} from "@/features/guard/quality-api";
export function useOperationsNodes() {
	return useQuery({
		queryKey: ["egress-nodes", "operations-summary"],
		queryFn: ({ signal }) => listAllEgressNodes({}, signal),
		staleTime: 15000,
		refetchInterval: 15000,
	});
}
export function useOperationsPools() {
	return useQuery({
		queryKey: ["egress-pools"],
		queryFn: ({ signal }) => listEgressPools(signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
}
export function useOperationsAccounts() {
	return useQuery({
		queryKey: ["quality", "account-names"],
		queryFn: ({ signal }) => listAllAccounts({}, signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
}
export function useOperationsCases() {
	return useQuery({
		queryKey: ["quality", "cases"],
		queryFn: ({ signal }) => fetchQualityCases(signal),
		staleTime: 10000,
		refetchInterval: 10000,
	});
}
export function useOperationsGuard() {
	return useQuery({
		queryKey: ["guard-stats"],
		queryFn: getGuardStats,
		staleTime: 10000,
		refetchInterval: 10000,
	});
}
export function useOperationsSelfCheck() {
	return useQuery({
		queryKey: ["quality", "guard"],
		queryFn: ({ signal }) => fetchQualityGuard(signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
}
