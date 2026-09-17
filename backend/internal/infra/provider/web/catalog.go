package web

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

type ModelSpec struct {
	PublicID      string
	UpstreamModel string
	ProtocolModel string
	ImaginePro    bool
	Capability    modeldomain.Capability
	Mode          string
	MinimumTier   account.WebTier
}

var catalog = []ModelSpec{
	{UpstreamModel: "grok-chat-fast", Mode: "fast", MinimumTier: account.WebTierBasic},
	{UpstreamModel: "grok-chat-auto", Mode: "auto", MinimumTier: account.WebTierSuper},
	{UpstreamModel: "grok-chat-expert", Mode: "expert", MinimumTier: account.WebTierSuper},
	{UpstreamModel: "grok-chat-heavy", Mode: "heavy", MinimumTier: account.WebTierHeavy},
	// Lite keeps the distinct fast/chat product name. Imagine WebSocket models
	// share the Console-facing product names but select their protocol version
	// through enable_pro. Media products are available to Basic accounts with
	// runtime selection fenced by tier-specific upstream quota windows.
	{UpstreamModel: "grok-imagine-image", ProtocolModel: "imagine-lite", Mode: "fast", MinimumTier: account.WebTierBasic},
	{UpstreamModel: "grok-imagine-image-quality", ProtocolModel: "imagine", Mode: "image_pro", MinimumTier: account.WebTierBasic},
	{UpstreamModel: "grok-imagine-image-2.0", ProtocolModel: "imagine", ImaginePro: true, Mode: "image_pro", MinimumTier: account.WebTierBasic},
	{UpstreamModel: "imagine-image-edit", Mode: "image_edit", MinimumTier: account.WebTierBasic},
	{UpstreamModel: "grok-imagine-video", ProtocolModel: "imagine-video-gen", Mode: "video", MinimumTier: account.WebTierBasic},
}

// Catalog joins Provider wire parameters with the M05 product mapping.
// 跨包契约测试入口,生产经静态目录定义。
func Catalog() []ModelSpec {
	values := make([]ModelSpec, 0, len(catalog))
	for _, spec := range catalog {
		values = append(values, withPublicProduct(spec))
	}
	return values
}

func withPublicProduct(spec ModelSpec) ModelSpec {
	spec.PublicID, spec.Capability, _ = modeldomain.CatalogDefault(account.ProviderWeb, spec.UpstreamModel)
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

func TierSupports(actual, minimum account.WebTier) bool {
	rank := map[account.WebTier]int{account.WebTierBasic: 1, account.WebTierSuper: 2, account.WebTierHeavy: 3}
	return rank[actual] >= rank[minimum]
}
