package mobile

import (
	"net/http"
	"testing"
)

// Before this the settings surface had no field for any generation knob, and the
// per-session PATCH used DisallowUnknownFields, so a phone that sent one got a
// 400 with no way to set a reasoning level or a temperature at all (CHANGE-077).

func generationAdmin(t *testing.T) (*fakeAdmin, http.Handler) {
	t.Helper()
	store := &fakeAdmin{}
	cache := &ramCache{}
	cache.Adopt("cf-token")
	return store, NewAdminHandler(store, NewGate(GateConfig{Verify: allowVerifier{}, Cache: cache}))
}

func TestAdminSavesTheGenerationKnobs(t *testing.T) {
	store, handler := generationAdmin(t)

	rec := adminRequest(t, handler, http.MethodPut, "/api/settings", `{
		"main": {"provider":"NousResearch","model":"m","generation":{
			"thinking_level":"high","temperature":0.35,"top_p":0.9,"top_k":0.4,
			"stop_sequences":["END"],"presence_penalty":0.5,"frequency_penalty":-1,
			"seed":7,"max_output_tokens":4096}},
		"sub": {"provider":"B.AI","model":"m","generation":{"temperature":0.1}},
		"sub_enabled": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d: %s", rec.Code, rec.Body)
	}
	g := store.settings.Main.Generation
	if g.ThinkingLevel != "high" || g.Temperature == nil || *g.Temperature != 0.35 {
		t.Fatalf("thinking/temperature = %+v", g)
	}
	if g.TopP == nil || *g.TopP != 0.9 || g.TopK == nil || *g.TopK != 0.4 {
		t.Fatalf("top_p/top_k = %+v", g)
	}
	if g.MaxOutputTokens != 4096 || g.Seed == nil || *g.Seed != 7 {
		t.Fatalf("cap/seed = %+v", g)
	}
	if len(g.StopSequences) != 1 || g.StopSequences[0] != "END" {
		t.Fatalf("stop = %+v", g.StopSequences)
	}
}

// A value no provider accepts must be refused, and refused before anything is
// stored, so a bad save leaves the working configuration alone.
func TestAdminRefusesAKnobAProviderWouldReject(t *testing.T) {
	store, handler := generationAdmin(t)
	store.settings.Main.Model = "already-there"

	cases := map[string]string{
		"temperature above range": `{"main":{"provider":"p","model":"m","generation":{"temperature":3}}}`,
		"top_p above range":       `{"main":{"provider":"p","model":"m","generation":{"top_p":1.5}}}`,
		"penalty out of range":    `{"main":{"provider":"p","model":"m","generation":{"frequency_penalty":-9}}}`,
		"unknown thinking level":  `{"main":{"provider":"p","model":"m","generation":{"thinking_level":"loud"}}}`,
		"negative output cap":     `{"main":{"provider":"p","model":"m","generation":{"max_output_tokens":-1}}}`,
		"sub agent out of range":  `{"main":{"provider":"p","model":"m"},"sub":{"generation":{"top_k":7}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := adminRequest(t, handler, http.MethodPut, "/api/settings", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
			}
			if store.settings.Main.Model != "already-there" {
				t.Fatalf("a refused save changed the stored settings: %+v", store.settings)
			}
		})
	}
}

func TestAdminStillRefusesAnUnknownField(t *testing.T) {
	_, handler := generationAdmin(t)
	rec := adminRequest(t, handler, http.MethodPut, "/api/settings", `{"main":{"provider":"p"},"temperature":0.5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
	}
}

// Sending nothing is not the same as sending zero, and must not be mistaken for
// asking for the settings to be cleared.
func TestAnEmptyGenerationBlockIsNotAValue(t *testing.T) {
	store, handler := generationAdmin(t)
	rec := adminRequest(t, handler, http.MethodPut, "/api/settings",
		`{"main":{"provider":"p","model":"m","generation":{}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body)
	}
	if !store.settings.Main.Generation.IsEmpty() {
		t.Fatalf("an empty block stored values: %+v", store.settings.Main.Generation)
	}
}

func TestGenerationSettingsValidateDirectly(t *testing.T) {
	zero := 0.0
	if err := ValidateGenerationSettings(GenerationSettings{Temperature: &zero}); err != nil {
		t.Errorf("zero temperature was refused: %v", err)
	}
	if err := ValidateGenerationSettings(GenerationSettings{ThinkingLevel: "none"}); err != nil {
		t.Errorf("thinking none was refused: %v", err)
	}
	negativeSeed := int64(-5)
	if err := ValidateGenerationSettings(GenerationSettings{Seed: &negativeSeed}); err != nil {
		t.Errorf("a negative seed was refused: %v", err)
	}
	positiveSeed := int64(5)
	if err := ValidateGenerationSettings(GenerationSettings{Seed: &positiveSeed}); err != nil {
		t.Errorf("a seed was refused: %v", err)
	}
	tooWarm := 2.5
	tooWide := -0.5
	tooManyOptions := 3.0
	tooPunished := -5.0
	for _, g := range []GenerationSettings{
		{Temperature: &tooWarm},
		{TopP: &tooWide},
		{TopK: &tooManyOptions},
		{PresencePenalty: &tooPunished},
		{ThinkingLevel: "extreme"},
		{MaxOutputTokens: -1},
	} {
		if err := ValidateGenerationSettings(g); err == nil {
			t.Errorf("%+v was accepted", g)
		}
	}
}

func TestEmptinessIsAboutValuesNotZeroes(t *testing.T) {
	zero := 0.0
	if (GenerationSettings{Temperature: &zero}).IsEmpty() {
		t.Error("a stored zero temperature was treated as nothing set")
	}
	if !(GenerationSettings{}).IsEmpty() {
		t.Error("an untouched block was not empty")
	}
	if (GenerationSettings{StopSequences: []string{""}}).IsEmpty() {
		t.Error("a blank stop sequence counted as set")
	}
}

func historyHandlerWithModels(t *testing.T, models *fakeModels) http.Handler {
	t.Helper()
	cache := &ramCache{}
	cache.Adopt("cf-token")
	return NewHistoryHandler(newFakeHistory(), NewGate(GateConfig{Verify: allowVerifier{}, Cache: cache}), models)
}

// The per-session route must accept the same knobs; it used to answer 400 because
// the body decoder refused any field it did not know.
func TestSessionPatchAcceptsGenerationAndStillRefusesNonsense(t *testing.T) {
	models := &fakeModels{}
	handler := historyHandlerWithModels(t, models)

	rec := historyRequest(t, handler, http.MethodPatch, "/api/sessions/s1", "cf-token",
		`{"generation":{"thinking_level":"low","temperature":0.2,"top_k":0.3}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH with generation = %d: %s", rec.Code, rec.Body)
	}
	choice, ok := models.choices["s1"]
	if !ok {
		t.Fatal("the generation block never reached the store")
	}
	if choice.Generation == nil || choice.Generation.ThinkingLevel != "low" {
		t.Fatalf("stored choice = %+v", choice.Generation)
	}
	if choice.Provider != "" || choice.Model != "" {
		t.Fatalf("sending only knobs rewrote the route: %+v", choice)
	}

	rec = historyRequest(t, handler, http.MethodPatch, "/api/sessions/s1", "cf-token",
		`{"generation":{"temperature":9}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad value = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestSessionPatchCanClearTheKnobs(t *testing.T) {
	models := &fakeModels{}
	handler := historyHandlerWithModels(t, models)

	rec := historyRequest(t, handler, http.MethodPatch, "/api/sessions/s1", "cf-token", `{"clear_generation":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body)
	}
	if choice := models.choices["s1"]; !choice.ClearGeneration {
		t.Fatal("the clear was not passed on to the store")
	}
}
