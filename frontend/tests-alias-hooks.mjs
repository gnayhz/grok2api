// node 测试运行器的 "@/" 别名解析钩子（vite/tsconfig paths 的 runner 侧等价）。
import { fileURLToPath, pathToFileURL } from "node:url";
import { resolve as resolvePath } from "node:path";

const root = resolvePath(fileURLToPath(new URL(".", import.meta.url)));

export async function resolve(specifier, context, next) {
  if (specifier.startsWith("@/")) {
    const base = pathToFileURL(resolvePath(root, "src", specifier.slice(2))).href;
    for (const candidate of [base + ".ts", base + ".tsx", base + "/index.ts"]) {
      try {
        return await next(candidate, context);
      } catch {
        /* try next form */
      }
    }
  }
  return next(specifier, context);
}
