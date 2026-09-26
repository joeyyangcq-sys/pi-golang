package usecase

import (
	"encoding/json"
	"strings"
	"testing"

	"pi-golang/internal/entity"
)

func TestFitMaxTokensToContextIncludesToolSchemas(t *testing.T) {
	cfg := entity.ContextCompactionConfig{ContextWindowTokens: 1000}
	base := entity.ChatRequest{
		Messages:  entity.Conversation{entity.User(strings.Repeat("x", 400))},
		MaxTokens: 900,
	}
	withoutTools := fitMaxTokensToContext(base, cfg)
	base.Tools = []entity.Info{{
		Name: "large_tool", Description: strings.Repeat("description ", 80),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
	}}
	withTools := fitMaxTokensToContext(base, cfg)
	if withTools >= withoutTools {
		t.Fatalf("tool schemas must reduce completion budget: without=%d with=%d", withoutTools, withTools)
	}
	if withTools < 1 {
		t.Fatalf("completion budget must remain valid: %d", withTools)
	}
}
