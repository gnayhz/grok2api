import { useQuery } from "@tanstack/react-query";
import { listAllEgressNodes, listEgressPools } from "@/entities/egress/egress-api";

export function useEgressNodes() {
	return useQuery({
		queryKey: ["egress-nodes", "operations-summary"],
		queryFn: ({ signal }) => listAllEgressNodes({}, signal),
		staleTime: 15000,
		refetchInterval: 15000,
	});
}

export function useEgressPools() {
	return useQuery({
		queryKey: ["egress-pools"],
		queryFn: ({ signal }) => listEgressPools(signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
}
