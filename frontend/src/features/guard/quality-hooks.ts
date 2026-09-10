import { useEffect, useState } from "react";

// 监控台共享 hooks(与组件文件分离,保持 fast-refresh 边界干净)。

/** Shared refresh clock for live quality metrics. */
export function useNow(intervalMs = 1_000): number {
	const [now, setNow] = useState(() => Date.now());
	useEffect(() => {
		const timer = setInterval(() => setNow(Date.now()), intervalMs);
		return () => clearInterval(timer);
	}, [intervalMs]);
	return now;
}
