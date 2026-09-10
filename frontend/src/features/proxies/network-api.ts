import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isNumber, isBoolean } from "@/shared/api/decoder";

export type NetworkRuntime = {
	network: {
		connections: number;
		activeConnections: number;
		idleConnections: number;
		establishingConnections: number;
		dialing: number;
		requests: number;
		waiters: number;
		rejected: number;
		draining: boolean;
		limits: { Connections: number; Dialing: number; Requests: number; Waiters: number };
	};
};

// Limits retain their Go field names in the existing runtime response.
export const decodeNetworkRuntime = createObjectDecoder<NetworkRuntime>("network runtime", {
	network: hasShape({
		connections: isNumber,
		activeConnections: isNumber,
		idleConnections: isNumber,
		establishingConnections: isNumber,
		dialing: isNumber,
		requests: isNumber,
		waiters: isNumber,
		rejected: isNumber,
		draining: isBoolean,
		limits: hasShape({
			Connections: isNumber,
			Dialing: isNumber,
			Requests: isNumber,
			Waiters: isNumber,
		}),
	}),
});

export function getNetworkRuntime(signal?: AbortSignal): Promise<NetworkRuntime> {
	return apiRequest("/api/admin/v1/egress-operations/runtime", { signal }, decodeNetworkRuntime);
}
