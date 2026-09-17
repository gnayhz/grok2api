// node 测试运行器的 "@/" 别名解析钩子（vite/tsconfig paths 的 runner 侧等价）。
import { fileURLToPath, pathToFileURL } from "node:url";
import { resolve as resolvePath } from "node:path";

const root = resolvePath(fileURLToPath(new URL(".", import.meta.url)));

export async function resolve(specifier, context, next) {
  if (specifier.startsWith("@/") || (/^\.\.?\//.test(specifier) && !/\.[a-z]+$/i.test(specifier))) {
    const base = specifier.startsWith("@/") ? pathToFileURL(resolvePath(root, "src", specifier.slice(2))).href : new URL(specifier, context.parentURL).href;
    // "@/" specifiers may already carry an extension (e.g. "@/entities/audit/audit-api.ts"),
    // mirroring vite/tsconfig paths resolution which accepts both forms.
    const candidates = /\.[a-z]+$/i.test(specifier) ? [base] : [base + ".ts", base + ".tsx", base + "/index.ts"];
    for (const candidate of candidates) {
      try {
        return await next(candidate, context);
      } catch {
        /* try next form */
      }
    }
  }
  return next(specifier, context);
}

// Exercise the real React providers in node:test. Type checking remains the
// responsibility of tsc; this loader only emits JSX for the DOM harness.
export async function load(url, context, next) {
  // DOM contract tests render full pages; CSS is exercised by browser checks.
  if (url.endsWith(".css")) return { format: "module", source: "", shortCircuit: true };
  if (url.startsWith("file:") && url.endsWith(".tsx")) {
    const [{ readFile }, ts] = await Promise.all([import("node:fs/promises"), import("typescript")]);
    const source = await readFile(new URL(url), "utf8");
    const result = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, jsx: ts.JsxEmit.ReactJSX }, fileName: fileURLToPath(url) });
    return { format: "module", source: result.outputText, shortCircuit: true };
  }
  return next(url, context);
}
