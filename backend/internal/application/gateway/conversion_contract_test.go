package gateway

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// R12 conversion-chain contract tests: they lock the current conversion
// semantics ahead of centralizing public protocol adapters.
//  1. a failed conversion never yields an empty successful response;
//  2. stream and JSON converters are mutually exclusive;
//  3. a converter without a body violates the contract;
//  4. JSON conversion output replaces the buffered body.

func completedJSON() string {
	return strings.Join([]string{"{", string('"'), "status", string('"'), ":", string('"'), "completed", string('"'), "}"}, "")
}

func publicShapeJSON() string {
	return strings.Join([]string{"{", string('"'), "public", string('"'), ":", string('"'), "shape", string('"'), "}"}, "")
}

func newConversionResponse(body io.ReadCloser) *provider.Response {
	return &provider.Response{StatusCode: 200, Body: body}
}

func TestConversionContractBothStreamsAndJSONIsRejected(t *testing.T) {
	response := newConversionResponse(io.NopCloser(strings.NewReader(completedJSON())))
	response.ConvertStream = func(source io.ReadCloser) io.ReadCloser { return source }
	response.ConvertJSON = func(raw []byte) ([]byte, error) { return raw, nil }
	err := prepareResponseDelivery(response, false, nil)
	if err == nil {
		t.Fatal("dual converters must fail the conversion contract")
	}
}

func TestConversionContractConverterWithoutBodyIsRejected(t *testing.T) {
	response := &provider.Response{StatusCode: 200}
	response.ConvertJSON = func(raw []byte) ([]byte, error) { return raw, nil }
	if err := prepareResponseDelivery(response, false, nil); err == nil {
		t.Fatal("converter without body must fail the conversion contract")
	}
}

func TestConversionContractFailingJSONConverterNeverSucceeds(t *testing.T) {
	response := newConversionResponse(io.NopCloser(strings.NewReader(completedJSON())))
	boom := errors.New("conversion boom")
	response.ConvertJSON = func(raw []byte) ([]byte, error) { return nil, boom }
	err := prepareResponseDelivery(response, false, nil)
	if err == nil || !errors.Is(err, errResponseConversion) {
		t.Fatalf("failed conversion must surface conversion error, got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("conversion error must preserve cause, got %v", err)
	}
}

func TestConversionContractJSONConverterOutputReplacesBody(t *testing.T) {
	response := newConversionResponse(io.NopCloser(strings.NewReader(completedJSON())))
	response.ConvertJSON = func(raw []byte) ([]byte, error) {
		return []byte(publicShapeJSON()), nil
	}
	if err := prepareResponseDelivery(response, false, nil); err != nil {
		t.Fatal(err)
	}
	data, release, ok := responsebuffer.Borrow(response.Body)
	if !ok {
		t.Fatal("converted body must be buffered")
	}
	release()
	if string(bytes.TrimSpace(data)) != publicShapeJSON() {
		t.Fatalf("converted body = %q", data)
	}
}
