package sdk

import "strings"

// No provider publishes what a model accepts. There is no capability endpoint to
// ask, so what is known has to be carried here, and the honest way to say so is
// to start from what the adapter can express and narrow it per model family
// (CHANGE-077, REQ-049(3)).
//
// Two rules cause most of the confusion and are encoded as such:
//
//   - A model that thinks before it answers takes only the default sampling
//     parameters. Asking for a temperature, a top_p or either penalty is refused
//     by OpenAI on chat/completions, and by Anthropic whenever extended thinking
//     is enabled. So a reasoning family does not support temperature.
//   - A model that cannot think does not support a reasoning level. Reporting it
//     as capable is how a setting gets accepted here and then dropped there.

// adapterKnobs is what each adapter's translation is able to express at all,
// before any model is considered. It is here rather than in the four adapters so
// there is one answer to compare against, and so a knob cannot be claimed by one
// adapter and quietly missing from another.
var adapterKnobs = map[AdapterID]Model{
	AdapterOpenAI: {
		SupportsTopP: true, SupportsStopSequences: true,
		SupportsPresencePenalty: true, SupportsFrequencyPenalty: true, SupportsSeed: true,
	},
	AdapterOpenCode: {
		SupportsTopP: true, SupportsStopSequences: true,
		SupportsPresencePenalty: true, SupportsFrequencyPenalty: true, SupportsSeed: true,
	},
	AdapterAnthropic: {
		SupportsTopP: true, SupportsTopK: true, SupportsStopSequences: true,
	},
	AdapterGemini: {
		SupportsTopP: true, SupportsTopK: true, SupportsStopSequences: true,
	},
}

// ModelCapabilities narrows what an adapter can express down to what one model
// accepts. The base comes from the adapter, because an adapter is the honest
// answer to "what can this integration say at all"; the rules below only remove
// what this particular model would refuse.
func ModelCapabilities(adapter AdapterID, base Model) Model {
	baseline, ok := adapterKnobs[adapter]
	if !ok {
		baseline = adapterKnobs[AdapterOpenAI]
	}
	base.SupportsTopP = baseline.SupportsTopP
	base.SupportsTopK = baseline.SupportsTopK
	base.SupportsStopSequences = baseline.SupportsStopSequences
	base.SupportsPresencePenalty = baseline.SupportsPresencePenalty
	base.SupportsFrequencyPenalty = baseline.SupportsFrequencyPenalty
	base.SupportsSeed = baseline.SupportsSeed

	base.SupportsThinking = base.SupportsThinking || thinksByFamily(base.ID)
	if base.SupportsThinking {
		// Reasoning pins the sampling knobs, so a temperature is not one of the
		// things this model can be told.
		base.SupportsTemperature = false
	}
	base.SupportsTools = base.SupportsTools || base.SupportsThinking
	return base
}

// thinksByFamily names the model families that expose a reasoning control.
// Matching is on the part of the id before any date or size suffix, because
// providers append those themselves: "gpt-5-2025-08-07" is still a gpt-5.
func thinksByFamily(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	for _, prefix := range []string{
		"o1", "o3", "o4", // OpenAI reasoning families
		"gpt-5", "gpt-oss", // and the current GPT line
		"claude-3-7", "claude-4", // Anthropic extended thinking arrived at 3.7
		"claude-opus-4", "claude-sonnet-4", "claude-haiku-4",
		"gemini-2.5", // Gemini thinking budget
		"deepseek-r1", "deepseek-reasoner", "qwq",
	} {
		if id == prefix || strings.HasPrefix(id, prefix+"-") {
			return true
		}
	}
	return false
}
