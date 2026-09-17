// Console Provider 跨页标签归 entity 并由 app 注册;键名保持不变。
export const consoleProviderZh = {

        name: "Grok Console",
        accountsDescription: "分别管理 Grok Build OAuth、Grok Web SSO 与 Grok Console SSO 号池、健康状态、并发和额度。",
        syncAllDescription: "将同步所有已启用 Grok Console 账号的额度状态。",
        importFile: "导入账号文件",
        quickImportTitle: "快速导入 Grok Console 账号",
        quickImportDescription: "支持粘贴多个 Console SSO Token，或上传每行一个 Token 的 TXT 文件；重复项会自动忽略。",
        baseURL: "上游地址",
        chatTimeout: "聊天超时",
        recoveryProbeAt: "下次主动恢复探测 {{time}}"
};

export const consoleProviderEn = {

        name: "Grok Console",
        accountsDescription: "Manage separate Grok Build OAuth, Grok Web SSO, and Grok Console SSO pools, including health, concurrency, and quotas.",
        syncAllDescription: "Sync quota state for every enabled Grok Console account.",
        importFile: "Import account files",
        quickImportTitle: "Quick import Grok Console accounts",
        quickImportDescription: "Paste multiple Console SSO tokens or upload a TXT file with one token per line. Duplicate entries are ignored.",
        baseURL: "Upstream URL",
        chatTimeout: "Chat timeout",
        recoveryProbeAt: "Next active recovery probe {{time}}"
};
