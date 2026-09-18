export const poolsZh = {
	stratSessionReuse: "会话复用",
	stratSessionReuseDesc: "Build 新会话按出口负载分配，后续沿用合格出口；切换上游账号不改变会话绑定。无会话标识或其他通道按请求随机选择。",
	sessionIdentityHint: "同一对话请保持会话标识，新建对话时更换。prompt_cache_key 优先于 X-Session-Id；绑定在单实例内保留，重启后重新分配。强制新连接的节点仍遵守其策略。",
	emptyTitle: "按流量用途建立第一个代理池",
	emptyDescription: "将出口组成代理池，再选择调度策略和需要时的回退路径。",
	statsDescription: "选中次数属于此池；失败次数是节点跨池的累计观测，不代表此池的请求失败率。统计随进程重启清零；重置只清除此池的选中次数。",
};

export const poolsEn: Record<keyof typeof poolsZh, string> = {
	stratSessionReuse: "Session reuse",
	stratSessionReuseDesc: "Assign new Build sessions by exit load, then retain an eligible exit across upstream account changes. Requests without session identity and other providers select randomly.",
	sessionIdentityHint: "Keep the session identity within a conversation and change it for a new one. prompt_cache_key takes precedence over X-Session-Id. Bindings are local to one instance and reset on restart. Nodes requiring fresh connections retain that policy.",
	emptyTitle: "Create a pool for your traffic",
	emptyDescription: "Group exits, choose how they are selected, and add a fallback path when needed.",
	statsDescription: "Selections belong to this pool. Failures are cumulative node observations across pools, not a pool request failure rate. Restarting clears these counters; resetting clears only this pool’s selections.",
};
