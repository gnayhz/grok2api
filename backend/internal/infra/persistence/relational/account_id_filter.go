package relational

import (
	"encoding/json"
	"gorm.io/gorm"
)

// accountIDFilter keeps large restriction sets below driver bind-parameter limits.
func accountIDFilter(query *gorm.DB, ids []uint64, exclude bool) *gorm.DB {
	operator := "IN"
	if exclude {
		operator = "NOT IN"
	}
	if len(ids) <= 500 {
		return query.Where("provider_accounts.id "+operator+" ?", ids)
	}
	encoded, _ := json.Marshal(ids) // uint64 slices cannot fail JSON encoding.
	source := "SELECT value FROM json_each(?)"
	if query.Dialector.Name() == "postgres" {
		source = "SELECT CAST(value AS BIGINT) FROM json_array_elements_text(CAST(? AS json)) AS ids(value)"
	}
	return query.Where("provider_accounts.id "+operator+" ("+source+")", string(encoded))
}
