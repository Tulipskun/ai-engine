package sdk

import "testing"

// The two providers disagree about whether the prompt total contains the cached
// part, and the phone is the one that has to draw the difference. These lock in
// which arithmetic each convention gets.
func TestFreshInputTokensFollowsTheProviderConvention(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		fresh int
		total int
	}{
		{"openai counts cache inside the prompt", Usage{InputTokens: 100, CacheReadTokens: 80, InputIncludesCache: true}, 20, 100},
		{"anthropic counts cache beside the prompt", Usage{InputTokens: 20, CacheReadTokens: 80}, 20, 100},
		{"a cache larger than the prompt does not go negative", Usage{InputTokens: 10, CacheReadTokens: 40, InputIncludesCache: true}, 0, 10},
		{"cache written counts toward the total only when separate", Usage{InputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 30}, 20, 130},
		{"openai total is the prompt as reported", Usage{InputTokens: 100, CacheReadTokens: 80, CacheWriteTokens: 5, InputIncludesCache: true}, 20, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.FreshInputTokens(); got != tc.fresh {
				t.Errorf("FreshInputTokens() = %d, want %d", got, tc.fresh)
			}
			if got := tc.usage.TotalInputTokens(); got != tc.total {
				t.Errorf("TotalInputTokens() = %d, want %d", got, tc.total)
			}
		})
	}
}
