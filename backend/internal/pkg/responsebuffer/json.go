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
	capacity := 4096 + len(data)*4
	quoted, escaped := false, false
	for _, c := range data {
		if quoted {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
			capacity += 64
		case '{', '[':
			capacity += 256
		case ',', ':':
			capacity += 128
		}
	}
	return capacity
}
