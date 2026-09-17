// 薄 re-export:通用数字/USD 格式化统一收在 shared/lib/format,
// dashboard 侧仅保留原导入路径,避免各视图散落重复实现。
export { formatUSD, formatUSDValue, formatCompactUSD, formatCompactNumber } from "@/shared/lib/format";
