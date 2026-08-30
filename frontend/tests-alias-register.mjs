import { register } from "node:module";

// node:test 环境没有 DOM，而 runtime-config.ts 在模块加载时读取
// window.__GROK2API_RUNTIME_CONFIG__ 与 window.location.origin——此前
// 9 个经由客户端链导入它的测试在裸 node 里全部崩溃（“Cannot read
// properties of undefined”）。提供最小 window 桩（origin 指向本地
// 回环），纯逻辑测试即可无 DOM 运行；真实浏览器行为不受影响。
if (typeof globalThis.window === "undefined") {
  globalThis.window = {
    location: { origin: "http://127.0.0.1:3000" },
  };
}

register("./tests-alias-hooks.mjs", import.meta.url);
