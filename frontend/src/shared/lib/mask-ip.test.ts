import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { maskIP } from "./mask-ip.ts";

describe("maskIP", () => {
	it("keeps the first two IPv4 octets", () => {
		assert.equal(maskIP("198.51.100.7"), "198.51.*.*");
	});
	it("masks IPv6 groups after the first two", () => {
		assert.equal(maskIP("2001:0db8:85a3::8a2e:0370:7334"), "2001:0db8:****:****");
	});
	it("masks provider-style IPv6 exits", () => {
		assert.equal(maskIP("2400:cb00:2048:1::c629:d7a2"), "2400:cb00:****:****");
	});
	it("renders -- for missing values", () => {
		assert.equal(maskIP(undefined), "--");
		assert.equal(maskIP(""), "--");
	});
});
