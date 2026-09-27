package main

import (
	"context"
	"fmt"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/anthropic"
	"github.com/Tulipskun/ai-engine/sdk/providers/gemini"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
)

func main() {
	_ = context.Background()
	_ = sdk.NewClient(openai.New(""))
	_ = sdk.NewClient(anthropic.New(""))
	_ = sdk.NewClient(gemini.New(""))
	fmt.Println("canonical AI SDK prototype: openai, anthropic, gemini")
}
