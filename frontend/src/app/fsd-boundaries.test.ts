import assert from "node:assert/strict";
import { existsSync, globSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const srcRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const frontendRoot = resolve(srcRoot, "..");

function walk(dir: string, acc: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) walk(path, acc);
    else if (/\.(ts|tsx)$/.test(name)) acc.push(path);
  }
  return acc;
}

function imports(file: string, text: string): string[] {
  const result: string[] = [];
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
  const visit = (node: ts.Node) => {
    let specifier: ts.Node | undefined;
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) {
      specifier = node.moduleSpecifier;
    } else if (ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword) {
      specifier = node.arguments[0];
    } else if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument)) {
      specifier = node.argument.literal;
    } else if (ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)) {
      specifier = node.moduleReference.expression;
    }
    if (specifier && (ts.isStringLiteral(specifier) || ts.isNoSubstitutionTemplateLiteral(specifier))) {
      result.push(specifier.text);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return result;
}

/**
 * Dynamic import() calls whose argument is not a string (or no-substitution
 * template) literal resolve their target at runtime, so the static scan in
 * `imports()` cannot rank them and would silently skip them. Surface them as
 * explicit failures instead: every dynamic dependency must stay visible to
 * the boundary walk (no `import(variable)`, no substitution templates, no
 * `import.meta.glob` — the latter already has its own test below).
 */
function nonLiteralDynamicImports(file: string, text: string): string[] {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
  const offenders: string[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword) {
      const argument = node.arguments[0];
      const literal = argument !== undefined && (ts.isStringLiteral(argument) || ts.isNoSubstitutionTemplateLiteral(argument));
      if (!literal) {
        const { line } = source.getLineAndCharacterOfPosition((argument ?? node).getStart(source));
        offenders.push(`${relative(srcRoot, file)}:${line + 1}`);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return offenders;
}

/**
 * Layer order, innermost (most reusable) first. A module may import its own
 * layer and every layer ranked below it; `app` composes all of them and is
 * allowed to import anything.
 *
 * The rule is default-deny: every top-level path that is NOT one of these four
 * layers is unranked leaf/entry code (`src/main.tsx`, `src/types/**`, and any
 * directory added later). Unranked code may boot `app` and reuse `shared`, but
 * it must never reach into `entities` or `features`, which is exactly the
 * bypass the previous `layer === "shared" | "entities" | "features"` switch
 * left unpoliced.
 */
const LAYER_ORDER = ["shared", "entities", "features", "app"] as const;

function layerRank(layer: string): number {
  return (LAYER_ORDER as readonly string[]).indexOf(layer);
}

function violation(file: string, specifier: string): boolean {
  const target = specifier.startsWith("@/") ? resolve(srcRoot, specifier.slice(2))
    : specifier.startsWith(".") ? resolve(dirname(file), specifier) : undefined;
  if (!target) return false;
  const [layer, feature] = relative(srcRoot, file).split(sep);
  if (layer === "app") return false;
  // A specifier that climbs out of src/ is outside every layer and cannot be
  // ranked; treat it as a boundary escape instead of silently passing.
  const targetRelative = relative(srcRoot, target);
  if (targetRelative === ".." || targetRelative.startsWith(".." + sep)) return true;
  const [targetLayer, targetFeature] = targetRelative.split(sep);
  const sourceRank = layerRank(layer);
  if (sourceRank === -1) return targetLayer === "entities" || targetLayer === "features";
  const targetRank = layerRank(targetLayer);
  // Unranked targets are ambient declarations and leaf helpers, never a layer.
  if (targetRank === -1) return false;
  // Strictly higher layers (closer to app) are denied, `app` included.
  if (targetRank > sourceRank) return true;
  // Siblings inside the features layer stay isolated from each other.
  return layer === "features" && targetLayer === "features" && feature !== targetFeature;
}

describe("FSD boundaries", () => {
  it("checks relative, alias, side-effect, dynamic, type and re-export dependencies", () => {
    const file = join(srcRoot, "features/accounts/example.ts");
    const source = `
      import { Page } from "../settings/page";
      import "@/features/guard/styles.css";
      export { data } from "../../app/data";
      const page = import("../settings/page");
      type PageType = import("../settings/page").Page;
      // import "@/features/ignored/example";
      const text = 'import "@/features/ignored/example"';
    `;
    const dependencies = imports(file, source);
    assert.equal(dependencies.length, 5);
    assert.ok(dependencies.every((specifier) => violation(file, specifier)));
    assert.equal(violation(file, "./local"), false);
    assert.equal(violation(file, "@/entities/account/types"), false);
    assert.equal(violation(join(srcRoot, "shared/ui/example.ts"), "../../entities/account/types"), true);
  });

  it("keeps shared, entities, features and app dependencies directed inward", () => {
    const files = walk(srcRoot);
    assert.equal(files.some((path) => relative(srcRoot, path).startsWith("components" + sep + "ui")), false);
    const violations: string[] = [];
    for (const file of files) {
      for (const specifier of imports(file, readFileSync(file, "utf8"))) {
        if (violation(file, specifier)) violations.push(relative(srcRoot, file) + ": " + specifier);
      }
    }
    assert.deepEqual(violations, []);
  });

  it("denies higher layers from every top-level path that is not a known layer", () => {
    // Previously unpoliced: a file under src/types/ reaching into entities.
    assert.equal(violation(join(srcRoot, "types/x.ts"), "../../entities/account/account-api"), true);
    assert.equal(violation(join(srcRoot, "types/x.ts"), "../entities/account/account-api"), true);
    // Previously unpoliced: the entry point reaching into entities.
    assert.equal(violation(join(srcRoot, "main.tsx"), "@/entities/account/account-api"), true);
    // Previously unpoliced: a brand new top-level directory reaching into a feature.
    assert.equal(violation(join(srcRoot, "utils/helper.ts"), "@/features/guard/quality-view"), true);
    // Unranked code may still boot the app composition and reuse shared code.
    assert.equal(violation(join(srcRoot, "main.tsx"), "@/app/router"), false);
    assert.equal(violation(join(srcRoot, "main.tsx"), "@/shared/i18n"), false);
    assert.equal(violation(join(srcRoot, "utils/helper.ts"), "@/shared/lib/cn"), false);
    // The app layer stays free to compose every layer.
    assert.equal(violation(join(srcRoot, "app/example.ts"), "@/features/guard/quality-view"), false);
    assert.equal(violation(join(srcRoot, "app/example.ts"), "@/entities/account/account-api"), false);
    assert.equal(violation(join(srcRoot, "app/example.ts"), "@/shared/ui/operations"), false);
    // Cross-feature and upward imports inside the ranked layers stay denied.
    assert.equal(violation(join(srcRoot, "features/accounts/example.ts"), "@/features/guard/quality-view"), true);
    assert.equal(violation(join(srcRoot, "features/accounts/example.ts"), "@/app/router"), true);
    assert.equal(violation(join(srcRoot, "entities/account/example.ts"), "@/features/guard/quality-view"), true);
    assert.equal(violation(join(srcRoot, "entities/account/example.ts"), "@/app/router"), true);
    assert.equal(violation(join(srcRoot, "shared/ui/example.ts"), "@/entities/account/account-api"), true);
    assert.equal(violation(join(srcRoot, "shared/ui/example.ts"), "@/app/router"), true);
  });

  it("rejects import.meta.glob so dynamic imports cannot bypass the boundary walk", () => {
    // Test files legitimately mention the token in fixtures/assertions; only
    // runtime sources must not use Vite's glob importer, whose specifiers are
    // invisible to the static import scan above.
    const offenders = walk(srcRoot)
      .filter((path) => !/\.test\.tsx?$/.test(path))
      .filter((path) => readFileSync(path, "utf8").includes("import.meta.glob"))
      .map((path) => relative(srcRoot, path));
    assert.deepEqual(offenders, [], `import.meta.glob bypasses boundary checks: ${offenders.join(", ")}`);
  });

  it("rejects dynamic import() calls whose argument is not a string literal", () => {
    // The detector must fire on a computed specifier...
    const demo = nonLiteralDynamicImports(join(srcRoot, "features/accounts/example.ts"), `
      const target = "../settings/page";
      const page = import(target);
    `);
    assert.equal(demo.length, 1);
    // ...and a literal dynamic import stays clean.
    assert.deepEqual(nonLiteralDynamicImports(join(srcRoot, "app/page-modules.ts"), 'const m = import("@/features/guard/quality-console");'), []);
    // No source file may resolve a dynamic import at runtime: the static
    // boundary walk cannot rank a target it never sees.
    const offenders = walk(srcRoot).flatMap((file) => nonLiteralDynamicImports(file, readFileSync(file, "utf8")));
    assert.deepEqual(offenders, [], `dynamic import() with a non-literal argument escapes the boundary walk: ${offenders.join(", ")}`);
  });
});

/**
 * eslint only lints what its `ignores` list does not exclude. A path that was
 * renamed or deleted keeps matching nothing, so the exemption silently grows
 * stale while the file it used to cover is no longer linted. Assert every
 * listed path still exists. `dist` is build output rather than source: eslint
 * must ignore it, but it legitimately does not exist before the first build.
 */
const BUILD_OUTPUT_IGNORES = new Set(["dist"]);

function eslintIgnores(): string[] {
  const configPath = join(frontendRoot, "eslint.config.js");
  const source = ts.createSourceFile(configPath, readFileSync(configPath, "utf8"), ts.ScriptTarget.Latest, true);
  const entries: string[] = [];
  const visit = (node: ts.Node) => {
    if (
      ts.isPropertyAssignment(node) &&
      ts.isIdentifier(node.name) &&
      node.name.text === "ignores" &&
      ts.isArrayLiteralExpression(node.initializer)
    ) {
      for (const element of node.initializer.elements) {
        if (ts.isStringLiteral(element)) entries.push(element.text);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return entries;
}

describe("eslint ignore list", () => {
  it("only ignores paths that exist, so a stale entry cannot un-lint a file", () => {
    const ignores = eslintIgnores();
    assert.ok(ignores.length > 0, "eslint.config.js declares no ignore list to verify");
    assert.deepEqual(ignores, [...new Set(ignores)], "eslint ignore entries must be unique");
    const missing = ignores
      .filter((entry) => !BUILD_OUTPUT_IGNORES.has(entry))
      .filter((entry) => (/[*?[\]]/.test(entry) ? globSync(entry, { cwd: frontendRoot }).length === 0 : !existsSync(join(frontendRoot, entry))));
    assert.deepEqual(missing, [], `eslint ignore entries no longer exist: ${missing.join(", ")}`);
  });
});
