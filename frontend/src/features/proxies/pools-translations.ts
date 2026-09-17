export const poolsZh = {
	emptyTitle: "按流量用途建立第一个代理池",
	emptyDescription: "将出口组成代理池，再选择调度策略和需要时的回退路径。",
	statsDescription: "选中次数属于此池；失败次数是节点跨池的累计观测，不代表此池的请求失败率。统计随进程重启清零；重置只清除此池的选中次数。",
};

export const poolsEn: Record<keyof typeof poolsZh, string> = {
	emptyTitle: "Create a pool for your traffic",
	emptyDescription: "Group exits, choose how they are selected, and add a fallback path when needed.",
	statsDescription: "Selections belong to this pool. Failures are cumulative node observations across pools, not a pool request failure rate. Restarting clears these counters; resetting clears only this pool’s selections.",
};
