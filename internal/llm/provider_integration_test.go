//go:build integration

// The claim that this runtime is not wired to one vendor (ADR 0006), checked
// against a second OpenAI-compatible provider.
//
//	TEST_ALT_LLM_BASE_URL=... TEST_ALT_LLM_MODEL=... TEST_ALT_LLM_API_KEY=... make test-integration
//
// It makes a real completion and costs one, so it skips unless
// TEST_ALT_LLM_BASE_URL is set on purpose.
//
// What it checks is a **tool call**, not that the endpoint answers. The part
// most likely to differ between providers is native tool calling, which is
// exactly what the agent loop depends on and what a "does it reply" check would
// miss.
package llm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
)

func TestSecondProviderMakesAToolCall(t *testing.T) {
	baseURL := os.Getenv("TEST_ALT_LLM_BASE_URL")
	if baseURL == "" {
		t.Skip("TEST_ALT_LLM_BASE_URL is not set; skipping the provider-switch test")
	}
	model := os.Getenv("TEST_ALT_LLM_MODEL")
	if model == "" {
		t.Fatal("TEST_ALT_LLM_BASE_URL is set but TEST_ALT_LLM_MODEL is not")
	}
	key := os.Getenv("TEST_ALT_LLM_API_KEY")
	if key == "" {
		// SiliconFlow hosts both this project's embeddings and chat models, so
		// the second provider is usually the embedding key that is already
		// configured rather than a new account.
		key = os.Getenv("EMBEDDING_API_KEY")
	}
	if key == "" {
		t.Fatal("neither TEST_ALT_LLM_API_KEY nor EMBEDDING_API_KEY is set")
	}

	client := New(config.LLM{
		BaseURL: baseURL, APIKey: key, Model: model,
		Timeout: 60 * time.Second, MaxRetries: 2,
	}, log.Discard())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tool := Tool{
		Name:        "prometheus_query",
		Description: "Run an instant PromQL query against Prometheus.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The PromQL expression."}
  },
  "required": ["query"],
  "additionalProperties": false
}`),
	}

	resp, err := client.Chat(ctx, Request{
		Tools: []Tool{tool},
		Messages: []Message{
			{Role: RoleSystem, Content: "You are an SRE. You investigate only by calling tools. " +
				"Never answer in prose; call a tool."},
			{Role: RoleUser, Content: "payment-service looks slow. Check its p99 request latency " +
				"with the prometheus_query tool."},
		},
	})
	if err != nil {
		t.Fatalf("Chat against %s (%s): %v", baseURL, model, err)
	}

	t.Logf("provider=%s model=%s finish_reason=%s tokens=%d/%d",
		baseURL, model, resp.FinishReason, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	if len(resp.ToolCalls) == 0 {
		t.Fatalf("%s returned no tool call; content was %q", model, resp.Content)
	}
	call := resp.ToolCalls[0]
	if call.Function.Name != "prometheus_query" {
		t.Errorf("called %q, want prometheus_query", call.Function.Name)
	}

	// The arguments have to be usable as they arrive. S7 chose native tool
	// calling so that no tolerant parser exists anywhere in this project, and a
	// provider that wraps its arguments in a code fence would break that.
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("the arguments are not valid JSON: %v\n%s", err, call.Function.Arguments)
	}
	if strings.TrimSpace(args.Query) == "" {
		t.Errorf("the tool was called with an empty query: %s", call.Function.Arguments)
	}
	t.Logf("tool call: %s(%s)", call.Function.Name, call.Function.Arguments)
}
