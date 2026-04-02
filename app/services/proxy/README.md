# Proxy Domain

这个目录是新的统一代理 / Cloudflare 凭据域。

当前阶段：

- 已建立统一模型
- 已建立配置归一化入口
- 已建立运行时 `acquire / report / release`
- 已区分 `egress` 与 `clearance`
- 已提供 `manual` 和 `flaresolverr managed` 两类 clearance provider
- 已替换 reverse 链路中的旧 `proxy_pool` 直接调用
- 已替换旧 `headers/session` 直读配置逻辑
- 已接管 FlareSolverr 的后台预热调度

设计基线见：

- `/Users/project/mine/grok2api/docs/proxy-architecture.md`
