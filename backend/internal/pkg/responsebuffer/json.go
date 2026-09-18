package responsebuffer

// JSONWorkspace reserves a conservative decoding/encoding workspace before a
// semantic JSON decoder can allocate maps, slices or unescaped strings. Raw
// byte length alone misses object amplification (for example millions of {}).
// The structural scan allocates nothing and ignores punctuation inside strings.
// It is a capacity policy, not a measurement of the Go allocator's exact usage.
func JSONWorkspace(budget *Budget, data []byte) (*Lease, error) {
	if budget == nil {
		budget = NewRequest()
	}
	return budget.Reserve(JSONWorkspaceSize(data))
}

// JSONWorkspaceSize is the conservative capacity policy shared by transient
// decoders and owners retaining decoded metadata.
func JSONWorkspaceSize(data []byte) int {
	return 4*len(data) + jsonStructureSize(data, false)
}

// BorrowedJSONWorkspaceSize bounds maps/slices and decoded object keys when
// values borrow validated input bytes. Callers must separately reserve any
// value decoding, copied strings, normalized items and encoded output.
func BorrowedJSONWorkspaceSize(data []byte) int {
	return jsonStructureSize(data, true)
}

func jsonStructureSize(data []byte, copiedKeys bool) int {
	capacity := 4096
	quoted, escaped := false, false
	stringStart := 0
	for i, c := range data {
		if quoted {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
				if copiedKeys {
					end := i + 1
					for end < len(data) && (data[end] == ' ' || data[end] == '\n' || data[end] == '\r' || data[end] == '\t') {
						end++
					}
					if end < len(data) && data[end] == ':' {
						capacity += 4 * (i - stringStart + 1)
					}
				}
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
			stringStart = i
			capacity += 64
		case '{', '[':
			capacity += 256
		case ',', ':':
			capacity += 128
		}
	}
	return capacity
}
