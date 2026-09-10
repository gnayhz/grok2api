import { ok, notEqual } from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { experimentEn, experimentZh } from "./experiment-translations.ts";

test("persisted probe failure codes have operator labels in both languages", () => {
	const source = readFileSync(resolve(import.meta.dirname, "../../../../backend/internal/quality/model/probe_failure.go"), "utf8");
	const codes = [...source.matchAll(/ProbeFailure\s*=\s*"([^"]+)"/g)].map((match) => match[1]);
	ok(codes.length > 0, "probe failure vocabulary must be found");
	for (const labels of [experimentZh.result, experimentEn.result]) {
		for (const code of codes) {
			ok(Object.hasOwn(labels, code), `missing operator label: ${code}`);
			notEqual(labels[code as keyof typeof labels], code);
		}
	}
});
