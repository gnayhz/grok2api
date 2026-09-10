package jsonvalue

import (
	"encoding/json"
	"testing"
)

func TestUnmarshalExactNumbersAndSingleValue(t *testing.T) {
	var value struct {
		Limit int            `json:"limit"`
		Data  map[string]any `json:"data"`
	}
	if err := Unmarshal([]byte(`{"limit":12,"data":{"id":9007199254740993,"fraction":0.1234567890123456789,"huge":1e1000}} `), &value); err != nil {
		t.Fatal(err)
	}
	if value.Limit != 12 || value.Data["id"] != json.Number("9007199254740993") || value.Data["fraction"] != json.Number("0.1234567890123456789") || value.Data["huge"] != json.Number("1e1000") {
		t.Fatalf("rounded numbers: %#v", value)
	}
	for _, raw := range []string{`{} {}`, `{} true`, `{} trailing`, "{}\u00a0", `{"limit":1.5}`, `{"data":`, ``, `NaN`} {
		if err := Unmarshal([]byte(raw), &value); err == nil {
			t.Errorf("accepted invalid input %q", raw)
		}
	}
}
