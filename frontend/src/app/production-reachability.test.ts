import assert from "node:assert/strict";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const srcRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const entry = join(srcRoot, "main.tsx");

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

// Vite-style resolution for extension-less specifiers, restricted to modules
// the bundler would actually include (TS sources and their index files).
function resolveSpecifier(fromFile: string, specifier: string): string | undefined {
  if (specifier.startsWith("@/")) {
    const base = join(srcRoot, specifier.slice(2));
    const candidates = [base, `${base}.ts`, `${base}.tsx`, join(base, "index.ts"), join(base, "index.tsx")];
    return candidates.find((candidate) => existsSync(candidate) && statSync(candidate).isFile());
  }
  if (specifier.startsWith(".")) {
    const base = resolve(dirname(fromFile), specifier);
    const candidates = [base, `${base}.ts`, `${base}.tsx`, join(base, "index.ts"), join(base, "index.tsx")];
    return candidates.find((candidate) => existsSync(candidate) && statSync(candidate).isFile());
  }
  return undefined;
}

// Old views kept only until a remediation task removes them after verifying
// feature parity. An entry that becomes reachable again is stale and must
// leave this list. R18 removed the six legacy proxy/guard views; the list
// stays as the mechanism for any future transitional exception.
const r18TransitionalUnreachable = new Set<string>([]);

describe("production reachability", () => {
  it("keeps every production module reachable from the real entry", () => {
    assert.ok(existsSync(entry), "src/main.tsx must exist as the production entry");
    const reachable = new Set<string>();
    const queue = [entry];
    while (queue.length > 0) {
      const file = queue.pop()!;
      if (reachable.has(file)) continue;
      reachable.add(file);
      for (const specifier of imports(file, readFileSync(file, "utf8"))) {
        const resolved = resolveSpecifier(file, specifier);
        if (resolved && !reachable.has(resolved)) queue.push(resolved);
      }
    }

    const productionFiles = walk(srcRoot).filter((file) => {
      const rel = relative(srcRoot, file);
      if (/\.test\.(ts|tsx)$/.test(rel)) return false;
      if (rel.endsWith("test-fixture.ts")) return false;
      // Ambient declaration files are consumed by the compiler through
      // tsconfig, not through import edges; tsc owns their validity.
      if (rel.endsWith(".d.ts")) return false;
      return true;
    });

    const unreachable = productionFiles
      .map((file) => relative(srcRoot, file))
      .filter((rel) => !reachable.has(join(srcRoot, rel)))
      .filter((rel) => !r18TransitionalUnreachable.has(rel))
      .sort();
    assert.deepEqual(
      unreachable,
      [],
      "production modules must be reachable from src/main.tsx (route entries are imported through the router); " +
        "if a file is intentionally kept until R18, bind it to the transitional list with its removal condition",
    );

    const stale = [...r18TransitionalUnreachable]
      .filter((rel) => reachable.has(join(srcRoot, rel)))
      .sort();
    assert.deepEqual(stale, [], "R18 transitional entries became reachable again; remove them from the list");
  });

  it("treats only real specifiers as edges", () => {
    // A commented-out or string-embedded import is not an edge; the walker must
    // not invent reachability from prose.
    const file = join(srcRoot, "features/example/example.ts");
    const source = [
      '// import "@/features/legacy/old-view";',
      `const text = 'import("@/features/legacy/old-view")';`,
      'import { helper } from "./helper";',
    ].join("\n");
    const specifiers = imports(file, source);
    assert.deepEqual(specifiers, ["./helper"]);
    assert.equal(resolveSpecifier(file, "./helper"), undefined);
    assert.notEqual(resolveSpecifier(file, "@/app/router"), undefined);
  });
});
