package sdk

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Every knob the providers accept has to survive a restart. Before this the
// daemon only ever stored provider, model, thinking level and temperature, so a
// session came back with the rest of its settings quietly reset (CHANGE-077).

func TestGenerationSettingsPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	pool := NewKeyPool("key-1")
	s, err := OpenSession(path, SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	set := []func() error{
		func() error { return s.SetTemperature(0.3) },
		func() error { return s.SetThinkingLevel(ThinkingMedium) },
		func() error { return s.SetTopP(0.85) },
		func() error { return s.SetTopK(0.4) },
		func() error { return s.SetStopSequences([]string{" STOP ", "", "END"}) },
		func() error { return s.SetPresencePenalty(0.5) },
		func() error { return s.SetFrequencyPenalty(-1.25) },
		func() error { return s.SetSeed(42) },
		func() error { return s.SetMaxOutputTokens(4096) },
	}
	for i, apply := range set {
		if err := apply(); err != nil {
			t.Fatalf("setter %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(path, SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "ignored"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cfg := reopened.Config()

	wantFloat := map[string]struct {
		got *float64
		val float64
	}{
		"temperature":       {cfg.Temperature, 0.3},
		"top_p":             {cfg.TopP, 0.85},
		"top_k":             {cfg.TopK, 0.4},
		"presence_penalty":  {cfg.PresencePenalty, 0.5},
		"frequency_penalty": {cfg.FrequencyPenalty, -1.25},
	}
	for name, w := range wantFloat {
		if w.got == nil || *w.got != w.val {
			t.Errorf("%s = %v, want %v", name, w.got, w.val)
		}
	}
	if cfg.ThinkingLevel != ThinkingMedium {
		t.Errorf("thinking level = %q", cfg.ThinkingLevel)
	}
	if cfg.Seed == nil || *cfg.Seed != 42 {
		t.Errorf("seed = %v", cfg.Seed)
	}
	if cfg.MaxOutputTokens != 4096 {
		t.Errorf("max output tokens = %d", cfg.MaxOutputTokens)
	}
	// Blank entries are dropped and the rest keeps its order.
	if len(cfg.StopSequences) != 2 || cfg.StopSequences[0] != "STOP" || cfg.StopSequences[1] != "END" {
		t.Errorf("stop sequences = %#v", cfg.StopSequences)
	}
}

func TestClearingGenerationSettingsIsNotAValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	pool := NewKeyPool("key-1")
	s, err := OpenSession(path, SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetTemperature(0.9); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTopP(0.9); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearTemperature(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearTopP(); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	// Cleared means "let the provider decide", which is nil, not 0.
	if cfg.Temperature != nil || cfg.TopP != nil {
		t.Fatalf("cleared knobs = %v / %v, want nil", cfg.Temperature, cfg.TopP)
	}
}

// A database written before the new columns existed must open without losing the
// session, and its knobs must read as unset rather than as zero.
func TestGenerationSettingsOpenOnDatabaseWithoutTheNewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			key_index INTEGER NOT NULL,
			thinking_level TEXT NOT NULL,
			temperature REAL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cache_hits INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO sessions VALUES('legacy','anthropic','claude',0,'high',0.7,0,0,0,0,0,0,'then','then');`
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	sessionDB, err := OpenSessionDB(path)
	if err != nil {
		t.Fatalf("opening a database from before the new columns: %v", err)
	}
	defer sessionDB.Close()

	cfg, err := sessionDB.LoadSession("legacy")
	if err != nil {
		t.Fatalf("legacy session lost: %v", err)
	}
	if cfg.Model != "claude" || cfg.ThinkingLevel != ThinkingHigh {
		t.Fatalf("legacy values damaged: %+v", cfg)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
		t.Fatalf("legacy temperature = %v", cfg.Temperature)
	}
	if cfg.TopP != nil || cfg.TopK != nil || cfg.Seed != nil || len(cfg.StopSequences) != 0 {
		t.Fatalf("unset knobs read as values: %+v", cfg)
	}
	if cfg.MaxOutputTokens != 0 {
		t.Fatalf("max output tokens = %d, want 0", cfg.MaxOutputTokens)
	}
}

func TestGenerationSettingsRejectWhatAProviderWouldReject(t *testing.T) {
	s := &Session{}
	cases := []struct {
		name string
		set  func() error
	}{
		{"top_p above 1", func() error { return s.SetTopP(1.5) }},
		{"top_p below 0", func() error { return s.SetTopP(-0.1) }},
		{"top_k above 1", func() error { return s.SetTopK(2) }},
		{"presence penalty above 2", func() error { return s.SetPresencePenalty(2.5) }},
		{"frequency penalty below -2", func() error { return s.SetFrequencyPenalty(-3) }},
		{"negative max output tokens", func() error { return s.SetMaxOutputTokens(-1) }},
		{"unknown thinking level", func() error { return s.SetThinkingLevel("loud") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set(); err == nil {
				t.Fatal("expected the value to be refused")
			}
		})
	}
}

// A turn may override the session without changing the session.
func TestApplyGenerationLetsTheTurnWin(t *testing.T) {
	temperature, topP := 0.25, 0.75
	cfg := SessionConfig{
		ThinkingLevel: ThinkingHigh,
		Temperature:   &temperature,
		TopP:          &topP,
		StopSequences: []string{"END"},
	}

	empty := Request{}
	cfg.applyGeneration(&empty)
	if empty.ThinkingLevel != ThinkingHigh || empty.Temperature == nil || *empty.Temperature != 0.25 {
		t.Fatalf("session settings did not fill the gaps: %+v", empty)
	}
	if empty.TopP == nil || *empty.TopP != 0.75 || len(empty.StopSequences) != 1 {
		t.Fatalf("new knobs did not fill the gaps: %+v", empty)
	}

	turnTemperature := 0.9
	override := Request{Temperature: &turnTemperature, ThinkingLevel: ThinkingLow, StopSequences: []string{"HALT"}}
	cfg.applyGeneration(&override)
	if *override.Temperature != 0.9 || override.ThinkingLevel != ThinkingLow {
		t.Fatalf("the turn lost its own value: %+v", override)
	}
	if override.StopSequences[0] != "HALT" {
		t.Fatalf("the turn lost its own stop sequences: %+v", override.StopSequences)
	}
	// The gap it did not fill still comes from the session.
	if override.TopP == nil || *override.TopP != 0.75 {
		t.Fatalf("gap was not filled from the session: %+v", override)
	}
}

// A request that holds a pointer into the session must not be able to reach
// through it and rewrite the session's stored settings.
func TestConfigHandsOutAnIndependentCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	pool := NewKeyPool("key-1")
	s, err := OpenSession(path, SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetTopP(0.5); err != nil {
		t.Fatal(err)
	}

	cfg := s.Config()
	held := cfg.TopP
	req := Request{}
	cfg.applyGeneration(&req)
	*held = 0.99
	if req.TopP == nil || *req.TopP != 0.5 {
		t.Fatalf("writing through a copy changed the request's value: %v", req.TopP)
	}
	if again := s.Config(); *again.TopP != 0.5 {
		t.Fatalf("writing through a copy changed the session: %v", *again.TopP)
	}
}
