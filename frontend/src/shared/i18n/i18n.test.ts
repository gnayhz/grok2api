import { deepStrictEqual } from "node:assert";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
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

/**
 * Collect source files that may call the i18n translator. Test files are
 * excluded: their `it("...")` callbacks are not translation lookups, and
 * dynamic template-literal keys (closed unions resolved in code) cannot be
 * checked statically.
 */
function listSourceFiles(dir: URL, files: string[] = []): string[] {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const child = new URL(`${entry.name}/`, dir);
		if (entry.isDirectory()) {
			if (entry.name === "node_modules" || entry.name === "dist") continue;
			listSourceFiles(child, files);
		} else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) {
			files.push(fileURLToPath(new URL(entry.name, dir)));
		}
	}
	return files;
}

// Matches both destructured t("key") and i18n.t("key") call forms where the
// key is a complete string literal argument (followed by ) or ,). The
// lookbehind rejects identifiers ending in t (split(, format() and friends)
// and member calls on other objects (.t( from non-i18n namespaces); the
// trailing [,)] rejects string concatenation (t("prefix." + suffix)) whose
// keys are resolved dynamically in code.
const staticTCall = /(?<![A-Za-z0-9_$.])(?:i18n\s*\.\s*)?t\s*\(\s*(["'])([^"\n]+)\1\s*[,)]/g;

test("every static t() key used in source has a zh-CN definition", () => {
	const srcRoot = new URL("../../", import.meta.url);
	const used = new Map<string, string>();
	for (const file of listSourceFiles(srcRoot)) {
		const source = readFileSync(file, "utf8");
		for (const match of source.matchAll(staticTCall)) {
			const key = match[2];
			if (key && !used.has(key)) used.set(key, file);
		}
	}
	const missing = [...used.entries()]
		.filter(([key]) => !zh.has(key))
		.map(([key, file]) => `${key} (${file.replace(fileURLToPath(srcRoot), "")})`);
	deepStrictEqual(missing, [], `t() keys without a definition render the raw key to users: ${missing.slice(0, 20).join(", ")}`);
});
