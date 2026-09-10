package relational

import (
	"bytes"
	"context"
	"encoding/json"
	"gorm.io/gorm"
	"io"
	"testing"
)

func TestConversationToolToggleAndLegacyBrokenRootRecovery(t *testing.T) {
	_, j, _ := journalFixture(t)
	r := newJournalReplay(j)
	ctx := context.Background()
	history := []any{map[string]any{"role": "user", "content": "one"}}
	for turn := 0; turn < 3; turn++ {
		body := map[string]any{"input": history}
		if turn == 1 {
			body["tools"] = []any{map[string]any{"type": "web_search"}}
			body["tool_choice"] = "auto"
		} else {
			body["tool_choice"] = "none"
		}
		encoded, _ := json.Marshal(body)
		_, prepared, e := r.Prepare(ctx, "model", "toggle", encoded)
		if e != nil {
			t.Fatal(e)
		}
		if turn > 0 && (prepared.Outcome() != "append_ok" || prepared.RestoredItems() != turn) {
			t.Fatalf("turn=%d outcome=%s restored=%d", turn, prepared.Outcome(), prepared.RestoredItems())
		}
		assistant := map[string]any{"role": "assistant", "content": string(rune('a' + turn))}
		response, _ := json.Marshal(map[string]any{"id": string(rune('a' + turn)), "status": "completed", "output": []any{journalCipherItem(t), assistant}})
		capture, commit, discard := prepared.Capture(io.NopCloser(bytes.NewReader(response)), false)
		io.Copy(io.Discard, capture)
		if e = commit(); e != nil {
			t.Fatal(e)
		}
		capture.Close()
		discard()
		history = append(history, assistant, map[string]any{"role": "user", "content": "next"})
	}
	// Recreate the deployed bug: the second accepted node became a new root
	// containing the full visible input, while the original branch stayed intact.
	var nodes []conversationTurnModel
	j.db.Order("created_at").Find(&nodes)
	full := [][]byte{}
	for _, v := range history[:3] {
		b, _ := json.Marshal(v)
		full = append(full, b)
	}
	encrypted, e := j.encrypt(full)
	if e != nil {
		t.Fatal(e)
	}
	delta := len(encrypted) - len(nodes[1].EncryptedInput)
	if e = j.db.Model(&nodes[1]).Updates(map[string]any{"parent": "", "encrypted_input": encrypted}).Error; e != nil {
		t.Fatal(e)
	}
	if e = j.db.Model(&conversationSessionModel{}).Where("id = ?", nodes[0].Session).Updates(map[string]any{"visible_hash_version": 0, "bytes": gorm.Expr("bytes + ?", delta)}).Error; e != nil {
		t.Fatal(e)
	}
	j.db.Model(&conversationTurnModel{}).Where("session = ?", nodes[0].Session).Update("prefix_hash", "legacy-config-digest")
	body, _ := json.Marshal(map[string]any{"input": history, "tools": []any{map[string]any{"type": "x_search"}}})
	_, prepared, e := r.Prepare(ctx, "model", "toggle", body)
	if e != nil {
		t.Fatal(e)
	}
	defer prepared.Discard()
	if prepared.RestoredItems() != 3 {
		t.Fatalf("legacy repair restored=%d", prepared.RestoredItems())
	}
	j.db.First(&nodes[1], "id = ?", nodes[1].ID)
	if nodes[1].Parent != nodes[0].ID {
		t.Fatal("legacy root was not repaired")
	}

}
