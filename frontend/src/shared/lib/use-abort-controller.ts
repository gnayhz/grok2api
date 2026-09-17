import { useCallback, useEffect, useRef } from "react";

// useAbortController 持有一个共享 AbortController 的生命周期(控制器
// 基元):退出时取消;begin 领取新控制器;cancel 主动取消。视图经命名
// 返回值消费;一个控制器拥有其 AbortController。
export function useAbortController() {
	const ref = useRef<AbortController | null>(null);
	const begin = useCallback(() => {
		ref.current?.abort();
		const controller = new AbortController();
		ref.current = controller;
		return controller;
	}, []);
	const cancel = useCallback(() => {
		ref.current?.abort();
		ref.current = null;
	}, []);
	useEffect(() => () => ref.current?.abort(), []);
	return { ref, begin, cancel };
}
