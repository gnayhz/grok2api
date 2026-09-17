// 账号导出/额度任务文案归 feature 并由 app 注册;键名保持不变。
// en 侧此前散落在嵌套位置(靠结构巧合对齐键集),本文件统一为顶层键。
export const accountQuotaZh = {
  accountExport: {
        countDescription: "按稳定顺序分批导出，单批最多 10000 个。",
        batchProgress: "已导出 {{count}} 个账号；下一批为第 {{batch}} 批。",
        nextBatch: "导出下一批",
        batchCompleted: "本批已导出 {{count}} 个账号",
        completed: "导出完成，共 {{count}} 个账号"},
  accountQuotaReset: {
        action: "重置额度",
        description: "清除本地待重置和模型额度耗尽阻断。健康冷却、认证、风控、模型访问和质量限制继续生效，上游 Billing 与审计记录保持不变。若额度仍已耗尽，真实请求会再次将账号标记为待重置。",
        completed: "已重置 {{reset}} 个账号的本地额度状态"},
  accountQuotaTask: {
        title: "处理所选 {{count}} 个账号的额度",
        description: "选择要对所选 Grok Build 账号执行的额度任务。",
        allTitle: "处理全部账号的额度",
        allDescription: "选择要对全部已启用 Grok Build 账号执行的额度任务。",
        syncDescription: "请求上游 Billing 并更新本地额度快照、账号类型和恢复状态。",
        resetAllDescription: "清除全部已启用且认证正常的 Grok Build 账号的本地待重置和额度耗尽阻断。健康冷却、认证、风控、模型访问和质量限制继续生效，上游 Billing 与审计记录保持不变。",
        execute: "执行任务"},
};

export const accountQuotaEn = {
  accountExport: {
          "countDescription": "Exports accounts in stable order, up to 10,000 per batch.",
          "batchProgress": "Exported {{count}} accounts; the next download is batch {{batch}}.",
          "nextBatch": "Export next batch",
          "batchCompleted": "Exported {{count}} accounts in this batch",
          "completed": "Export complete: {{count}} accounts"},
  accountQuotaReset: {
          "action": "Reset quota",
          "description": "Clear local waiting-reset state and exhausted-model quota blocks. Health cooldowns, authentication, risk, model access and quality restrictions remain in effect; upstream Billing and audit history are preserved. Accounts that remain exhausted will be marked again by real traffic.",
          "completed": "Reset local quota state for {{reset}} accounts"},
  accountQuotaTask: {
          "title": "Process quota for {{count}} selected accounts",
          "description": "Choose the quota task to run for the selected Grok Build accounts.",
          "allTitle": "Process quota for all accounts",
          "allDescription": "Choose the quota task to run for all enabled Grok Build accounts.",
          "syncDescription": "Request upstream Billing and update local quota snapshots, account tiers, and recovery state.",
          "resetAllDescription": "Clear local waiting-reset and exhausted-quota blocks for all enabled Grok Build accounts with valid authentication. Health cooldowns, authentication, risk, model access and quality restrictions remain in effect; upstream Billing and audit history are preserved.",
          "execute": "Run task"},
};
