package inference

import (
	"bytes"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"mime/multipart"
)

func benchmarkDecodeSTTOptions(encoding string, data []byte, form *multipart.Form) (gateway.STTInput, error) {
	var options sttOptions
	var input gateway.STTInput
	var err error
	if encoding == "json" {
		err = decodeSingleJSON(bytes.NewReader(data), &options, false)
	} else {
		options, err = multipartSTTOptions(form)
	}
	if err != nil {
		return input, err
	}
	err = options.apply(&input)
	return input, err
}
