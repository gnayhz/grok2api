package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	compactionPrefix     = "g2a_compact_v1."
	compactionVersion    = 1
	maxCompactionSummary = 8 << 20
)

var ErrCompactionLossNotAuthorized = errors.New("compaction history cannot be decoded without discarding prior context")

// CompactionCipher supplies encryption mechanics; this history owner defines
// the portable envelope, validity, scope hint and input preparation rules.
type CompactionCipher interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type compactionEnvelope struct {
	Version int    `json:"version"`
	Session string `json:"session"`
	Summary string `json:"summary"`
}

type CompactionCodec struct{ cipher CompactionCipher }

func NewCompactionCodec(cipher CompactionCipher) *CompactionCodec {
	if cipher == nil {
		return nil
	}
	return &CompactionCodec{cipher: cipher}
}

func (c *CompactionCodec) Encode(session, summary string) (string, error) {
	if c == nil || c.cipher == nil {
		return "", fmt.Errorf("compaction codec unavailable")
	}
	if summary == "" || len(summary) > maxCompactionSummary {
		return "", fmt.Errorf("compaction summary size is invalid")
	}
	data, err := json.Marshal(compactionEnvelope{Version: compactionVersion, Session: session, Summary: summary})
	if err != nil {
		return "", err
	}
	encrypted, err := c.cipher.Encrypt(string(data))
	if err != nil {
		return "", err
	}
	return compactionPrefix + encrypted, nil
}

func (c *CompactionCodec) decode(session, blob string) (summary string, owned, drifted bool, err error) {
	if !strings.HasPrefix(blob, compactionPrefix) {
		return "", false, false, nil
	}
	if c == nil || c.cipher == nil {
		return "", true, false, fmt.Errorf("compaction codec unavailable")
	}
	plain, err := c.cipher.Decrypt(strings.TrimPrefix(blob, compactionPrefix))
	if err != nil {
		return "", true, false, fmt.Errorf("decode gateway compaction blob: %w", err)
	}
	var envelope compactionEnvelope
	if err = json.Unmarshal([]byte(plain), &envelope); err != nil {
		return "", true, false, fmt.Errorf("decode gateway compaction payload: %w", err)
	}
	if envelope.Version != compactionVersion || envelope.Summary == "" || len(envelope.Summary) > maxCompactionSummary {
		return "", true, false, fmt.Errorf("gateway compaction payload is invalid")
	}
	// The encrypted summary is portable; the upstream cache/session key is a
	// reuse hint, not an authorization boundary for a client-carried envelope.
	return envelope.Summary, true, envelope.Session != session, nil
}

// CompactionPreparation is a side-effect-free proposal. Unavailable counts
// input states replaced by an explicit boundary; using such a body requires
// the logical request owner's permission. No journal generation is changed.
type CompactionPreparation struct {
	Body           []byte
	Unavailable    int
	SessionDrifted int
}

func (c *CompactionCodec) Prepare(body []byte, session string) (CompactionPreparation, error) {
	result := CompactionPreparation{Body: body}
	// ASCII letters in JSON strings may also be spelled with Unicode escapes.
	// These are the only escaped spellings of "compaction"; prose false positives
	// are harmless, but this prefilter must never skip a semantic compaction item.
	if !bytes.Contains(body, []byte(`"compaction"`)) && !bytes.Contains(body, []byte(`\u`)) {
		return result, nil
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || payload == nil {
		return result, nil
	} // protocol parser owns invalid JSON
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return result, nil
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return result, nil
	}
	changed := false
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "compaction" {
			continue
		}
		blob, _ := item["encrypted_content"].(string)
		summary, owned, drifted, err := c.decode(session, blob)
		role := "user"
		if err != nil || !owned {
			result.Unavailable++
			role = "developer"
			if owned {
				summary = "A prior compacted context could not be decoded by this gateway instance. Continue from the retained conversation messages."
			} else {
				summary = "A compacted context created by another provider cannot be decoded by Grok Build. Continue from the retained conversation messages."
			}
		} else if drifted {
			result.SessionDrifted++
		}
		// The Responses carrier is an explicit visible message; it never fabricates
		// provider opaque data. Other items retain exact JSON number lexemes.
		items[i] = compactionMessage{Type: "message", Role: role,
			Content: []compactionText{{Type: "input_text", Text: summary}}}
		changed = true
	}
	if !changed {
		return result, nil
	}
	payload["input"] = items
	var err error
	result.Body, err = json.Marshal(payload)
	return result, err
}

type compactionMessage struct {
	Type    string           `json:"type"`
	Role    string           `json:"role"`
	Content []compactionText `json:"content"`
}
type compactionText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
