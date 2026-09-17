package provider

// Build 方言的客户端身份事实。组合根、infra/config 默认值与 Build 适配器
// 共用同一来源:Provider 身份归 Provider 合同层,不再从 infra/config
// 反向引用。
const (
	// RecommendedBuildClientVersion 是已完成兼容验证的 Grok Build 客户端版本。
	RecommendedBuildClientVersion = "1.0.4"
	// RecommendedBuildUserAgent 是随 Build 请求发送的推荐 User-Agent。
	RecommendedBuildUserAgent = "grok-shell/" + RecommendedBuildClientVersion + " (linux; x86_64)"
)
