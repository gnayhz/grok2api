package model

import "github.com/chenyme/grok2api/backend/internal/domain/account"

// NameSource belongs to a name/route relationship, independently of the route's
// product origin. Legacy facts carry no proof that an automatic writer owns them.
type NameSource string

const (
	NameSourceLegacy    NameSource = "legacy"
	NameSourceGenerated NameSource = "generated"
	NameSourceManual    NameSource = "manual"
)

// MergeNameSource never downgrades an existing retention requirement. Missing
// or unrecognized historical metadata is treated conservatively as legacy.
func MergeNameSource(a, b NameSource) NameSource {
	if a == NameSourceManual || b == NameSourceManual {
		return NameSourceManual
	}
	if a == NameSourceGenerated && b == NameSourceGenerated {
		return NameSourceGenerated
	}
	return NameSourceLegacy
}

// RetireCatalogName applies only to a proven automatically generated name of
// the retired product. A manual route or name is not owned by catalog retirement.
func RetireCatalogName(route Route, name string, source NameSource) bool {
	if source != NameSourceGenerated || (route.Origin != OriginCatalog && route.Origin != OriginDiscovered) {
		return false
	}
	canonical, ok := NormalizePublicID(route.Provider, name)
	return ok && route.Provider == account.ProviderWeb &&
		canonical == "Web/grok-imagine-image-quality-lite" &&
		route.UpstreamModel == "grok-imagine-image-quality" && route.Capability == CapabilityImage
}

type AliasPromotion uint8

const (
	AliasPromotionConflict AliasPromotion = iota
	AliasPromotionRestore
	AliasPromotionRemap
	AliasPromotionArchive
	AliasPromotionKeepArchived
)

// CatalogAliasPromotion preserves the historical canonical-product precedence
// for legacy aliases, but archives their unknown-source edges. They must leave
// routing/permission candidates, just as the old product split removed them.
// Only generated edges can be discarded during a remap to a retained product.
func CatalogAliasPromotion(source NameSource, sameTarget, retainedTarget, replaced bool) AliasPromotion {
	if sameTarget {
		return AliasPromotionRestore
	}
	if replaced {
		return AliasPromotionKeepArchived
	}
	if !retainedTarget || source == NameSourceManual {
		return AliasPromotionConflict
	}
	if source == NameSourceGenerated {
		return AliasPromotionRemap
	}
	return AliasPromotionArchive
}
