package audit

import "time"

type Operation string

const (
	OperationResponses  Operation = "responses"
	OperationCompaction Operation = "compaction"
	OperationChat       Operation = "chat"
	OperationMessages   Operation = "messages"
	OperationImage      Operation = "image"
	OperationImageEdit  Operation = "image_edit"
	OperationVideo      Operation = "video"
	OperationTTS        Operation = "tts"
	OperationSTT        Operation = "stt"
	OperationRealtime   Operation = "realtime"
	OperationVoice      Operation = "voice"
)

type UsageSource string

const (
	UsageSourceUpstream  UsageSource = "upstream"
	UsageSourceEstimated UsageSource = "estimated"
	UsageSourceNone      UsageSource = "none"
)

type AttemptSource string

const (
	AttemptSourceUpstreamHTTP AttemptSource = "upstream_http"
	AttemptSourceTransport    AttemptSource = "gateway_transport"
	AttemptSourceCredential   AttemptSource = "credential"
)

type ErrorFrame struct {
	Type    string
	Message string
}

// Attempt 保存一次失败尝试经过裁剪和脱敏的管理员诊断快照。
type Attempt struct {
	ID                    uint64
	AuditID               uint64
	Number                int
	Source                AttemptSource
	Stage                 string
	AccountID             *uint64
	AccountName           string
	Method                string
	RequestPath           string
	UpstreamURL           string
	StartedAt             time.Time
	DurationMS            int64
	UpstreamStatusCode    *int
	UpstreamStatus        string
	ResponseHeaders       map[string][]string
	ResponseBody          []byte
	ResponseBodyTruncated bool
	TransportError        string
	ErrorChain            []ErrorFrame
}

type EgressMode string

const (
	EgressModeDirect EgressMode = "direct"
	EgressModeProxy  EgressMode = "proxy"
)

// Record 表示推理请求审计；成功请求不保存正文，失败请求仅保留受限诊断快照。
type Record struct {
	// Completion stages retain independent acknowledgements. Empty means not
	// recorded. Server delivery cannot prove client consumption; a missing
	// acknowledgement does not establish that a remote write was rolled back.
	UpstreamStatusCode  int
	ResponseID          string
	AdmissionOutcome    string
	GenerationOutcome   string
	ProviderStateCommit string
	OwnershipCommit     string
	DeliveryOutcome     string
	PhysicalReceipt     string
	QualityReceipt      string
	LedgerOutcome       string
	GenerationUsages    []GenerationUsage

	HistoryOutcome       string
	HistoryScopeHash     string
	HistoryGeneration    int64
	HistoryRestoredItems int
	HistoryNormalizer    int
	HistoryCommit        string

	ID                      uint64
	EventID                 string
	RequestID               string
	ClientKeyID             uint64
	ClientKeyName           string
	ClientIP                string
	ModelRouteID            uint64
	ModelPublicID           string
	ModelUpstreamModel      string
	Provider                string
	Operation               Operation
	UsageSource             UsageSource
	ReasoningEffort         string
	AccountID               *uint64
	AccountName             string
	EgressNodeID            *uint64
	EgressNodeName          string
	EgressScope             string
	EgressMode              EgressMode
	StatusCode              int
	Streaming               bool
	MediaInputImages        int64
	MediaOutputImages       int64
	MediaOutputSeconds      int64
	AudioDurationMS         int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	CostInUSDTicks          int64
	EstimatedCostInUSDTicks int64
	PricingModel            string
	PricingVersion          string
	NumSourcesUsed          int64
	NumServerSideToolsUsed  int64
	ContextInputTokens      int64
	ContextOutputTokens     int64
	FirstTokenMS            *int64
	// DeliveredEvents/DeliveredBytes：流式=转发到客户端的 SSE data 事件数与
	// 累计字节；非流式=响应体字节数。回答「200 且带错误码时实际交付了多少」。
	DeliveredEvents int64
	DeliveredBytes  int64
	DurationMS      int64
	ErrorCode       string
	// QualityFailOpen: 请求经质量守卫判定降级但按 fail-open 策略交付。
	QualityFailOpen bool
	// QualityExempt: 守卫未介入该请求的豁免原因 token（disabled/skip_input/
	// operation/compaction/provider/model_out_of_scope/messages_thinking_off/
	// model_no_reasoning）。空串=守卫介入。历史事故复盘需容器日志与内存计数器
	// 交叉才能复原"守卫为何不在场"，该字段让审计行自带答案。
	QualityExempt string
	// QualityRule: 守卫介入时最终交付尝试的判决指纹规则（thinking/item_done/
	// outrun/terminal…）。交付行 rule=thinking 表示流内观察到可见思考增量；
	// 非 thinking 规则的 200 交付配合 QualityFailOpen 表达 fail-open 形态。
	QualityRule string
	// 请求诊断(#983 隐私安全载荷,写入前经 sanitizeRequestMetadata 脱敏)。
	RequestMethod  string
	RequestPath    string
	RequestHeaders map[string][]string
	AttemptCount   int
	Attempts       []Attempt
	CreatedAt      time.Time
}

// Summary 表示指定审计范围内的聚合用量。
type Summary struct {
	Requests                int64
	SuccessfulRequests      int64
	FailedRequests          int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	DurationMS              int64
	EstimatedCostInUSDTicks int64
	PricedRequests          int64
	UnpricedRequests        int64
	PricedTokens            int64
	UnpricedTokens          int64
}
