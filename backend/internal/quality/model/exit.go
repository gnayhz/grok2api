package model

// ExitState 是出口(节点,IP-epoch)在质量轴上的状态。
// 临时调查结束后直接回到 AVAILABLE;持久 ban 只作用于当前 epoch。
type ExitState string

const (
	// ExitAvailable 可用:正常参与调度。
	ExitAvailable ExitState = "available"
	// ExitRemanded 羁押:立案后等待裁决。
	ExitRemanded ExitState = "remanded"
	// ExitBanned IP 禁:IP 有罪裁决后的处置。池隧道节点永不进入此状态
	// (粘性窗口内靠 epoch 翻篇自愈,按请求天然无事)。
	ExitBanned ExitState = "banned"
)

// Schedulable 报告该状态下出口是否可参与生产调度(B2)。
func (s ExitState) Schedulable() bool {
	return s == ExitAvailable
}

// exitTransitionLegal 登记 B1.2 出口状态转移的合法来源集合。
var exitTransitionLegal = map[ExitState]map[ExitState]struct{}{
	ExitRemanded: {
		// 立案即羁押(降智事件)。
		ExitAvailable: {},
		// 同一状态的重复写入保持幂等。
		ExitRemanded: {},
	},
	ExitAvailable: {
		// 调查无罪或证据不足:调度无痕释放。
		ExitRemanded: {},
		// 统一 ban 律:探测发现 IP 变化(epoch 翻篇)即解禁。
		ExitBanned: {},
	},
	ExitBanned: {
		// IP 有罪裁决:REMANDED→BANNED。
		ExitRemanded: {},
	},
}

// CanTransitionExit 报告出口状态转移是否合法(B1.2)。
func CanTransitionExit(from, to ExitState) bool {
	if from == to {
		// 幂等重放(同状态重复执行)由执行器语义保证,状态层放行。
		return true
	}
	sources, ok := exitTransitionLegal[to]
	if !ok {
		return false
	}
	_, ok = sources[from]
	return ok
}

// NodeType 是出口节点的三型学(基准 2.1:固定/webhook/代理池隧道)。
// 类型只决定降智后的自愈路径,不影响池调度(成员一等公民)。
type NodeType string

const (
	// NodeFixed 固定出口:IP 恒定(家宽缓慢漂移;直连是其退化情形)。
	// 换 IP 控制方=无人。BANNED 解禁=探测到漂移/人工。
	NodeFixed NodeType = "fixed"
	// NodeWebhook webhook 节点:自管 WARP 出口+配套换 IP 服务,
	// 换 IP 控制方=我们(主动触发轮换后探测确认解禁)。
	NodeWebhook NodeType = "webhook"
	// NodePoolSticky 粘性池隧道:供应商规定时间内固定、到时自动换。
	// 永不 BANNED;epoch 翻篇即自动解除羁押。
	NodePoolSticky NodeType = "pool_sticky"
	// NodePoolPerRequest 按请求池隧道:每次换 IP,单 IP ban 无意义,
	// 质量侧无动作。
	NodePoolPerRequest NodeType = "pool_per_request"
)

// QualityBanApplicable 报告该节点型是否接受 IP 级质量 ban
// (B1.2 定稿决议 1:池隧道节点永远不进入 BANNED)。
func (t NodeType) QualityBanApplicable() bool {
	return t == NodeFixed || t == NodeWebhook
}

// EpochKey 定位一个出口统计单元:节点 + IP-epoch。
// epoch 检测到 IP 变化即翻篇——新 IP 不背旧 IP 的历史(I15)。
type EpochKey struct {
	NodeID uint64
	Epoch  uint64
}
