package sdk

import (
	"errors"
	"testing"
)

func TestOpenSessionPersistsHistory(t *testing.T) {
	path := t.TempDir() + "/session.db"
	cfg := SessionConfig{ID: "s1", Provider: "test", Model: "model", KeyIndex: 0, ThinkingLevel: ThinkingNone}
	keys := NewKeyPool("key")
	s, err := OpenSession(path, cfg, keys)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}})
	s.Append(Turn{Role: RoleModel, Content: []ContentPart{{Type: ContentText, Text: "world"}}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(path, cfg, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h := reopened.History()
	if len(h) != 2 || h[0].Content[0].Text != "hello" || h[1].Content[0].Text != "world" {
		t.Fatalf("unexpected history: %#v", h)
	}
}

func TestSessionRollbackPersists(t *testing.T) {
	path := t.TempDir() + "/session.db"
	cfg := SessionConfig{ID: "s1", Provider: "test", Model: "model", KeyIndex: 0}
	s, err := OpenSession(path, cfg, NewKeyPool("key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Append(Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "keep"}}})
	s.Append(Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "rollback"}}})
	s.ReplaceHistory(s.History()[:1])
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSession(path, cfg, NewKeyPool("key"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if h := reopened.History(); len(h) != 1 || h[0].Content[0].Text != "keep" {
		t.Fatalf("rollback was not persisted: %#v", h)
	}
}

func TestSessionDBRecordsAttemptAndUsageWithoutRawPayloads(t *testing.T) {
	path := t.TempDir() + "/session.db"
	cfg := SessionConfig{ID: "s1", Provider: "test", Model: "model", KeyIndex: 0}
	s, err := OpenSession(path, cfg, NewKeyPool("key"))
	if err != nil {
		t.Fatal(err)
	}

	req := Request{
		Provider:        "test",
		Model:           "model",
		SystemPrompt:    "system",
		Messages:        []Turn{{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}}},
		Tools:           []Tool{{Name: "read"}},
		ThinkingLevel:   ThinkingLow,
		MaxOutputTokens: 100,
		Stream:          true,
	}
	id, err := s.RecordRequest(1, req)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("provider failed")
	resp := Response{
		Provider:     "test",
		Model:        "model",
		FinishReason: "error",
		Usage: Usage{
			InputTokens:      100,
			OutputTokens:     25,
			TotalTokens:      125,
			CacheReadTokens:  80,
			CacheWriteTokens: 10,
		},
		Cache: CacheInfo{Hit: true, Layer: "prompt"},
	}
	if err := s.RecordResponse(id, resp, wantErr); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenSessionDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var success, inputTokens, outputTokens, totalTokens, cacheRead, cacheWrite, cacheHit int
	var recorded string
	if err := db.db.QueryRow(`
		SELECT success,input_tokens,output_tokens,total_tokens,cache_read_tokens,cache_write_tokens,cache_hit,error
		FROM attempts WHERE id=?`, id).Scan(
		&success, &inputTokens, &outputTokens, &totalTokens, &cacheRead, &cacheWrite, &cacheHit, &recorded,
	); err != nil {
		t.Fatal(err)
	}
	if success != 0 || recorded != wantErr.Error() {
		t.Fatalf("unexpected attempt record: success=%d error=%q", success, recorded)
	}
	if inputTokens != 100 || outputTokens != 25 || totalTokens != 125 || cacheRead != 80 || cacheWrite != 10 || cacheHit != 1 {
		t.Fatalf("unexpected usage: input=%d output=%d total=%d cache_read=%d cache_write=%d cache_hit=%d", inputTokens, outputTokens, totalTokens, cacheRead, cacheWrite, cacheHit)
	}

	usage, err := db.LoadUsage("s1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 25 || usage.TotalTokens != 125 || usage.CacheReadTokens != 80 || usage.CacheWriteTokens != 10 {
		t.Fatalf("unexpected session usage: %#v", usage)
	}

	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('requests','responses')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy raw request/response tables still exist: %d", count)
	}
}
