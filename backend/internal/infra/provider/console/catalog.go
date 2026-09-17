package console

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

const (
	QuotaMode      = "console"
	QuotaModeImage = "console_image"
	QuotaModeVideo = "console_video"
)

type ModelSpec struct {
	PublicID                string
	UpstreamModel           string
	SupportsReasoning       bool
	SupportsReasoningEffort bool
	DefaultReasoningEffort  string
	MaxOutputTokens         int
}

var catalog = []ModelSpec{
	{UpstreamModel: "grok-4.3", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000},
	{UpstreamModel: "grok-4.20-0309-reasoning", SupportsReasoning: true, MaxOutputTokens: 1_000_000},
	{UpstreamModel: "grok-4.20-0309-non-reasoning", MaxOutputTokens: 1_000_000},
	{UpstreamModel: "grok-4.20-multi-agent-0309", SupportsReasoning: true, SupportsReasoningEffort: true, MaxOutputTokens: 1_000_000},
	{UpstreamModel: "grok-4.5", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000},
	{UpstreamModel: "grok-build-0.1", MaxOutputTokens: 256_000},
}

// Catalog retains the text protocol parameters; public products come from M05.
// 跨包契约测试入口,生产经静态目录定义。
func Catalog() []ModelSpec {
	values := make([]ModelSpec, 0, len(catalog))
	for _, spec := range catalog {
		values = append(values, withPublicProduct(spec))
	}
	return values
}

func withPublicProduct(spec ModelSpec) ModelSpec {
	spec.PublicID, _, _ = modeldomain.CatalogDefault(account.ProviderConsole, spec.UpstreamModel)
	return spec
}

func Resolve(upstreamModel string) (ModelSpec, bool) {
	for _, spec := range catalog {
		if spec.UpstreamModel == upstreamModel {
			return withPublicProduct(spec), true
		}
	}
	return ModelSpec{}, false
}

func ResolveMedia(upstreamModel string, capability modeldomain.Capability) bool {
	return capability != modeldomain.CapabilityResponses &&
		modeldomain.CatalogSupports(account.ProviderConsole, upstreamModel, capability)
}

func allModels() []string {
	products := modeldomain.CatalogModels(account.ProviderConsole)
	values := make([]string, 0, len(products))
	for _, spec := range products {
		values = append(values, spec.UpstreamModel)
	}
	return values
}
