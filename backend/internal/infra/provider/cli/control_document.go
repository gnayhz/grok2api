package cli

import "io"

// readControlDocument preserves bounded error diagnostics while reporting
// whether a successful document is complete. Callers check truncation after
// interpreting the HTTP status and before decoding any success fields.
func readControlDocument(body io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}
