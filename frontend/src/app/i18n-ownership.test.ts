import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const srcRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

type Tree = { [key: string]: string | Tree };

const languages = ["zh-CN", "en"] as const;
type Language = (typeof languages)[number];

// register-feature-i18n.ts deep-merges every feature bundle into the same
// `resources` object that shared/i18n exports (i18next's addResourceBundle
// mutates the store in place), so the base catalog has to be snapshotted
// before that module is evaluated. Dynamic imports keep that order explicit.
const { i18nResources } = await import("@/shared/i18n");
const baseTranslation = Object.fromEntries(
	languages.map((language) => [language, structuredClone(i18nResources[language].translation) as unknown as Tree]),
) as Record<Language, Tree>;
const { featureTranslationBundles } = await import("./register-feature-i18n.ts");
const bundles = featureTranslationBundles as unknown as Record<Language, Record<string, Tree>>;

function collectLeaves(node: Tree, prefix: string, out: Set<string>): Set<string> {
	for (const [key, value] of Object.entries(node)) {
		const path = prefix ? `${prefix}.${key}` : key;
		if (typeof value === "string") out.add(path);
		else collectLeaves(value, path, out);
	}
	return out;
}

/**
 * Maps every feature/entity-owned namespace to the directory that declares it,
 * parsed from the registration table so the mapping cannot drift from the real
 * bundle list. Base namespaces are the top-level keys of the base catalog and
 * are usable by every layer.
 */
function declaredNamespaceOwners(): Map<string, string> {
	const registerPath = join(srcRoot, "app/register-feature-i18n.ts");
	const source = ts.createSourceFile(registerPath, readFileSync(registerPath, "utf8"), ts.ScriptTarget.Latest, true);
	const moduleOf = new Map<string, string>();
	let table: ts.ObjectLiteralExpression | undefined;
	const visit = (node: ts.Node): void => {
		if (
			ts.isImportDeclaration(node) &&
			ts.isStringLiteral(node.moduleSpecifier) &&
			node.importClause?.namedBindings &&
			ts.isNamedImports(node.importClause.namedBindings)
		) {
			for (const element of node.importClause.namedBindings.elements) {
				moduleOf.set(element.name.text, node.moduleSpecifier.text);
			}
		}
		if (
			ts.isVariableDeclaration(node) &&
			ts.isIdentifier(node.name) &&
			node.name.text === "featureTranslationBundles" &&
			node.initializer &&
			ts.isObjectLiteralExpression(node.initializer)
		) {
			table = node.initializer;
		}
		ts.forEachChild(node, visit);
	};
	visit(source);
	assert.ok(table, "register-feature-i18n.ts must declare the featureTranslationBundles table");
	const owners = new Map<string, string>();
	for (const languageEntry of table.properties) {
		if (!ts.isPropertyAssignment(languageEntry) || !ts.isObjectLiteralExpression(languageEntry.initializer)) continue;
		for (const entry of languageEntry.initializer.properties) {
			if (!ts.isPropertyAssignment(entry)) continue;
			const namespace = ts.isIdentifier(entry.name) || ts.isStringLiteral(entry.name) ? entry.name.text : undefined;
			if (!namespace) continue;
			const root = ts.isPropertyAccessExpression(entry.initializer) ? entry.initializer.expression : entry.initializer;
			const module = ts.isIdentifier(root) ? moduleOf.get(root.text) : undefined;
			if (!module) continue;
			owners.set(namespace, module.replace(/^@\//, "").split("/").slice(0, 2).join("/"));
		}
	}
	return owners;
}

const namespaceOwners = declaredNamespaceOwners();

// Leaf key -> declaring owners. A namespace can be declared twice (`ops` and
// `proxies` exist in both the base catalog and a feature bundle); the owner set
// records both, so a co-declared namespace does not make feature-only keys
// reusable by shared code.
const keyOwners = new Map<string, Set<string>>();
function addOwner(key: string, owner: string): void {
	const owners = keyOwners.get(key) ?? new Set<string>();
	owners.add(owner);
	keyOwners.set(key, owners);
}
for (const key of collectLeaves(baseTranslation["zh-CN"], "", new Set())) addOwner(key, "base");
for (const language of languages) {
	for (const [namespace, bundle] of Object.entries(bundles[language])) {
		const owner = namespaceOwners.get(namespace);
		assert.ok(owner, `namespace "${namespace}" has no declaring directory in register-feature-i18n.ts`);
		// Bundle files are flat: their top-level keys are the namespace's keys,
		// so the declaring namespace is the path prefix.
		for (const key of collectLeaves(bundle, namespace, new Set())) addOwner(key, owner);
	}
}

/**
 * Feature-to-feature couplings recorded instead of hidden. Each entry covers
 * one consumer directory; an exact key only allows that key, a `prefix.*` entry
 * allows the whole subtree (used where the consumer renders a complete settings
 * section or builds keys dynamically). Nothing else may be added silently: a
 * new key outside these entries fails the test, and a stale entry fails too.
 *
 * `ops` is the one namespace shared code may consume: `src/shared/i18n/index.ts`
 * declares `ops.parameterHelp` / `ops.unavailable` / `ops.appliedValue` as the
 * shared fallbacks that `src/shared/ui/operations.tsx` renders, while
 * features/operations extends the same namespace with the feature-owned
 * vocabulary. Ownership is tracked per key, so shared keeps to the base-declared
 * three and every other `ops.*` consumer is recorded below.
 */
const CROSS_FEATURE_ALLOWLIST: Record<string, Array<{ allow: string[]; reason: string }>> = {
	"features/accounts": [
		{
			allow: ["models.providerGrokBuild", "models.providerGrokWeb"],
			reason: "account rows label the provider of the account DTO with the models feature's provider names",
		},
		{
			allow: ["settings.invalidValue", "settings.egress.cloudflareCookie", "settings.egress.keepConfigured"],
			reason: "account import and egress dialogs reuse the settings feature's validation and cookie-option copy",
		},
	],
	"features/audits": [
		{
			allow: ["network.pulseLive", "network.refresh"],
			reason: "live badge and refresh tooltip vocabulary published by the proxies feature; audits has no equivalent copy",
		},
		{
			allow: ["settings.guardStats.exempts.*"],
			reason: "audit details render the quality-exemption reason vocabulary owned by settings.guardStats",
		},
	],
	"features/dashboard": [
		{
			allow: ["audits.averageFirstToken", "audits.averageOutputSpeed", "audits.performancePending", "audits.throughputPending"],
			reason: "dashboard metrics are computed from audit aggregates and reuse the audits feature's labels",
		},
		{
			allow: ["models.providerGrokBuild", "models.providerGrokWeb"],
			reason: "provider distribution labels the provider DTO with the models feature's names",
		},
	],
	"features/guard": [
		{
			allow: ["ops.*"],
			reason:
				"the quality console renders the shared operations vocabulary; several lookups are dynamic (ops.${status}, ops.${verdictKey(...)}), so the whole prefix is recorded",
		},
		{
			allow: ["settings.requestRetry.*", "settings.egressRotation.*", "settings.guardStats.*", "settings.resetToDefaultsConfirm"],
			reason: "quality settings and tribunal pages render the settings feature's retry, rotation and guard-status sections",
		},
		{
			allow: ["network.refresh"],
			reason: "refresh tooltip vocabulary published by the proxies feature",
		},
	],
	"features/proxies": [
		{
			allow: [
				"ops.applyDraft",
				"ops.netDeleteSource",
				"ops.netDeleteSourceHelp",
				"ops.netDirect",
				"ops.netKeepStoredAddress",
				"ops.netLastImported",
				"ops.netSourceAuto",
				"ops.netSourceExample",
				"ops.netSourcePaused",
				"ops.netSourcesHelp",
				"ops.netSyncFailed",
				"ops.network",
				"ops.poolTab",
				"ops.probeIntervalInvalid",
				"ops.subscriptionInvalid",
			],
			reason: "proxy pages render the shared operations vocabulary (subscription/pool/probe controls)",
		},
		{
			allow: ["settings.egress.*"],
			reason: "proxy subscription and probe dialogs render the egress settings vocabulary",
		},
	],
	"features/settings": [
		{
			allow: ["models.providerGrokBuild"],
			reason: "the Build settings tab is labelled with the models feature's provider name",
		},
	],
};

function ownsKey(consumer: string, key: string): boolean {
	const owners = keyOwners.get(key);
	if (!owners) return true; // Undefined keys are i18n.test.ts's concern, not ownership's.
	if (consumer === "shared") return owners.has("base");
	const feature = consumer.slice("features/".length);
	return [...owners].some((owner) => owner === "base" || owner === `features/${feature}` || owner.startsWith("entities/"));
}

function allowEntryFor(consumer: string, key: string): string | undefined {
	for (const { allow } of CROSS_FEATURE_ALLOWLIST[consumer] ?? []) {
		for (const entry of allow) {
			if (entry === key || (entry.endsWith(".*") && key.startsWith(entry.slice(0, -1)))) return entry;
		}
	}
	return undefined;
}

type Usage = { kind: "exact" | "prefix"; value: string; line: number };

function usagesIn(source: ts.SourceFile): Usage[] {
	const usages: Usage[] = [];
	const lineOf = (position: number) => source.getLineAndCharacterOfPosition(position).line + 1;
	const visit = (node: ts.Node): void => {
		if (ts.isCallExpression(node)) {
			const callee = node.expression;
			const isTranslator =
				(ts.isIdentifier(callee) && callee.text === "t") ||
				(ts.isPropertyAccessExpression(callee) && ts.isIdentifier(callee.expression) && callee.expression.text === "i18n" && callee.name.text === "t");
			if (isTranslator && node.arguments.length > 0) {
				const argument = node.arguments[0];
				const line = lineOf(argument.getStart(source));
				if (ts.isStringLiteral(argument)) usages.push({ kind: "exact", value: argument.text, line });
				else if (ts.isTemplateExpression(argument)) {
					const head = /^([A-Za-z][\w-]*(?:\.[\w-]+)*\.)$/.exec(argument.head.text);
					if (head) usages.push({ kind: "prefix", value: head[1].slice(0, -1), line });
				} else if (ts.isBinaryExpression(argument) && argument.operatorToken.kind === ts.SyntaxKind.PlusToken && ts.isStringLiteral(argument.left)) {
					usages.push({ kind: "prefix", value: argument.left.text.replace(/\.$/, ""), line });
				} else if (ts.isConditionalExpression(argument)) {
					for (const branch of [argument.whenTrue, argument.whenFalse]) {
						if (ts.isStringLiteral(branch)) usages.push({ kind: "exact", value: branch.text, line });
					}
				}
			}
		}
		// Dotted literals are keys too: data tables and helper maps (e.g. the
		// status -> label maps in the proxy views) hold keys that dynamic
		// lookups later pass to t().
		if (ts.isStringLiteral(node)) usages.push({ kind: "exact", value: node.text, line: lineOf(node.getStart(source)) });
		ts.forEachChild(node, visit);
	};
	visit(source);
	return usages;
}

function listSourceFiles(dir: string, files: string[] = []): string[] {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const path = join(dir, entry.name);
		if (entry.isDirectory()) {
			if (entry.name === "node_modules" || entry.name === "dist") continue;
			listSourceFiles(path, files);
		} else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) {
			files.push(path);
		}
	}
	return files;
}

// Translation bundles declare every key as a property name; scanning them would
// mark the whole catalog as "used" by itself.
const bundleFiles = new Set(["shared/i18n/index.ts"]);
for (const file of listSourceFiles(srcRoot)) {
	if (/-translations\.ts$/.test(file)) bundleFiles.add(relative(srcRoot, file));
}

describe("i18n namespace ownership", () => {
	it("keeps shared on the base catalog and features on their own, base and entity namespaces", () => {
		const violations: string[] = [];
		const matchedEntries = new Set<string>();
		const catalogKeys = [...keyOwners.keys()];
		for (const file of listSourceFiles(srcRoot)) {
			const rel = relative(srcRoot, file);
			if (bundleFiles.has(rel)) continue;
			const [layer, feature] = rel.split("/");
			if (layer !== "shared" && layer !== "features") continue;
			const consumer = layer === "shared" ? "shared" : `features/${feature}`;
			const ownersOf = (key: string) => [...(keyOwners.get(key) ?? [])].sort().join(", ") || "unknown";
			for (const usage of usagesIn(ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true))) {
				if (usage.kind === "exact") {
					if (ownsKey(consumer, usage.value)) continue;
					const entry = allowEntryFor(consumer, usage.value);
					if (entry) matchedEntries.add(`${consumer}:${entry}`);
					else violations.push(`${rel}:${usage.line} uses ${usage.value} [owned by ${ownersOf(usage.value)}]`);
					continue;
				}
				const foreign = catalogKeys.filter((key) => key.startsWith(usage.value + ".") && !ownsKey(consumer, key));
				if (foreign.length === 0) continue;
				const entry = allowEntryFor(consumer, usage.value + ".*");
				if (entry) {
					matchedEntries.add(`${consumer}:${entry}`);
					continue;
				}
				violations.push(
					`${rel}:${usage.line} uses ${usage.value}.* [${foreign.length} foreign keys, e.g. ${foreign[0]} owned by ${ownersOf(foreign[0])}]`,
				);
			}
		}
		const stale = Object.entries(CROSS_FEATURE_ALLOWLIST).flatMap(([consumer, entries]) =>
			entries.flatMap(({ allow }) => allow.filter((entry) => !matchedEntries.has(`${consumer}:${entry}`)).map((entry) => `${consumer}: ${entry}`)),
		);
		assert.deepEqual(
			violations,
			[],
			`shared/feature code consumed i18n copy it does not own; hoist generic strings into the base catalog or record the coupling in CROSS_FEATURE_ALLOWLIST:\n${violations.join("\n")}`,
		);
		assert.deepEqual(stale, [], `CROSS_FEATURE_ALLOWLIST entries no longer match any usage; remove or fix them:\n${stale.join("\n")}`);
	});
});
