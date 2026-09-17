// quality 文案归 feature(guard)并由 app 注册;键名保持不变。
export const qualityZh = {
        tribunal: {
          waitingReason: {
            deadline_reached: "调查期限已到,未完成探针将被取消并收口",
            awaiting_probes: "等待本轮取证探针结论({{pending}} 个在飞)",
            replacing_failed_exits: "已排除 {{failed}} 个差分失败出口,等待补充未测试出口",
            awaiting_supplement: "有效差分不足({{current}}/{{target}}),等待补充未测试出口",
            probes_settled: "探针已结束,账号置信度 {{accountPercent}}% / 出口置信度 {{exitPercent}}%"
          }
        },
        bureau: {
          colId: "任务",
          colDirection: "方向",
          colCase: "案件",
            colParties: "取证对象 / 路径",
          colResult: "结论",
          colDuration: "耗时",
          colCreated: "创建"
        },
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
          help: "先落库后热应用,保存后新案件使用新规则，已有案件保留立案时规则;时长格式如 45m/12h",
          savedNote: "已生效",
          pendingNote: "设置已保存（版本 {{saved}}），正在重试应用；本实例已应用版本 {{applied}}。",
          changedNote: "设置已被其他会话更新。取消当前编辑后可载入最新设置。",
          capacityProjection: "当前轮换总容量：每小时 {{count}} 次，可在出口轮换设置中修改。",
          feasibility: {
            kOverN: "降票数({{k}})不得超过见证人数({{n}})——否则出口定罪永不可达",
            spanOverExits: "跨节点数({{span}})不得超过降智出口数({{exits}})——否则账号定罪永不可达",
            jurorsBelowWitnesses: "每出口陪审员数({{jurors}})不得少于出口见证门槛({{n}})——否则出口定罪永不可达",
            budgetBelowCoreProbes: "每案探针预算({{budget}})不得小于差分({{differential}})+陪审({{jurors}})——否则一轮调查无法覆盖核心任务",
          },
          fields: {
            account_need_exits: "账号归因·差分有效样本目标",
            account_span_nodes: "账号归因·降智节点观察目标",
            exit_need_n: "出口定罪·见证账号数",
            exit_need_k: "出口定罪·降智见证数",
            differential_exits: "差分探测出口数",
            jurors_per_exit: "每出口陪审员数",
            probe_budget: "每案探针预算",
            investigation_timeout: "案件调查期限",
            max_rotations_per_hour: "轮换限速(次/时)",
            retention: "观测保留期",
            evidence_window: "证据统计滑窗"
          },
          helps: {
            account_need_exits: "至少在这些独立出口上复现账号异常，并由同路径正常账号验证；单次结果不能定性",
            account_span_nodes: "用于解释账号差分覆盖范围的物理节点目标,不是失败路径的替代票",
            exit_need_n: "出口定罪需要 ≥N 个账号在该出口上有观测(陪审团规模)",
            exit_need_k: "出口定罪需要其中 ≥N 个账号观测到降智(降票门槛)",
            differential_exits: "首轮差分取证派发的对照出口数;失败后仍会在上限内补测",
            jurors_per_exit: "每个受审出口同时上场的陪审员账号数",
            probe_budget: "首轮测试关系数量；异常测试最多增加一次匹配对照请求，补测有独立有限上限",
            investigation_timeout: "从案件创建开始计时;期限到达后取消未完成探针并按现有证据收口,防止账号与出口无限冻结",
            max_rotations_per_hour: "全所每小时主动轮换上限(同步出口层全局限速,单一旋钮),风暴排队保护",
            retention: "原始观测数据保留期,过期清理",
            evidence_window: "证据局统计滑窗:态势统计与目标筛选只看该窗口内的观测"
          }
        }
};

export const qualityEn = {
        tribunal: {
          waitingReason: {
            deadline_reached: "Investigation deadline reached; unfinished probes will be cancelled and the case closed",
            awaiting_probes: "Waiting for this round of probes ({{pending}} in flight)",
            replacing_failed_exits: "{{failed}} failed differential exits excluded; waiting for untested replacements",
            awaiting_supplement: "Admissible differentials short ({{current}}/{{target}}); waiting for untested exits",
            probes_settled: "Probes settled: account confidence {{accountPercent}}% / exit confidence {{exitPercent}}%"
          }
        },
        bureau: {
          colId: "Task",
          colDirection: "Direction",
          colCase: "Case",
          colParties: "Subjects / paths",
          colResult: "Result",
          colDuration: "Took",
          colCreated: "Created"
        },
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
          help: "Persisted then hot-applied on save; durations like 45m/12h",
          savedNote: "Applied",
          pendingNote: "Saved version {{saved}} is awaiting application; this instance has applied version {{applied}}. Retrying automatically.",
          changedNote: "Another session updated these settings. Cancel your edits to load the latest settings.",
          capacityProjection: "Current total rotation capacity: {{count}} per hour. Change it in the rotation settings.",
          feasibility: {
            kOverN: "Degraded votes ({{k}}) must not exceed witnesses ({{n}}) — exit conviction would be unreachable",
            spanOverExits: "Span nodes ({{span}}) must not exceed degraded exits ({{exits}}) — account conviction would be unreachable",
            jurorsBelowWitnesses: "Jurors per exit ({{jurors}}) must be at least the witness threshold ({{n}}) — exit conviction would be unreachable",
            budgetBelowCoreProbes: "Probe budget ({{budget}}) must cover differential ({{differential}}) + jury ({{jurors}}) tasks — the core investigation round would be incomplete",
          },
          fields: {
            account_need_exits: "Attribution · differential target",
            account_span_nodes: "Attribution · degraded node target",
            exit_need_n: "Exit conviction · witnesses",
            exit_need_k: "Exit conviction · degraded witnesses",
            differential_exits: "Differential probe exits",
            jurors_per_exit: "Jurors per exit",
            probe_budget: "Probe budget per case",
            investigation_timeout: "Case investigation deadline",
            max_rotations_per_hour: "Rotation rate limit (per hour)",
            retention: "Observation retention",
            evidence_window: "Evidence stats window"
          },
          helps: {
            account_need_exits: "Minimum independent paths reproducing account degradation, each with a clean matched control; a single result cannot convict",
            account_span_nodes: "Target for explaining physical-node coverage; it is not a substitute for failed evidence",
            exit_need_n: "Exit conviction needs observations from ≥N accounts (jury size)",
            exit_need_k: "Exit conviction needs ≥N of those accounts observing degradation (vote threshold)",
            differential_exits: "Control exits dispatched in the core round; failed paths may be replaced within a bound",
            jurors_per_exit: "Juror accounts fielded simultaneously per exit under review",
            probe_budget: "Core probe budget per case (replacements have a separate finite bound)",
            investigation_timeout: "Measured from case creation; unfinished probes are cancelled at the deadline and the case closes using available evidence, preventing indefinite holds",
            max_rotations_per_hour: "Enforcement-wide hourly rotation cap (synced with the egress-layer global limit — one knob); storm protection",
            retention: "Raw observation retention before cleanup",
            evidence_window: "Evidence-bureau sliding window: status statistics and target selection only see observations inside it"
          }
        }
};
