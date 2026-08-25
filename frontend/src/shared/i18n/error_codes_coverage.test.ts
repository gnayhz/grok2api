import assert from "node:assert/strict";
import { execSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve as resolvePath } from "node:path";
import { test } from "node:test";

// 后端 response.Error 的错误码必须在前端 apiErrors 有本地化映射——
// 缺失时用户看到英文回退（zh 用户）或后端中文原文（en 用户）。
// 本测试扫描后端源码提取全部 code 并断言逐个覆盖，防止两侧漂移。

const repoRoot = resolvePath(import.meta.dirname, "../../../../");
const i18nSource = readFileSync(resolvePath(import.meta.dirname, "index.ts"), "utf8");

function extractBackendCodes(): Set<string> {
  // 用字面字符串拼 grep 参数，避免正则字面量里的括号被解析器吞掉。
  const grepcmd = [
    "grep", "-rhoE",
    'response[.]Error[(]c, [^,]+, "[a-zA-Z]+"',
    "backend/internal/transport/http/", "--include=*.go",
  ].map((part) => part.includes(" ") ? JSON.stringify(part) : part).join(" ");
  const tailcmd = ["grep", "-oE", '"[a-zA-Z]+"$'].map((p) => JSON.stringify(p)).join(" ");
  const full = grepcmd + " | " + tailcmd + " | tr -d '" + String.fromCharCode(34) + "' | sort -u";
  const out = execSync(full, { cwd: repoRoot }).toString();
  return new Set(out.split("\n").filter((line) => /^[a-zA-Z]+$/.test(line)));
}

function extractLocaleKeys(blockIndex: number): Set<string> {
  const starts: number[] = [];
  let cursor = 0;
  for (;;) {
    const at = i18nSource.indexOf("apiErrors: {", cursor);
    if (at === -1) break;
    starts.push(at);
    cursor = at + 1;
  }
  const start = starts[blockIndex];
  let depth = 0;
  let i = start + "apiErrors:".length;
  for (; i < i18nSource.length; i++) {
    if (i18nSource[i] === "{") depth++;
    else if (i18nSource[i] === "}") {
      depth--;
      if (depth === 0) break;
    }
  }
  const body = i18nSource.slice(start, i);
  return new Set([...body.matchAll(/\n\s+(\w+):/g)].map((m) => m[1]));
}

test("every backend error code has zh-CN and en translations", () => {
  const backend = extractBackendCodes();
  assert.ok(backend.size > 50, `backend code extraction looks wrong: ${backend.size}`);
  const zh = extractLocaleKeys(0);
  const en = extractLocaleKeys(1);
  assert.deepEqual([...zh].sort(), [...en].sort(), "zh-CN and en apiErrors must stay in sync");
  const missing = [...backend].filter((code) => !zh.has(code)).sort();
  assert.deepEqual(missing, [], `backend error codes without frontend translation: ${missing.join(", ")}`);
});
