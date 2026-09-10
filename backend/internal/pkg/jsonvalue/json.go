// Package jsonvalue decodes editable JSON trees without rounding their numbers.
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Unmarshal retains JSON numbers as json.Number in interface values. Like
// json.Unmarshal, it accepts exactly one value, including trailing whitespace.
// Typed numeric fields still follow encoding/json's normal decoding rules.
func Unmarshal(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	for _, char := range data[decoder.InputOffset():] {
		switch char {
		case ' ', '\t', '\n', '\r':
		default:
			return errors.New("invalid data after JSON value")
		}
	}
	return nil
}
