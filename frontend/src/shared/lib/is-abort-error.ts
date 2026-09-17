// 中止错误判定:请求被 AbortController 取消时 fetch 抛出
// AbortError(DOMException)或同名 Error。统一谓词,避免各页面复制。
export function isAbortError(error: unknown): boolean {
	return (error instanceof DOMException || error instanceof Error) && error.name === "AbortError";
}
