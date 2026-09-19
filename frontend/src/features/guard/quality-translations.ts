// quality 文案归 feature(guard)并由 app 注册;键名保持不变。
export const qualityZh = {
        guardConfig: {
          saved: "守卫配置已保存并在本实例应用",
          reload: "重新载入已保存值",
          reset: "恢复守卫默认",
          resetTitle: "恢复守卫文件默认配置？",
          resetDescription: "将开关、管辖模型和重试参数保存为本实例文件默认值的新版本。",
          resetRotation: "恢复轮换默认",
          resetRotationTitle: "恢复出口轮换文件默认配置？",
          resetRotationDescription: "仅恢复出口轮换参数，其余运行设置保持当前已保存值。",
          selfCheckOk: "自检通过",
          selfCheckError: "自检异常:{{detail}}",
          emptySelection: "启用状态必须至少勾选一个管辖模型",
          title: "管辖模型",
          modelsCount: "{{selected}}/{{total}}",
          searchPlaceholder: "搜索模型 / 上游…",
          noMatch: "没有匹配的模型",
          staleGroup: "已勾选但不在启用路由(保留防丢)"
        },
        settingsPage: {
          backToConsole: "返回监控台",
          masterEnabled: "启用实时质量守卫",
          masterEnabledHelp: "请求路径的唯一总开关:关闭后所有模型不再质量扣留,降智响应直接交付",
          retryTitle: "请求重试与耗尽策略",
          retryHelp: "质量扣留后的重试预算与各类截止时间;预算耗尽即 fail-closed 拒绝,绝不放行降智响应",
          rotationHelp: "被 ban 出口的自动换 IP 参数;仅配置了轮换 webhook 的节点会触发,全局限速防风暴",
          jurisdictionHelp: "守卫介入的模型范围与自检状态;启用+空勾选会被拒绝"
        },
        settings: {
          help: "检测在证据足够时立即结束。可调整案件调查期限、观测保留期和统计时间范围。时长支持 45m、12h。",
          savedNote: "已生效",
          pendingNote: "设置已保存（版本 {{saved}}），正在重试应用；本实例已应用版本 {{applied}}。",
          changedNote: "设置已被其他会话更新。取消当前编辑后可载入最新设置。",
          fields: {
            investigation_timeout: "案件调查期限",
            retention: "观测保留期",
            evidence_window: "证据统计滑窗"
          },
          helps: {
            investigation_timeout: "从案件创建开始计时;期限到达后取消未完成探针并按现有证据收口,防止账号与出口无限冻结",
            retention: "原始观测数据保留期,过期清理",
            evidence_window: "证据局统计滑窗:态势统计与目标筛选只看该窗口内的观测"
          }
        }
};

export const qualityEn = {
        guardConfig: {
          saved: "Guard settings saved and applied on this instance",
          reload: "Reload saved values",
          reset: "Reset guard defaults",
          resetTitle: "Restore guard file defaults?",
          resetDescription: "Save the switch, model scope and retry parameters from this instance’s file defaults as a new revision.",
          resetRotation: "Reset rotation defaults",
          resetRotationTitle: "Restore exit rotation file defaults?",
          resetRotationDescription: "Restore only exit rotation parameters. Other saved runtime settings remain as they are.",
          selfCheckOk: "Self-check OK",
          selfCheckError: "Self-check error: {{detail}}",
          emptySelection: "Enabled guard requires at least one model",
          title: "Guarded models",
          modelsCount: "{{selected}}/{{total}}",
          searchPlaceholder: "Search model / upstream…",
          noMatch: "No matching models",
          staleGroup: "Checked but not in enabled routes (kept to avoid silent loss)"
        },
        settingsPage: {
          backToConsole: "Back to console",
          masterEnabled: "Enable realtime quality guard",
          masterEnabledHelp: "The single master switch on the request path: off means no quality holds for any model — degraded responses deliver as-is",
          retryTitle: "Request retry & exhaustion",
          retryHelp: "Retry budget and deadlines after a quality hold; exhaustion fails closed — degraded responses are never delivered",
          rotationHelp: "Auto IP-rotation for banned exits; only nodes with a rotation webhook trigger, with a global rate limit",
          jurisdictionHelp: "Model scope the guard engages, with self-check state; enabled + empty selection is rejected"
        },
        settings: {
          help: "Checks stop when evidence is sufficient. Configure investigation deadlines and record retention here. Durations accept 45m or 12h.",
          savedNote: "Applied",
          pendingNote: "Saved version {{saved}} is awaiting application; this instance has applied version {{applied}}. Retrying automatically.",
          changedNote: "Another session updated these settings. Cancel your edits to load the latest settings.",
          fields: {
            investigation_timeout: "Case investigation deadline",
            retention: "Observation retention",
            evidence_window: "Evidence stats window"
          },
          helps: {
            investigation_timeout: "Measured from case creation; unfinished probes are cancelled at the deadline and the case closes using available evidence, preventing indefinite holds",
            retention: "Raw observation retention before cleanup",
            evidence_window: "Evidence-bureau sliding window: status statistics and target selection only see observations inside it"
          }
        }
};
