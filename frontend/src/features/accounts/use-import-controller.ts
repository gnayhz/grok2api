import { useCallback, useEffect, useRef } from "react";
import { toast } from "sonner";

// useImportController 持有账号导入的 AbortController 与进度 toast
// 生命周期: 一个控制器拥有其 AbortController 与 toast 句柄; 退出时
// 取消请求并撤销提示。视图经命名参数接收状态/命令。
export function useImportController() {
	const abortRef = useRef<AbortController | null>(null);
	const toastRef = useRef<string | number | null>(null);

	const dismiss = useCallback(() => {
		if (toastRef.current !== null) toast.dismiss(toastRef.current);
		toastRef.current = null;
	}, []);

	const cancel = useCallback(() => {
		abortRef.current?.abort();
		dismiss();
		abortRef.current = null;
	}, [dismiss]);

	useEffect(() => () => {
		abortRef.current?.abort();
		if (toastRef.current !== null) toast.dismiss(toastRef.current);
	}, []);

	return { abortRef, toastRef, cancel, dismiss };
}
