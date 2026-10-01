package sdk

import "testing"

// The phone can only hide a setting it was told about. Before this, a model's
// reasoning capability was discovered and then dropped on the way out, and the
// remaining knobs were never reported at all (CHANGE-077).

func TestAReasoningModelReportsThatItCannotBeGivenATemperature(t *testing.T) {
	got := ModelCapabilities(AdapterOpenAI, Model{ID: "gpt-5-mini", SupportsTemperature: true, SupportsStreaming: true})
	if !got.SupportsThinking {
		t.Error("a gpt-5 model was reported as not thinking")
	}
	if got.SupportsTemperature {
		t.Error("a reasoning model was reported as accepting a temperature; the provider refuses one")
	}
}

func TestAPlainModelKeepsTemperatureAndReportsNoReasoning(t *testing.T) {
	got := ModelCapabilities(AdapterOpenAI, Model{ID: "gpt-4o", SupportsTemperature: true})
	if got.SupportsThinking {
		t.Error("gpt-4o was reported as thinking")
	}
	if !got.SupportsTemperature {
		t.Error("gpt-4o lost its temperature")
	}
}

// A dated id is still the same family: providers append the date themselves.
func TestADatedModelIdIsStillRecognised(t *testing.T) {
	for _, id := range []string{"gpt-5-2025-08-07", "o3-mini", "claude-sonnet-4-20250514", "gemini-2.5-flash"} {
		if got := ModelCapabilities(AdapterAnthropic, Model{ID: id, SupportsTemperature: true}); !got.SupportsThinking {
			t.Errorf("%s was not recognised as a reasoning model", id)
		}
	}
	for _, id := range []string{"gpt-4o", "gpt-4-turbo", "claude-3-5-sonnet", "llama-3.1-70b"} {
		if got := ModelCapabilities(AdapterAnthropic, Model{ID: id}); got.SupportsThinking {
			t.Errorf("%s was wrongly reported as a reasoning model", id)
		}
	}
}

// The names must not match by accident: "o1" is a family but "o1-preview-16k"
// is not, and neither is anything that merely contains a digit.
func TestFamilyMatchingDoesNotFireOnUnrelatedNames(t *testing.T) {
	for _, id := range []string{"", "  ", "titan-text-express", "command-r-plus", "some-model"} {
		if thinksByFamily(id) {
			t.Errorf("%q was treated as a reasoning model", id)
		}
	}
}

// Each knob is reported only where it can actually be sent.
func TestKnobsAreReportedPerAdapter(t *testing.T) {
	cases := []struct {
		adapter AdapterID
		want    Model
	}{
		{AdapterOpenAI, Model{SupportsTopP: true, SupportsStopSequences: true, SupportsPresencePenalty: true, SupportsFrequencyPenalty: true, SupportsSeed: true}},
		{AdapterAnthropic, Model{SupportsTopP: true, SupportsTopK: true, SupportsStopSequences: true}},
		{AdapterGemini, Model{SupportsTopP: true, SupportsTopK: true, SupportsStopSequences: true}},
		{AdapterOpenCode, Model{SupportsTopP: true, SupportsStopSequences: true, SupportsPresencePenalty: true, SupportsFrequencyPenalty: true, SupportsSeed: true}},
	}
	for _, tc := range cases {
		got := ModelCapabilities(tc.adapter, Model{ID: "plain-model"})
		if got.SupportsTopP != tc.want.SupportsTopP || got.SupportsTopK != tc.want.SupportsTopK ||
			got.SupportsStopSequences != tc.want.SupportsStopSequences ||
			got.SupportsPresencePenalty != tc.want.SupportsPresencePenalty ||
			got.SupportsFrequencyPenalty != tc.want.SupportsFrequencyPenalty ||
			got.SupportsSeed != tc.want.SupportsSeed {
			t.Errorf("%s reported %+v, want %+v", tc.adapter, got, tc.want)
		}
	}
}

// A reasoning model is always a tool model; the reverse is not true.
func TestReasoningImpliesTools(t *testing.T) {
	got := ModelCapabilities(AdapterOpenAI, Model{ID: "gpt-5"})
	if !got.SupportsTools {
		t.Error("a reasoning model was reported as unable to call tools")
	}
	plain := ModelCapabilities(AdapterOpenAI, Model{ID: "gpt-4o"})
	if plain.SupportsTools {
		t.Error("a model was reported as calling tools without an adapter saying it can")
	}
}
