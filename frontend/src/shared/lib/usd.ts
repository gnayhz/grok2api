export const USD_TICKS_PER_DOLLAR = 10_000_000_000;

export function usdTicksToValue(ticks: number): number {
  return ticks / USD_TICKS_PER_DOLLAR;
}

export function formatUSDTicks(ticks: number, fractionDigits: number): string {
  return `$${usdTicksToValue(ticks).toFixed(fractionDigits)}`;
}
