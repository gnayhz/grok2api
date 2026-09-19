export const experimentZh = {
	dispositions: {
		"remanded": "调查中",
		"sentenced": "已判受限",
		"released": "本案已释放",
		"withdrawn": "已撤下",
		"dismissed": "已解除",
		"unknown": "处置未知",
	},
	manualReview: "人工复核与释放",
	manualHelp:
		"填写复核原因后，解除本案施加的限制。原结论和测试保留供复盘；其他案件仍有效的限制继续保留。",
	manualReason: "复核理由（至少 3 个字符）",
	release: "记录理由并释放本案",
	manualReleased: "本案已由人工复核释放",
	intro:
		"固定出口换账号，固定账号换出口。先暂时冻结涉案资源，再通过对照测试判断问题跟随谁，并在截止时间内完成处置。",
	case: "案件 #{{id}}",
};
export const experimentEn = {
	dispositions: {
		"remanded": "Under investigation",
		"sentenced": "Restricted by verdict",
		"released": "Released by this case",
		"withdrawn": "Withdrawn",
		"dismissed": "Dismissed",
		"unknown": "Unknown disposition",
	},
	manualReview: "Review and release",
	manualHelp:
		"Record a reason to revoke this case’s restrictions. Original decisions and tests remain available. Other cases retain their restrictions.",
	manualReason: "Review reason (at least 3 characters)",
	release: "Record reason and release",
	manualReleased: "Released after manual review",
	intro:
		"Hold the account and exit, isolate one variable at a time, and settle each controlled investigation before its deadline.",
	case: "Case #{{id}}",
};
