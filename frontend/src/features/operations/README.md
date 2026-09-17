# 运营文案命名空间

本目录不是独立页面。`operations-translations.ts` 只提供 `ops` 文案包：共享壳层（`shared/ui/operations.tsx`）声明 `ops.parameterHelp` / `ops.unavailable` / `ops.appliedValue`，本 feature 扩展节点/订阅/池相关词条。

页面在 `features/guard`（质量案件与守护设置）和 `features/proxies`（出口节点、订阅、池、路由）。跨 feature 使用 `ops.*` 必须登记在 `app/i18n-ownership.test.ts`。
