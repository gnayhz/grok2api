// 批量任务文案归 entity 并由 app 注册:accounts 与 settings 两页共用。
export const batchTasksZh = {
        missingStrategyDescription: "仅为未关联 Grok Build 的账号执行 Device OAuth 转换；已有 Build 关联保持不变。",
        allStrategyDescription: "为全部目标重新执行 Device OAuth 转换，并创建或更新对应的 Grok Build 账号。"
};

export const batchTasksEn = {
        missingStrategyDescription: "Run Device OAuth conversion only for accounts without a Grok Build link; existing Build links remain unchanged.",
        allStrategyDescription: "Run Device OAuth conversion for every target, creating or updating the corresponding Grok Build account."
};
