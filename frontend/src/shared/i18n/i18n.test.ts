import { deepStrictEqual } from "node:assert";
import { test } from "node:test";

import { i18nResources as resources } from "./index.ts";

type Tree = { [key: string]: string | Tree };

function collectKeys(node: Tree, prefix: string, keys: Map<string, string>): void {
	for (const [key, value] of Object.entries(node)) {
		const path = prefix ? `${prefix}.${key}` : key;
		if (typeof value === "string") {
			keys.set(path, value);
		} else {
			collectKeys(value, path, keys);
		}
	}
}

function placeholders(value: string): string[] {
	return [...value.matchAll(/\{\{(\w+)\}\}/g)].map((m) => m[1]!).sort();
}

const zh = new Map<string, string>();
const en = new Map<string, string>();
collectKeys((resources as Record<string, { translation: Tree }>)["zh-CN"].translation, "", zh);
collectKeys((resources as Record<string, { translation: Tree }>)["en"].translation, "", en);

test("zh-CN and en expose identical key sets", () => {
	const missingInEn = [...zh.keys()].filter((k) => !en.has(k));
	const missingInZh = [...en.keys()].filter((k) => !zh.has(k));
	deepStrictEqual(missingInEn, [], `keys missing in en (renders raw key to users): ${missingInEn.slice(0, 20).join(", ")}`);
	deepStrictEqual(missingInZh, [], `keys missing in zh-CN: ${missingInZh.slice(0, 20).join(", ")}`);
});

test("interpolation placeholders match across locales", () => {
	const mismatched: string[] = [];
	for (const [key, zhValue] of zh) {
		const enValue = en.get(key);
		if (enValue === undefined) continue;
		if (JSON.stringify(placeholders(zhValue)) !== JSON.stringify(placeholders(enValue))) {
			mismatched.push(`${key} zh=[${placeholders(zhValue)}] en=[${placeholders(enValue)}]`);
		}
	}
	deepStrictEqual(mismatched, [], `placeholder drift (renders undefined at runtime): ${mismatched.slice(0, 10).join("; ")}`);
});
