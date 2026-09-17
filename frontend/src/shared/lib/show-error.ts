import { toast } from "sonner";

type Translate = (key: string) => string;

// 统一的操作失败提示:优先透传后端/库抛出的错误消息,没有可读消息时
// 回退到基础目录的 errors.generic(中英文均已定义,由 i18n-ownership
// 测试允许 shared 消费 base 键)。t 由调用方注入,shared 层不耦合 i18n 实例。
export function showErrorToast(error: unknown, t: Translate): void {
	toast.error(error instanceof Error ? error.message : t("errors.generic"));
}
