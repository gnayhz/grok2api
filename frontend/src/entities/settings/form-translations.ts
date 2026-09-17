export const settingsFormZh = {
    application: {
        "savedRevision": "已保存版本 {{revision}}",
        "appliedRevision": "本实例热应用版本 {{revision}}",
        "unknown": "本实例应用状态未知",
        "savedPending": "设置已保存，本实例仍有目标待应用",
        "pending": "本实例尚未全部应用，正在自动重试。",
        "targetPending": "待应用",
        "lastAttempt": "最近尝试 {{time}}",
        "restart": "以下设置还需重启生效：{{fields}}",
        "notification": {
            "disabled": "未配置跨实例通知；其他实例通过定期读取持久设置收敛。",
            "observed": "此版本由本实例读取；此处不代表其他实例的应用状态。",
            "pending": "变更通知待发布。",
            "published": "变更通知已发布；其他实例的应用状态未确认。",
            "failed": "变更通知发布失败，正在重试；其他实例仍会定期读取持久设置。"
        },
        "targets": {
            "inference_capacity": "推理容量",
            "batch": "批量任务",
            "provider_build": "Build 服务",
            "network": "网络",
            "provider_web": "Web 服务",
            "provider_console": "Console 服务",
            "media": "媒体管理",
            "quota_recovery": "额度恢复",
            "account_sync": "账号同步",
            "selector": "账号选择",
            "egress_rotation": "出口轮换",
            "accounts": "账号政策",
            "conversation": "会话管理",
            "gateway": "请求编排",
            "audit": "审计写入",
            "client_keys": "密钥默认限制"
        }
    },
    durationUnit: "时长单位",
    units: { seconds: "秒", minutes: "分", hours: "时", days: "天" }
};
export const settingsFormEn = {
    application: {
        "savedRevision": "Saved revision {{revision}}",
        "appliedRevision": "Hot-applied revision {{revision}} on this instance",
        "unknown": "Applied state unknown on this instance",
        "savedPending": "Settings saved; this instance still has pending targets",
        "pending": "This instance has not applied everything yet; retrying automatically.",
        "targetPending": "Pending apply",
        "lastAttempt": "Last attempt {{time}}",
        "restart": "The following settings still require a restart: {{fields}}",
        "notification": {
            "disabled": "No cross-instance notification configured; other instances converge by periodically reading persisted settings.",
            "observed": "This revision was read by this instance; it does not represent other instances' applied state.",
            "pending": "Change notification pending publication.",
            "published": "Change notification published; other instances' applied state is unconfirmed.",
            "failed": "Change notification publishing failed, retrying; other instances still read persisted settings periodically."
        },
        "targets": {
            "inference_capacity": "Inference capacity",
            "batch": "Batch tasks",
            "provider_build": "Build service",
            "network": "Network",
            "provider_web": "Web service",
            "provider_console": "Console service",
            "media": "Media management",
            "quota_recovery": "Quota recovery",
            "account_sync": "Account sync",
            "selector": "Account selection",
            "egress_rotation": "Exit rotation",
            "accounts": "Account policy",
            "conversation": "Conversation",
            "gateway": "Request orchestration",
            "audit": "Audit writes",
            "client_keys": "Key defaults"
        }
    },
    durationUnit: "Duration unit",
    units: { seconds: "Sec", minutes: "Min", hours: "Hr", days: "Day" }
};
