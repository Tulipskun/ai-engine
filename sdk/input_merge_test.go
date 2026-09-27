package sdk

import (
	"context"
	"testing"
)

type staticInputSource struct{ inputs []Input }

func (s staticInputSource) Receive(context.Context) (<-chan Input, error) {
	out := make(chan Input, len(s.inputs))
	for _, input := range s.inputs {
		out <- input
	}
	close(out)
	return out, nil
}

func TestMergeInputSourcesCombinesStreams(t *testing.T) {
	ctx := context.Background()
	inputs, err := MergeInputSources(ctx,
		staticInputSource{inputs: []Input{{Source: "discord", SessionID: "discord:1"}}},
		staticInputSource{inputs: []Input{{Source: "cli", SessionID: "cli:default"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for input := range inputs {
		seen[input.Source] = true
	}
	if !seen["discord"] || !seen["cli"] || len(seen) != 2 {
		t.Fatalf("merged sources = %#v", seen)
	}
}
