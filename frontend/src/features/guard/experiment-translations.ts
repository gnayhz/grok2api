export const experimentZh = {
	manualReview: "人工复核与释放",
	manualHelp:
		"填写复核原因后，解除本案施加的限制。原结论和测试保留供复盘；其他案件仍有效的限制继续保留。",
	manualReason: "复核理由（至少 3 个字符）",
	release: "记录理由并释放本案",
	manualReleased: "本案已由人工复核释放",
	title: "质量归因案件",
	intro:
		"固定出口换账号，固定账号换出口。先暂时冻结涉案资源，再通过对照测试判断问题跟随谁，并在截止时间内完成处置。",
	review: "立即评估",
	steps: {
		protect: "保护服务 · 暂时冻结",
		compare: "隔离变量 · 两组对照",
		explain: "检查支持与反证",
		finish: "限时结案 · 明确处置",
	},
	filter: {
		all: "全部案件",
		open: "调查中",
		closed: "已结案",
	},
	search: "搜索账号、出口、IP 或案件号",
	empty: "没有符合条件的案件。质量异常触发后，调查过程和结论会出现在这里。",
	verdict: {
		investigating: "调查中，双方暂时冻结",
		account_guilty: "更可能是账号问题",
		exit_guilty: "更可能是出口 / IP 问题",
		insufficient: "无法区分，释放双方",
		dismissed: "案件已释放",
	},
	inspect: "查看对照与理由",
	case: "案件 #{{id}}",
	legacyShort: "历史规则结论 · 查看原始证据",
	legacy:
		"这是旧规则产生的历史结论，不具有新实验规则所需的全部对照证据。历史记录保持原样；其分数不能解释为经校准的误判概率。下方保留当时的测试与原始结论，便于人工复核。",
	legacyData: "查看历史原始记录",
	identitiesUnavailable:
		"部分账号名称或出口档案暂时无法加载，引用可能只显示保留的编号。",
	accountSupport: "为什么支持账号问题",
	exitSupport: "为什么支持出口问题",
	limitations: "反证与尚未排除的因素",
	noSupport: "目前没有足够的有效对照支持这一方向。",
	protocol: "实验规则：{{version}}",
	remaining: "最迟约 {{minutes}} 分 {{seconds}} 秒后收口",
	deadlineReached: "截止时间已到，等待本次评估提交结案",
	thresholds:
		"本案规则：账号质量异常至少跨 {{account}} 个独立路径复现；重复响应异常至少跨 {{transport}} 个路径且同路径正常账号通过。涉案出口需 {{jury}} 个独立账号参与对照，至少 {{degraded}} 个在该出口异常、在比较出口恢复；案件账号也需在比较出口恢复。有效反证不能被票数抵消。",
	closure: {
		experiment_unsupported: "实验规格不受支持，未完成归因",
		investigation_unavailable: "调查对象不可用",
		evidence_complete: "对照证据满足规则",
		deadline_reached: "调查时限已到",
		candidates_exhausted: "可用对照或测试预算已用尽",
	},
	group: {
		exit_jury: "固定涉案出口 · 更换账号",
		account_differential: "固定案件账号 · 更换出口",
	},
	question: {
		exit_jury: "这个出口是否也会影响其他正常账号？",
		account_differential: "这个账号在其他独立出口上是否仍然异常？",
	},
	noCandidates: "本方向尚无可执行的对照；缺少资源会作为判断限制保留。",
	count: {
		clean: "正常",
		degraded: "降智",
		transport: "响应异常",
		unavailable: "无法测试",
		pending: "等待结果",
		cancelled: "已中断",
	},
	result: {
		"unknown/measurement/error": "测试失败，原因未确认",
		"local/prepare/configuration": "测试配置不可用",
		"local/prepare/account": "无法读取测试账号",
		"local/prepare/credential": "测试凭据不可用",
		"local/prepare/capacity": "测试资源暂不可用",
		"local/verification/identity": "测试身份未核实",
		"local/verification/path": "比较出口未核实",
		"local/prepare/policy": "测试策略不可用",
		"local/verification/experiment": "测试条件不匹配",
		"local/measurement/interrupted": "测试已中断",
		"local/measurement/resource_limit": "测试达到资源限制",
		"local/record/persistence": "测试记录未保存",
		"unknown/forward/error": "请求失败，来源未确认",
		"unknown/forward/empty_response": "未取得上游响应",
		"upstream/headers/request_rejected": "上游拒绝测试请求",
		"upstream/headers/server_error": "上游服务响应异常",
		"upstream/admission/created_timeout": "上游首事件超时",
		"upstream/admission/evidence_timeout": "上游思考证据等待超时",
		"upstream/admission/empty_stream": "上游返回空流",
		"upstream/admission/truncated_stream": "上游流提前中断",
		"local/admission/unsupported_protocol": "响应协议无法判定",
		"unknown/admission/error": "响应检查失败，来源未确认",
		"unknown/completion/error": "响应完成状态未确认",
		"local/completion/deadline": "测试超过完成时限",

		clean: "已观察到思考特征",
		degraded: "质量降智",
		error: "未取得可判定响应",
		pending: "排队中",
		running: "测试中",
		cancelled: "中断 / 已取消",
		unavailable: "无法测试",
		transport: "传输异常",
		created_timeout: "首事件超时",
		evidence_timeout: "证据等待超时",
		empty_stream: "空响应",
		upstream_http: "上游 HTTP 异常",
		credential_unavailable: "凭据不可用",
		route_unavailable: "路由不可用",
		account_unavailable: "账号不可用",
	},
	matchedControl: "补充对照",
	controlVerified: "已核实出口关系，补充结果可用于验证该方向。",
	controlUnverified: "出口关系未核实，这条补充结果不能用于归因。",
	diagnostics: "执行诊断",
	reason: {
		awaiting_probes: "正在收集两组对照结果",
		account_quality_pattern: "账号在多个经验证的正常路径上持续降智",
		account_availability_pattern: "账号在多个正常路径上反复无法获得有效响应",
		exit_quality_pattern: "其他账号仅在涉案出口复现异常",
		insufficient_controls: "有效对照不足，无法可靠区分",
		not_reproduced: "本轮对照未复现稳定异常",
		repeated_anomaly_unconfirmed: "已发现账号重复异常，尚缺独立对照确认",
		conflicting_evidence: "两个方向同时异常，现有证据冲突",
		account_missing: "案件账号已不存在",
		unsupported_protocol: "此案件使用旧实验规则，现已停止自动归因",
		experiment_baseline_missing: "原始异常缺少完整实验条件，无法开展可比取证",
		experiment_profile_unsupported: "旧版实验不支持原请求的工具或协议配置",
		unsupported_experiment_version: "此案件的测试样本版本已不再支持",
		experiment_sample_unknown: "此案件的测试样本不可用",
	},
	explanation: {
		default:
			"依据本案保存的实验规则检查两组结果。缺失、执行失败和质量异常分别记录；单次异常不足以形成归因结论。",
		account_quality_pattern:
			"涉案出口可以正常服务其他账号；案件账号在多个独立出口降智，而同出口的补充正常账号通过。暂停案件账号的调度，释放本案对出口的冻结。",
		account_availability_pattern:
			"这不是把传输失败当成降智票。多条独立路径上的正常账号均能通过，只有案件账号反复无法取得有效响应。按账号可用性异常暂停调度，释放本案对出口的冻结，可由人工复核。",
		exit_quality_pattern:
			"多个独立账号在涉案出口异常、换出口后恢复，且案件账号也在比较出口恢复。处置涉案 IP，释放本案对账号的冻结。",
		repeated_anomaly_unconfirmed:
			"重复失败已经被保留为账号嫌疑，未被当作普通空白证据。由于路径独立性或同路径正常对照尚未确认，仍不能排除网络或系统因素；达到时限或预算后释放双方，保留具体疑点供人工复核。",
		not_reproduced:
			"案件账号在比较出口正常，其他账号在涉案出口也正常，本轮未发现稳定的账号或出口问题。释放双方，继续由守卫观察实际请求。",
		conflicting_evidence:
			"账号方向和出口方向都出现异常，不能强行选择一方。结束本案并释放本案冻结，同时保留冲突证据。",
	},
	scope: "取证范围：{{model}} · 推理档位 {{effort}}",
	scopeHelp: "使用固定测试样本检查推理通道行为。结果适用于保存的模型配置，不代表复杂任务的答案正确率。",
	signals: {
		experiment_mismatch: "部分结果与本案保存的模型、推理配置或测试样本不一致，已排除。",
		physical_identity_unverified: "部分测试缺少可核实的实际调用身份，已排除。",
		mixed_guard_policies: "测试使用了不同的守卫策略，无法合并归因。",
		experiment_baseline_missing: "原始异常缺少完整实验条件。",
		experiment_profile_unsupported: "旧版实验不支持原始工具或协议配置。新立案件使用思考流特征探针。",
		incident_path_unverified:
			"部分涉案出口测试的实际路径未核实，不能作为同出口对照。",
		incident_exit_changed:
			"涉案出口测试期间使用了不同 IP，无法视为固定出口实验。",
		incident_exit_serves_other_accounts: "涉案出口能够正常服务多个独立账号。",
		account_degraded_on_healthy_paths:
			"案件账号在正常账号能够通过的比较路径上降智。",
		repeated_account_response_failures:
			"案件账号跨比较出口反复无法取得可判定响应。",
		matched_controls_exclude_path_outage:
			"这些比较路径上的补充正常账号通过，支持排除路径整体不可用。",
		other_accounts_degrade_only_on_incident_exit:
			"其他账号在涉案出口降智，在独立比较出口恢复。",
		account_recovers_on_other_exit:
			"案件账号在经验证的不同出口恢复正常，构成账号持续异常的反证。",
		duplicate_exit_ip: "多个节点通向同一出口 IP，仅计为一个独立路径。",
		missing_matched_control:
			"部分异常缺少成功的补充对照，不能排除路径或上游问题。",
		account_results_conflict: "案件账号存在正常与异常结果，尚未形成一致模式。",
		both_directions_abnormal: "两组对照均有异常，可能有共同原因或多个问题。",
		interrupted_tests: "部分测试因重启、超时或结案中断，未计为执行失败。",
		exit_control_quorum_missing: "涉案出口的有效独立账号数量尚未达到本案要求。",
	},
};
export const experimentEn = {
	manualReview: "Review and release",
	manualHelp:
		"Record a reason to revoke this case’s restrictions. Original decisions and tests remain available. Other cases retain their restrictions.",
	manualReason: "Review reason (at least 3 characters)",
	release: "Record reason and release",
	manualReleased: "Released after manual review",
	title: "Quality attribution cases",
	intro:
		"Hold the account and exit, isolate one variable at a time, and settle each controlled investigation before its deadline.",
	review: "Evaluate now",
	steps: {
		protect: "Protect · hold both parties",
		compare: "Compare · isolate variables",
		explain: "Check support and counterevidence",
		finish: "Settle · bounded deadline",
	},
	filter: {
		all: "All cases",
		open: "Investigating",
		closed: "Closed",
	},
	search: "Account, exit, IP or case number",
	empty: "No matching cases. New quality incidents will appear here.",
	verdict: {
		investigating: "Investigating · temporary holds",
		account_guilty: "Account problem more likely",
		exit_guilty: "Exit / IP problem more likely",
		insufficient: "Unable to distinguish · release both",
		dismissed: "Released",
	},
	inspect: "Inspect controls and reasons",
	case: "Case #{{id}}",
	legacyShort: "Historical decision · original evidence",
	legacy:
		"This historical decision used the previous rules and lacks the full matched controls required by the new protocol. Its score is not a calibrated probability. Original measurements and records are retained for review.",
	legacyData: "Original historical record",
	identitiesUnavailable:
		"Some account names or exit records could not be loaded. References may show retained identifiers.",
	accountSupport: "Support for the account hypothesis",
	exitSupport: "Support for the exit hypothesis",
	limitations: "Counterevidence and unresolved factors",
	noSupport: "There are not enough valid controls supporting this hypothesis.",
	protocol: "Protocol: {{version}}",
	remaining: "Deadline in {{minutes}}m {{seconds}}s",
	deadlineReached: "Deadline reached; awaiting settlement commit",
	thresholds:
		"Account: {{account}} independent paths with reproduced degradation, or {{transport}} repeated response failures with clean matched controls. Exit: {{jury}} independent accounts with {{degraded}} degrading only on the incident exit, plus defendant recovery elsewhere. Valid counterevidence cannot be outweighed by counts.",
	closure: {
		experiment_unsupported: "Unsupported experiment; attribution unavailable",
		investigation_unavailable: "Investigation subject unavailable",
		evidence_complete: "Evidence meets protocol",
		deadline_reached: "Investigation deadline reached",
		candidates_exhausted: "Eligible controls or attempt budget exhausted",
	},
	group: {
		exit_jury: "Keep incident exit · change accounts",
		account_differential: "Keep incident account · change exits",
	},
	question: {
		exit_jury: "Does this exit affect other healthy accounts?",
		account_differential:
			"Does this account remain abnormal on independent exits?",
	},
	noCandidates:
		"No comparison available for this direction. Missing resources remain an explicit limitation.",
	count: {
		clean: "Clean",
		degraded: "Degraded",
		transport: "Response failure",
		unavailable: "Unavailable",
		pending: "Awaiting result",
		cancelled: "Interrupted",
	},
	result: {
		"unknown/measurement/error": "Measurement failed; cause unconfirmed",
		"local/prepare/configuration": "Test configuration unavailable",
		"local/prepare/account": "Test account could not be loaded",
		"local/prepare/credential": "Test credential unavailable",
		"local/prepare/capacity": "Test capacity unavailable",
		"local/verification/identity": "Test identity unverified",
		"local/verification/path": "Comparison exit unverified",
		"local/prepare/policy": "Test policy unavailable",
		"local/verification/experiment": "Experiment conditions do not match",
		"local/measurement/interrupted": "Measurement interrupted",
		"local/measurement/resource_limit": "Measurement resource limit reached",
		"local/record/persistence": "Measurement could not be saved",
		"unknown/forward/error": "Request failed; source unconfirmed",
		"unknown/forward/empty_response": "No upstream response available",
		"upstream/headers/request_rejected": "Upstream rejected the test request",
		"upstream/headers/server_error": "Upstream service error",
		"upstream/admission/created_timeout": "Upstream first-event timeout",
		"upstream/admission/evidence_timeout": "Upstream reasoning evidence timeout",
		"upstream/admission/empty_stream": "Upstream returned an empty stream",
		"upstream/admission/truncated_stream": "Upstream stream ended prematurely",
		"local/admission/unsupported_protocol": "Response protocol cannot be classified",
		"unknown/admission/error": "Response inspection failed; source unconfirmed",
		"unknown/completion/error": "Response completion unconfirmed",
		"local/completion/deadline": "Measurement completion deadline reached",

		clean: "Thinking evidence observed",
		degraded: "Quality degraded",
		error: "No classifiable response",
		pending: "Queued",
		running: "Running",
		cancelled: "Interrupted / cancelled",
		unavailable: "Unavailable",
		transport: "Transport failure",
		created_timeout: "First-event timeout",
		evidence_timeout: "Evidence timeout",
		empty_stream: "Empty response",
		upstream_http: "Upstream HTTP failure",
		credential_unavailable: "Credential unavailable",
		route_unavailable: "Route unavailable",
		account_unavailable: "Account unavailable",
	},
	matchedControl: "Matched control",
	controlVerified:
		"The exit relationship was verified; this result can test the hypothesis.",
	controlUnverified:
		"Exit relationship unverified; this control cannot support attribution.",
	diagnostics: "Execution diagnostics",
	reason: {
		awaiting_probes: "Collecting both comparison groups",
		account_quality_pattern: "Account degrades across verified healthy paths",
		account_availability_pattern:
			"Account repeatedly fails on otherwise healthy paths",
		exit_quality_pattern: "Other accounts fail only on the incident exit",
		insufficient_controls: "Insufficient valid controls to distinguish causes",
		not_reproduced: "No stable anomaly reproduced",
		repeated_anomaly_unconfirmed:
			"Repeated account anomaly observed; independent controls missing",
		conflicting_evidence: "Both directions abnormal; conflicting evidence",
		account_missing: "Incident account no longer exists",
		unsupported_protocol: "Automatic attribution stopped for this older experiment protocol",
		experiment_baseline_missing: "The original anomaly lacks a complete experiment specification",
		experiment_profile_unsupported: "The legacy experiment does not support the original tool or protocol configuration",
		unsupported_experiment_version: "The saved sample version is no longer supported",
		experiment_sample_unknown: "The saved test sample is unavailable",
	},
	explanation: {
		default:
			"Apply the protocol saved with this case. Missing results, execution failures and quality degradation are separate. A single anomaly cannot establish attribution.",
		account_quality_pattern:
			"Other accounts pass on the incident exit. The defendant degrades across independent exits where matched controls pass. Suspend the defendant and release this case’s exit hold.",
		account_availability_pattern:
			"Transport failures are not quality votes. Clean accounts pass on multiple independent paths where the defendant repeatedly fails. Suspend the account for an availability pattern, release the exit, and retain evidence for review.",
		exit_quality_pattern:
			"Independent accounts recover when moved away from the incident exit, as does the defendant. Restrict the implicated IP and release this case’s account hold.",
		repeated_anomaly_unconfirmed:
			"Repeated failures remain explicit account suspicion. Missing independent paths or clean matched controls prevent ruling out shared infrastructure failure. Release at the deadline or budget limit while retaining the specific unresolved pattern.",
		not_reproduced:
			"Both comparison directions recovered. Release both parties and continue observing actual traffic.",
		conflicting_evidence:
			"Both directions show anomalies. A common cause or multiple problems remain possible. Release this case’s holds and retain the contradiction.",
	},
	scope: "Test scope: {{model}} · reasoning effort {{effort}}",
	scopeHelp: "Fixed samples check reasoning-channel behavior under the saved model configuration, not answer correctness on complex tasks.",
	signals: {
		experiment_mismatch: "Results with different models, reasoning settings or samples were excluded.",
		physical_identity_unverified: "Tests without verifiable physical identities were excluded.",
		mixed_guard_policies: "Different guard policies cannot be combined for attribution.",
		experiment_baseline_missing: "The original anomaly lacks a complete experiment specification.",
		experiment_profile_unsupported: "The legacy experiment does not support this tool or protocol configuration. New cases use thinking-stream probes.",
		incident_path_unverified:
			"Some incident-exit measurements lack a verified path.",
		incident_exit_changed:
			"The incident exit used different IPs during testing; the fixed-exit control is invalid.",
		incident_exit_serves_other_accounts:
			"Multiple independent accounts pass on the incident exit.",
		account_degraded_on_healthy_paths:
			"The defendant degrades on paths where matched healthy accounts pass.",
		repeated_account_response_failures:
			"The defendant repeatedly fails to obtain a classifiable response across comparison exits.",
		matched_controls_exclude_path_outage:
			"Clean matched controls on these paths support excluding a path-wide outage.",
		other_accounts_degrade_only_on_incident_exit:
			"Other accounts degrade on the incident exit and recover elsewhere.",
		account_recovers_on_other_exit:
			"The defendant recovers on a verified different exit, opposing persistent account failure.",
		duplicate_exit_ip:
			"Several node records lead to one IP and count as one independent path.",
		missing_matched_control:
			"Some anomalies lack clean matched controls; path or provider failure remains possible.",
		account_results_conflict:
			"The defendant has both clean and abnormal results.",
		both_directions_abnormal:
			"Both comparison groups are abnormal; shared causes or multiple issues remain possible.",
		interrupted_tests:
			"Some tests were interrupted by restart, timeout or settlement and are not execution failures.",
		exit_control_quorum_missing:
			"The incident exit lacks the required number of valid independent account controls.",
	},
};
