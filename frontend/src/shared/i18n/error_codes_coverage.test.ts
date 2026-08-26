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
  // 三类来源都要扫：直接 response.Error 字面量、经 writeServiceError 等
  // 中转函数传入的代码、以及 handler 内 code := "x" 变量赋值——此前只扫
  // 第一类，间接传参的 9 个代码逃过了覆盖检查（round 14 修复）。
  // 每项含主 pattern 与从整行提取代码的尾段命令：多数模式的匹配行以代码
  // 引号结尾(行尾锚定提取即可)；gin.H 信封行代码居中，需独立提取段。
  // 字符类必须含下划线——错误码存在 snake_case(server_overloaded 等)。
  const patterns: Array<{ pattern: string; extract: string; path?: string }> = [
    { pattern: 'response[.]Error[(]c, [^,]+, "[a-zA-Z]+"', extract: '"[a-zA-Z]+"$' },
    { pattern: 'write[A-Za-z]*Error[(]c, "[a-zA-Z]+"', extract: '"[a-zA-Z]+"$' },
    { pattern: 'code := "[a-zA-Z]+"', extract: '"[a-zA-Z]+"$' },
    // SSE 流式任务错误事件（round 45 补入：stream.WriteError 家族）。
    { pattern: 'riteError [(] "[a-zA-Z]+"', extract: '"[a-zA-Z]+"$' },
    // OpenAI 兼容信封的手写 gin.H 四子句行（round 63 补入：并发闸门
    // server_overloaded 与启动恢复 service_reconciling 用这种第四种构造
    // 方式，此前的三类模式都扫不到，两个代码长期缺 i18n 映射）。
    { pattern: '"code": "[a-zA-Z_]+", "message": .+, "param": nil', extract: '"[a-zA-Z_]+", "message' },
    // round 111 补入两类：inference handler 的 status/code 赋值行
    // (billing_limit_exceeded 等 9 个)与 auth 中间件 clientErrorCode 的
    // snake_case return——创意工作台用用户 Key 调 /v1/* 会拿到这些码,
    // 此前长期缺 i18n 映射导致英文界面显示中文原文。
    { pattern: 'status, code = http[.][A-Za-z]+, "[a-zA-Z_]+"', extract: '"[a-zA-Z_]+"$' },
    // clientErrorCode 的 snake_case return 只在 auth.go 内扫描——
    // 其他文件的 snake_case return 多为操作类型分类(image_edit 等),会误报。
    { pattern: 'return "[a-z]+(_[a-z]+)+"', extract: '"[a-z]+(_[a-z]+)+"$', path: "backend/internal/transport/http/middleware/auth.go" },
  ];
  const codes = new Set<string>();
  for (const { pattern, extract, path } of patterns) {
    const grepcmd = [
      "grep", "-rhoE",
      pattern,
      path ?? "backend/internal/transport/http/", "--include=*.go",
    ].map((part) => part.includes(" ") ? JSON.stringify(part) : part).join(" ");
    const tailcmd = ["grep", "-oE", extract].map((p) => JSON.stringify(p)).join(" ");
    let full = grepcmd + " | " + tailcmd + " | tr -d '" + String.fromCharCode(34) + "' | sort -u";
    if (extract.endsWith('", "message')) {
      // tr 已剥离引号，sed 按无引号形态去掉尾部 ", message" 标记。
      full = full + " | sed 's/, message$//' | sort -u";
    }
    const out = execSync(full, { cwd: repoRoot }).toString();
    for (const line of out.split("\n")) {
      // round 63: 错误码存在 snake_case(server_overloaded 等), 字符类必须
      // 含下划线, 否则即使 grep 命中也会在这里被丢弃。
      if (/^[a-zA-Z_]+$/.test(line)) codes.add(line);
    }
  }
  return codes;
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
