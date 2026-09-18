package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

// UseJournal is startup wiring. Config updates remain atomic; the journal and
// its cipher stay fixed for the lifetime of this instance.
func (r *ReasoningReplay) UseJournal(j repository.ConversationJournal, retention, lease time.Duration) {
	r.journal = j
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	if lease <= 0 {
		lease = 30 * time.Minute
	}
	r.retention, r.requestLease = retention, lease
}
func (r *ReasoningReplay) Persistent() bool { return r != nil && r.Enabled() && r.journal != nil }
func journalHash(s string) string           { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func journalScope(model, key string) repository.JournalScope {
	return repository.JournalScope{Model: model, Key: key, Normalizer: historydomain.JournalNormalizerVersion}
}

// PreparedHistory belongs to exactly one physical request, reserved before its
// send. Never reuse it after account/plane switching or a generation reset.
type PreparedHistory struct {
	replay      *ReasoningReplay
	reservation repository.JournalReservation
	restored    int
}

func (p *PreparedHistory) Outcome() string {
	if p == nil {
		return "disabled"
	}
	return p.reservation.Outcome
}
func (p *PreparedHistory) Generation() int64 {
	if p == nil {
		return 0
	}
	return p.reservation.Ticket.Generation
}

// Prepare resolves full-prefix lineage, not completion order or a text-only
// anchor. Missing opaque reasoning is restored at the original visible index.
func (r *ReasoningReplay) Prepare(ctx context.Context, model, key string, body []byte, options ...historydomain.ReplayPreparation) (output []byte, result historydomain.Prepared, returnedErr error) {
	defer requestdiag.Stage(ctx, "history_prepare", time.Now())
	defer func() {
		if returnedErr != nil {
			requestdiag.Failure(ctx, "history", "history_prepare", journalFailureReason(returnedErr))
			returnedErr = fmt.Errorf("%w: %w", historydomain.ErrHistoryPrepare, returnedErr)
		}
	}()
	if !r.Persistent() || key == "" {
		next := r.Apply(ctx, model, key, body)
		if key != "" {
			if err := r.authorizePriorCache(ctx, model, key, next, options); err != nil {
				return body, nil, err
			}
		}
		return next, nil, nil
	}
	// Borrow validated input fields: media data must not be copied into both
	// root and item RawMessages before it can be hashed or stored.
	if !json.Valid(body) || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return body, nil, fmt.Errorf("invalid history request")
	}
	budget := responsebuffer.FromContext(ctx)
	ctx = responsebuffer.WithContext(ctx, budget)
	// Structural metadata remains live during reservation and restoration. The
	// byte workspaces below are separate, sequential phases of the request.
	metadata, err := budget.Reserve(responsebuffer.BorrowedJSONWorkspaceSize(body))
	if err != nil {
		return body, nil, err
	}
	defer metadata.Release()
	root := make(map[string]json.RawMessage)
	jsonpeek.ObjectFields(body, func(key, value []byte) bool { root[string(key)] = value; return true })
	var raw []json.RawMessage
	input := bytes.TrimSpace(root["input"])
	if len(input) > 0 && input[0] == '[' {
		jsonpeek.ArrayValues(input, func(value []byte) bool { raw = append(raw, value); return true })
	} else if !bytes.Equal(input, []byte("null")) {
		workspace, err := budget.Reserve(4 * len(input))
		if err != nil {
			return body, nil, err
		}
		defer workspace.Release()
		var text string
		if json.Unmarshal(input, &text) != nil {
			return body, nil, fmt.Errorf("invalid history input")
		}
		item, _ := json.Marshal(map[string]string{"role": "user", "content": text})
		raw = []json.RawMessage{item}
	}
	largest := 0
	for _, item := range raw {
		largest = max(largest, len(item))
	}
	workspace, err := budget.Reserve(4 * largest)
	if err != nil {
		return body, nil, err
	}
	defer workspace.Release()
	var previous string
	_ = json.Unmarshal(root["previous_response_id"], &previous)
	// Conversation identity follows exact visible history, independently of the
	// current tool/instruction settings. Account, plane and generation still isolate it.
	baseHash := journalHash("conversation-visible-v1")
	hash := baseHash
	inputReasoning := map[int][][]byte{}
	reasoningState := responsebuffer.NewState(budget, len(body)*2)
	defer reasoningState.Close()
	var visible [][]byte
	var prefixes, hashes []string
	for _, item := range raw {
		canonical, isReasoning, err := canonicalJournalItem(item)
		if err != nil {
			return body, nil, err
		}
		if isReasoning {
			if err := reasoningState.Grow(2*len(item), 0); err != nil {
				return body, nil, err
			}
			normalized, ok := normalizeJournalReasoning(item)
			if !ok {
				return body, nil, fmt.Errorf("invalid input reasoning")
			}
			inputReasoning[len(visible)] = append(inputReasoning[len(visible)], normalized)
			continue
		}
		itemHash := journalHash(string(canonical))
		hash = journalHash(hash + itemHash)
		hashes = append(hashes, itemHash)
		prefixes = append(prefixes, hash)
		visible = append(visible, item)
	}
	// Canonical bytes are no longer retained. SQL decryption gets the same
	// budget without holding the per-item encoding workspace simultaneously.
	workspace.Release()
	var legacyScopes []repository.JournalScope
	for _, option := range options {
		for _, oldKey := range option.LegacyKeys {
			legacyScopes = append(legacyScopes, journalScope(model, oldKey))
		}
	}
	reservation, err := r.journal.Reserve(ctx, repository.JournalReserve{LegacyScopes: legacyScopes, Scope: journalScope(model, key), Token: uuid.NewString(), ParentResponseID: previous, Incremental: previous != "", Input: visible, InputReasoning: inputReasoning, BaseHash: baseHash, ItemHash: CanonicalHistoryItemHash, Prefixes: prefixes, ItemHashes: hashes, Now: r.now().UTC(), Retention: r.retention, Lease: r.requestLease})
	if err != nil {
		r.journalObserve(model, key, "reserve", historydomain.HistoryFailureReason(err), 0, 0)
		return body, nil, err
	}
	// A pre-upgrade client may supply visible history whose opaque prefix is
	// already gone. Record that boundary explicitly; only subsequent accepted
	// turns carry the durable continuity guarantee.
	if reservation.Outcome == "new_history" {
		for _, item := range raw {
			var entry struct {
				Role string `json:"role"`
				Type string `json:"type"`
			}
			_ = json.Unmarshal(item, &entry)
			if entry.Role == "assistant" || entry.Type == "function_call" || entry.Type == "custom_tool_call" {
				reservation.Outcome = "legacy_history_missing"
				break
			}
		}
	}
	prepared := &PreparedHistory{replay: r, reservation: reservation}
	if previous == "" && len(reservation.Turns) == 0 {
		if err := r.authorizePriorContext(ctx, model, root, raw, prefixes, options); err != nil {
			prepared.Discard()
			return body, nil, err
		}
	}
	restored := 0
	if previous == "" && len(reservation.Turns) > 0 {
		// Account for decoded turns and for opaque items added while encoding
		// the result; restored reasoning can be larger than the client input.
		turnBytes := 0
		for _, turn := range reservation.Turns {
			for _, item := range turn.Input {
				turnBytes += len(item)
			}
			for _, item := range turn.Output {
				turnBytes += len(item)
			}
		}
		restoreWorkspace, reserveErr := budget.Reserve(2 * (len(body) + turnBytes))
		if reserveErr != nil {
			prepared.Discard()
			return nil, nil, reserveErr
		}
		defer restoreWorkspace.Release()
		var next []byte
		next, restored, err = restoreJournalItems(root, raw, reservation.Turns)
		if restored > 0 {
			body = next
		}
		if err != nil {
			prepared.Discard()
			return nil, nil, err
		}
	}
	prepared.reservation.Turns = nil
	prepared.restored = restored
	r.journalObserve(model, key, "restore", reservation.Outcome, reservation.Ticket.Generation, restored)
	return body, prepared, nil
}

func canonicalJournalItem(raw []byte) ([]byte, bool, error) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return nil, false, fmt.Errorf("invalid history item")
	}
	var kind, role string
	_ = json.Unmarshal(item["type"], &kind)
	_ = json.Unmarshal(item["role"], &role)
	if kind == "reasoning" {
		return nil, true, nil
	}
	delete(item, "id")
	delete(item, "status")
	if kind == "message" || kind == "" && role != "" {
		item["type"] = json.RawMessage(`"message"`)
		// Text representations differ between Chat, Messages and Responses. This
		// normalization compares exact content, preserving whitespace and order.
		if parts, ok := assistantParts(item["content"]); ok {
			normalized := make([]map[string]string, 0, len(parts))
			for _, p := range parts {
				normalized = append(normalized, map[string]string{"type": p.partType, "value": p.value})
			}
			item["content"], _ = json.Marshal(normalized)
		}
	}
	if kind == "function_call" || kind == "function_call_output" || kind == "custom_tool_call" || kind == "custom_tool_call_output" {
		var call string
		_ = json.Unmarshal(item["call_id"], &call)
		item["call_id"], _ = json.Marshal(strings.TrimPrefix(call, "toolu_"))
	}
	b, e := json.Marshal(item)
	return b, false, e
}

func journalItemIsReasoning(item []byte) (bool, error) {
	value := bytes.TrimSpace(item)
	if len(value) == 0 || value[0] != '{' || !json.Valid(value) {
		return false, fmt.Errorf("invalid history item")
	}
	return jsonpeek.RootStringFieldScan(value, "type") == "reasoning", nil
}

func restoreJournalItems(root map[string]json.RawMessage, input []json.RawMessage, turns []repository.JournalTurn) ([]byte, int, error) {
	pending := map[int][][]byte{}
	for _, turn := range turns {
		inputVisible := 0
		for _, item := range turn.Input {
			reasoning, e := journalItemIsReasoning(item)
			if e != nil {
				return nil, 0, e
			}
			if !reasoning {
				inputVisible++
			}
		}
		position := turn.InputCount - inputVisible
		for _, item := range turn.Input {
			reasoning, _ := journalItemIsReasoning(item)
			if reasoning {
				pending[position] = append(pending[position], item)
			} else {
				position++
			}
		}
		position = turn.InputCount
		for _, item := range turn.Output {
			reasoning, e := journalItemIsReasoning(item)
			if e != nil {
				return nil, 0, e
			}
			if reasoning {
				normalized, ok := normalizeJournalReasoning(item)
				if !ok {
					return nil, 0, fmt.Errorf("invalid persisted reasoning")
				}
				pending[position] = append(pending[position], normalized)
			} else {
				position++
			}
		}
	}
	var next []json.RawMessage
	position := 0
	restored := 0
	// Client reasoning at a visible boundary must equal the persisted item at
	// that boundary. A conflicting cipher is a new history, never silently mixed.
	existing := map[int][][]byte{}
	for _, item := range input {
		reasoning, e := journalItemIsReasoning(item)
		if e != nil {
			return nil, 0, e
		}
		if reasoning {
			existing[position] = append(existing[position], item)
		} else {
			position++
		}
	}
	position = 0
	inserted := map[int]bool{}
	inject := func() error {
		if inserted[position] {
			return nil
		}
		inserted[position] = true
		seenCipher := map[string]bool{}
		for _, saved := range pending[position] {
			var s struct {
				Encrypted string `json:"encrypted_content"`
			}
			_ = json.Unmarshal(saved, &s)
			if s.Encrypted != "" && seenCipher[s.Encrypted] {
				continue
			}
			seenCipher[s.Encrypted] = true
			found := false
			for _, client := range existing[position] {
				var c struct {
					Encrypted string `json:"encrypted_content"`
				}
				_ = json.Unmarshal(client, &c)
				if c.Encrypted == s.Encrypted {
					found = true
				}
			}
			if !found && len(existing[position]) > 0 {
				return fmt.Errorf("%w: reasoning_conflict", historydomain.ErrHistoryAmbiguous)
			}
			if !found {
				next = append(next, json.RawMessage(saved))
				restored++
			}
		}
		return nil
	}
	for _, item := range input {
		if e := inject(); e != nil {
			return nil, 0, e
		}
		next = append(next, item)
		reasoning, _ := journalItemIsReasoning(item)
		if !reasoning {
			position++
		}
	}
	if e := inject(); e != nil {
		return nil, 0, e
	}
	if restored == 0 {
		return nil, 0, nil
	}
	encoded, e := json.Marshal(next)
	if e != nil {
		return nil, 0, e
	}
	root["input"] = encoded
	body, e := json.Marshal(root)
	return body, restored, e
}

func (p *PreparedHistory) store(ctx context.Context, payload []byte) error {
	if p == nil {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil {
		return journalCommitFailure("validate", "malformed_payload", nil)
	}
	if nested, ok := root["response"]; ok {
		if json.Unmarshal(nested, &root) != nil {
			return journalCommitFailure("validate", "malformed_response", nil)
		}
	}
	var status, responseID string
	_ = json.Unmarshal(root["status"], &status)
	_ = json.Unmarshal(root["id"], &responseID)
	if status != "" && status != "completed" {
		return journalCommitFailure("validate", "incomplete_response", nil)
	}
	var items []json.RawMessage
	if json.Unmarshal(root["output"], &items) != nil {
		return journalCommitFailure("validate", "missing_output", nil)
	}
	output := make([][]byte, 0, len(items))
	hash := p.reservation.Ticket.InputHash
	count := p.reservation.Ticket.InputCount
	for _, raw := range items {
		canonical, reasoning, e := canonicalJournalItem(raw)
		if e != nil {
			return journalCommitFailure("validate", "invalid_output", nil)
		}
		if reasoning {
			if _, ok := normalizeJournalReasoning(raw); !ok {
				return journalCommitFailure("validate", "invalid_opaque_reasoning", nil)
			}
		}
		if !reasoning {
			hash = journalHash(hash + journalHash(string(canonical)))
			count++
		}
		output = append(output, append([]byte(nil), raw...))
	}
	commit := repository.JournalCommit{Ticket: p.reservation.Ticket, ResponseID: responseID, PrefixHash: hash, TotalCount: count, Output: output}
	err := p.commitStoredOutput(ctx, commit)
	outcome := "committed"
	if err != nil {
		outcome = historydomain.HistoryFailureReason(err)
	}
	p.replay.journalObserve(p.reservation.Ticket.Scope.Model, p.reservation.Ticket.Scope.Key, "commit", outcome, p.Generation(), len(output))
	if err != nil {
		return journalCommitFailure("store", journalFailureReason(err), err)
	}
	return nil
}
func (r *ReasoningReplay) journalObserve(model, key, stage, outcome string, generation int64, items int) {
	r.logger.Info("conversation_continuity", "scope_hash", journalHash(model+":"+key), "stage", stage, "outcome", outcome, "generation", generation, "items", items, "normalizer", historydomain.JournalNormalizerVersion)
	perfmetrics.Default.Inc("conversation_continuity_total", perfmetrics.Labels{Subsystem: "history", Outcome: outcome})
}

func (p *PreparedHistory) Discard() {
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := p.replay.journal.Release(ctx, p.reservation.Ticket); e != nil {
		p.replay.journalObserve(p.reservation.Ticket.Scope.Model, p.reservation.Ticket.Scope.Key, "release", historydomain.HistoryFailureReason(e), p.Generation(), 0)
	}
}

// Reset commits a compaction boundary only if the original generation remains
// current. A delayed compaction cannot invalidate a newer conversation.
func (p *PreparedHistory) Reset() error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.reset(ctx)
}

func (p *PreparedHistory) reset(ctx context.Context) error {
	ticket := p.reservation.Ticket
	if e := p.replay.journal.Reset(ctx, ticket.Scope, p.replay.now().UTC(), ticket.Generation); e != nil {
		err := journalCommitFailure("reset", journalFailureReason(e), e)
		p.observeCommitFailure(err)
		return err
	}
	p.replay.clearLegacy(ctx, ticket.Scope.Model, ticket.Scope.Key)
	return nil
}
func (r *ReasoningReplay) Reset(ctx context.Context, model, key string) error {
	if r.Persistent() {
		if e := r.journal.Reset(ctx, journalScope(model, key), r.now().UTC()); e != nil {
			return e
		}
	}
	r.clearLegacy(ctx, model, key)
	return nil
}

func (p *PreparedHistory) RestoredItems() int {
	if p == nil {
		return 0
	}
	return p.restored
}
func (p *PreparedHistory) ScopeHash() string {
	if p == nil {
		return ""
	}
	s := p.reservation.Ticket.Scope
	return journalHash(s.Model + ":" + s.Key)
}

func normalizeJournalReasoning(item []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(item, &raw) != nil || raw == nil {
		return nil, false
	}
	var kind string
	if json.Unmarshal(raw["type"], &kind) != nil || kind != "reasoning" {
		return nil, false
	}
	// Durable history already has an authorized scope and exact lineage. The
	// provider owns its opaque encoding; byte entropy and a guessed minimum
	// ciphertext length cannot establish validity. Apply the same normalization
	// to native output, restored rows and client-carried copies of those rows.
	next := map[string]json.RawMessage{"type": json.RawMessage(`"reasoning"`), "summary": json.RawMessage(`[]`)}
	if encrypted := raw["encrypted_content"]; len(encrypted) > 0 && strings.TrimSpace(string(encrypted)) != "null" {
		var opaque string
		if json.Unmarshal(encrypted, &opaque) != nil || len(opaque) > maxReplayEncryptedLen {
			return nil, false
		}
		if opaque != "" {
			next["encrypted_content"] = encrypted
		}
	}
	for _, field := range []string{"id", "summary", "content"} {
		if value := raw[field]; len(value) > 0 && string(value) != "null" {
			next[field] = value
		}
	}
	data, err := json.Marshal(next)
	return data, err == nil
}

// CanonicalHistoryItemHash supplies the same visible-item identity to legacy migration.
func CanonicalHistoryItemHash(raw []byte) (string, bool, error) {
	b, r, e := canonicalJournalItem(raw)
	if e != nil {
		return "", r, e
	}
	return journalHash(string(b)), r, nil
}
