import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  formatUSDTicks,
  USD_TICKS_PER_DOLLAR,
  usdTicksToValue,
} from "./usd.ts";

describe("USD tick formatting", () => {
  it("uses ten billion ticks per US dollar", () => {
    assert.equal(USD_TICKS_PER_DOLLAR, 10_000_000_000);
    assert.equal(usdTicksToValue(200_000_000), 0.02);
    assert.equal(formatUSDTicks(200_000_000, 6), "$0.020000");
  });
});
