// 时长值的通用表达与换算:设置表单、质量防护表单与字段组件共用。
// 纯函数,无域依赖;序列化口径与后端 duration 解析一致。

export type DurationUnit = "s" | "m" | "h" | "d";
export type DurationValue = { value: number; unit: DurationUnit };
export function isDurationUnit(value: string): value is DurationUnit {
  return value === "s" || value === "m" || value === "h" || value === "d";
}
export function durationSeconds(value: DurationValue): number {
  const factors: Record<DurationUnit, number> = { s: 1, m: 60, h: 3_600, d: 86_400 };
  return value.value * factors[value.unit];
}
export function formatDuration(value: DurationValue): string {
  if (value.unit === "d") return `${value.value * 24}h`;
  return `${value.value}${value.unit}`;
}
// 0 是有意义值(关闭/默认)的时长字段:0 必须序列化为 "0s" 而不是被抹掉。
export function formatNonNegativeDuration(value: DurationValue): string {
  if (durationSeconds(value) === 0) return "0s";
  return formatDuration(value);
}
export function parseDuration(value: string): DurationValue {
  const simple = value.match(/^(\d+(?:\.\d+)?)(ms|s|m|h)$/);
  if (simple) {
    const amount = Number(simple[1]);
    if (simple[2] === "ms") return { value: amount / 1000, unit: "s" };
    if (simple[2] === "h" && amount >= 24 && amount % 24 === 0) return { value: amount / 24, unit: "d" };
    if (isDurationUnit(simple[2])) return { value: amount, unit: simple[2] };
  }

  const factors: Record<string, number> = { ns: 0.000001, us: 0.001, "µs": 0.001, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  const parts = [...value.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)];
  if (parts.map((part) => part[0]).join("") !== value || parts.length === 0) return { value: 1, unit: "s" };
  const milliseconds = parts.reduce((total, part) => total + Number(part[1]) * factors[part[2]], 0);
  const units: Array<[DurationUnit, number]> = [["d", 86_400_000], ["h", 3_600_000], ["m", 60_000], ["s", 1000]];
  for (const [unit, factor] of units) {
    const amount = milliseconds / factor;
    if (amount >= 1 && Number.isInteger(amount)) return { value: amount, unit };
  }
  return { value: milliseconds / 1000, unit: "s" };
}
