import { useQuery } from "@tanstack/react-query";
import { getGuardStats } from "@/entities/guard/guard-stats-api";
import { fetchQualityCases, fetchQualityGuard } from "@/entities/guard/quality-api";

export function useQualityCases() {
	return useQuery({
		queryKey: ["quality", "cases"],
		queryFn: ({ signal }) => fetchQualityCases(signal),
		staleTime: 10000,
		refetchInterval: 10000,
	});
}

// 轮询节奏可按调用方覆盖(横幅等低频消费方放慢),未传时保持 10s。
export function useGuardStats(options?: { refetchInterval?: number }) {
	return useQuery({
		queryKey: ["guard-stats"],
		queryFn: getGuardStats,
		staleTime: 10000,
		refetchInterval: options?.refetchInterval ?? 10000,
	});
}

export function useQualityGuardSelfCheck() {
	return useQuery({
		queryKey: ["quality", "guard"],
		queryFn: ({ signal }) => fetchQualityGuard(signal),
		staleTime: 30000,
		refetchInterval: 30000,
	});
}
