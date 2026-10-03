package sdk

import (
	"github.com/Tulipskun/ai-engine/provider"
	"github.com/Tulipskun/ai-engine/session"
)

const defaultContextWindowTokens = 58000

// buildContextWindow returns the newest complete interaction groups that fit
// within the token budget. A group starts at a user turn and contains the
// model response plus any tool calls/results that follow it. Groups are never
// split, so provider-native reasoning and tool state stay consistent.
func buildContextWindow(history []provider.Turn, maxTokens int) []provider.Turn {
	if len(history) == 0 {
		return nil
	}
	if maxTokens <= 0 {
		maxTokens = defaultContextWindowTokens
	}

	groups := make([][]provider.Turn, 0, len(history))
	current := make([]provider.Turn, 0, 4)
	for _, turn := range history {
		if turn.Role == provider.RoleUser && len(current) > 0 {
			groups = append(groups, current)
			current = make([]provider.Turn, 0, 4)
		}
		current = append(current, session.CloneTurn(turn))
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}

	selectedStart := len(groups)
	total := 0
	for i := len(groups) - 1; i >= 0; i-- {
		groupTokens := estimateTurnGroupTokens(groups[i])
		if selectedStart < len(groups) && total+groupTokens > maxTokens {
			break
		}
		selectedStart = i
		total += groupTokens
		if total >= maxTokens {
			break
		}
	}

	var out []provider.Turn
	for _, group := range groups[selectedStart:] {
		out = append(out, group...)
	}
	return out
}

func estimateTurnGroupTokens(group []provider.Turn) int {
	total := 0
	for _, turn := range group {
		total += estimateTurnTokens(turn)
	}
	return total
}

func estimateTurnTokens(turn provider.Turn) int {
	chars := 0
	for _, part := range turn.Content {
		chars += len([]rune(part.Text))
	}
	if turn.ToolCall != nil {
		chars += len(turn.ToolCall.ID) + len(turn.ToolCall.Name) + len(turn.ToolCall.Arguments)
	}
	if turn.ToolResult != nil {
		chars += len(turn.ToolResult.ID) + len(turn.ToolResult.Content)
	}
	if turn.Reasoning != nil {
		chars += len(turn.Reasoning.ID) + len(turn.Reasoning.Text)
	}
	if chars == 0 {
		return 1
	}
	return (chars + 3) / 4
}
