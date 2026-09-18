package requestdiag

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

var promptKey struct {
	sync.Once
	key       [32]byte
	epoch     string
	available bool
}

type promptKeyContext struct{}

// WithPrompt hashes the final provider payload synchronously and retains only
// HMACs. Borrowed JSON fields avoid copying inline media or opaque reasoning.
func WithPrompt(ctx context.Context, body []byte) context.Context {
	if FromContext(ctx) == nil {
		return ctx
	}
	promptKey.Do(func() {
		if _, err := rand.Read(promptKey.key[:]); err != nil {
			return
		}
		h := sha256.Sum256(promptKey.key[:])
		promptKey.epoch = hex.EncodeToString(h[:8])
		promptKey.available = true
	})
	if !promptKey.available {
		return ctx
	}
	started := time.Now()
	defer Stage(ctx, "prompt_diagnostics", started)
	digest := func(parts ...[]byte) string {
		h := hmac.New(sha256.New, promptKey.key[:])
		for _, p := range parts {
			h.Write(p)
			h.Write([]byte{0})
		}
		return hex.EncodeToString(h.Sum(nil)[:16])
	}
	fields := make(map[string][]byte, 8)
	jsonpeek.ObjectFields(body, func(k, v []byte) bool {
		switch string(k) {
		case "input", "instructions", "tools", "prompt_cache_key", "model", "reasoning", "tool_choice", "parallel_tool_calls", "text", "temperature", "top_p", "max_output_tokens":
			fields[string(k)] = v
		}
		return true
	})
	p := &audit.PromptDiagnostic{KeyEpoch: promptKey.epoch,
		Session: digest([]byte("session"), fields["prompt_cache_key"]), Instructions: digest([]byte("instructions"), fields["instructions"]),
		Tools: digest([]byte("tools"), fields["tools"]), InputBytes: len(fields["input"])}
	parts := [][]byte{[]byte("parameters")}
	for _, key := range []string{"model", "reasoning", "tool_choice", "parallel_tool_calls", "text", "temperature", "top_p", "max_output_tokens"} {
		parts = append(parts, []byte(key), fields[key])
	}
	p.Parameters = digest(parts...)
	jsonpeek.ArrayValues(fields["input"], func([]byte) bool { p.InputItems++; return true })
	chain := hmac.New(sha256.New, promptKey.key[:])
	chain.Write([]byte("input\x00"))
	index := 0
	var sum [32]byte
	p.Prefixes = make([]audit.PrefixDiagnostic, 0, min(p.InputItems, 64))
	jsonpeek.ArrayValues(fields["input"], func(item []byte) bool {
		index++
		chain.Write(item)
		chain.Write([]byte{0})
		if index <= 32 || index > p.InputItems-32 {
			digest := chain.Sum(sum[:0])
			p.Prefixes = append(p.Prefixes, audit.PrefixDiagnostic{Items: index, Digest: hex.EncodeToString(digest[:16])})
		}
		return true
	})
	// A string input is a complete single item too.
	if p.InputItems == 0 && len(fields["input"]) > 0 {
		p.Prefixes = []audit.PrefixDiagnostic{{Items: 0, Digest: digest([]byte("non_array_input"), fields["input"])}}
	}
	return context.WithValue(ctx, promptKeyContext{}, p)
}
