package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
)

// clientFor points a client at a test server with a retry budget small enough
// that a test never waits on the backoff schedule for long.
func clientFor(t *testing.T, h http.HandlerFunc, retries int) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(config.LLM{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "test-model",
		Timeout:    2 * time.Second,
		MaxRetries: retries,
	}, log.Discard())
}

const toolCallBody = `{
  "choices": [{
    "message": {
      "content": "let me look",
      "tool_calls": [{"id": "call_1", "type": "function",
                      "function": {"name": "prometheus_query", "arguments": "{\"query\":\"up\"}"}}]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 120, "completion_tokens": 30}
}`

func TestChatSendsToolsAndReadsTheCall(t *testing.T) {
	var body map[string]any
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		io.WriteString(w, toolCallBody)
	}, 0)

	resp, err := c.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "what is wrong"}},
		Tools: []Tool{{
			Name: "prometheus_query", Description: "run a query",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "prometheus_query" {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	if resp.Usage != (Usage{PromptTokens: 120, CompletionTokens: 30}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}

	// parallel_tool_calls is sent alongside the tools, because the agent's
	// schema records one action per step.
	if body["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", body["parallel_tool_calls"])
	}
	if _, present := body["tool_choice"]; present {
		t.Errorf("tool_choice was sent when the request did not set one: %v", body["tool_choice"])
	}
	// The MCP schema goes out unchanged, nested under the function.
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "prometheus_query" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("tool definition = %v", fn)
	}
}

func TestChatSendsToolChoiceWhenSet(t *testing.T) {
	var body map[string]any
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		io.WriteString(w, toolCallBody)
	}, 0)

	if _, err := c.Chat(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "finish"}},
		Tools:      []Tool{{Name: "finish", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: ChoiceRequired,
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if body["tool_choice"] != ChoiceRequired {
		t.Errorf("tool_choice = %v, want %q", body["tool_choice"], ChoiceRequired)
	}
}

// A message sequence is the whole point of this client's Message shape: an
// assistant message carrying a tool call has to go back out with its reply.
func TestChatRoundTripsAToolReply(t *testing.T) {
	var body struct {
		Messages []Message `json:"messages"`
	}
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, toolCallBody)
	}, 0)

	sent := []Message{
		{Role: RoleSystem, Content: "you are an SRE"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{NewToolCall("call_1", "finish", map[string]string{"a": "b"})}},
		{Role: RoleTool, ToolCallID: "call_1", Content: "an observation"},
	}
	if _, err := c.Chat(context.Background(), Request{Messages: sent}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(body.Messages))
	}
	if body.Messages[1].ToolCalls[0].ID != "call_1" || body.Messages[2].ToolCallID != "call_1" {
		t.Errorf("the tool call and its reply did not pair: %+v", body.Messages)
	}
}

func TestChatRetriesServerErrorsAndSucceeds(t *testing.T) {
	var attempts atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, toolCallBody)
	}, 2)

	if _, err := c.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// The 400 family is the request being wrong, and it will be wrong the same way
// next time. This is deliberately the opposite of internal/embed's choice to
// retry 401: an ingestion backlog should heal itself, an agent run should not
// spend its budget on a request that cannot succeed.
func TestChatDoesNotRetryTheBadRequestFamily(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		var attempts atomic.Int32
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(status)
			io.WriteString(w, "no")
		}, 3)

		_, err := c.Chat(context.Background(), Request{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
			t.Errorf("status %d: err = %v", status, err)
		}
		if got := attempts.Load(); got != 1 {
			t.Errorf("status %d: attempts = %d, want 1", status, got)
		}
	}
}

func TestChatRetriesRateLimitsAndHonoursRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, toolCallBody)
	}, 2)

	if _, err := c.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// A provider that reports no usage leaves the counters at zero rather than
// having them estimated: a number nobody can reconcile with the bill is worse
// than an obvious zero.
func TestChatToleratesAMissingUsage(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`)
	}, 0)

	resp, err := c.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage != (Usage{}) {
		t.Errorf("Usage = %+v, want zero", resp.Usage)
	}
	if resp.Content != "hello" {
		t.Errorf("Content = %q", resp.Content)
	}
}

func TestChatRejectsAResponseWithNoChoices(t *testing.T) {
	var attempts atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		io.WriteString(w, `{"choices":[]}`)
	}, 3)

	_, err := c.Chat(context.Background(), Request{})
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v, want ErrBadResponse", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a provider not speaking the protocol will not start", got)
	}
}

// The timeout is applied per attempt through the context, so a retry gets a
// fresh one — the HTTP client itself has none.
func TestChatTimesOutOneAttempt(t *testing.T) {
	// The handler blocks until the test is over. Waiting on the request's own
	// context is not enough: the client abandoning the request does not always
	// reach the server, and Close then waits for a handler that never returns.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})
	c := New(config.LLM{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Timeout: 50 * time.Millisecond, MaxRetries: 0}, log.Discard())

	_, err := c.Chat(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "calling the chat API") {
		t.Fatalf("err = %v, want a call failure", err)
	}
}

func TestChatStopsOnACancelledContext(t *testing.T) {
	var attempts atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Chat(ctx, Request{}); err == nil {
		t.Fatal("Chat on a cancelled context succeeded")
	}
	if got := attempts.Load(); got > 1 {
		t.Errorf("attempts = %d: a cancelled run must not keep retrying", got)
	}
}

func TestFakeFollowsItsScriptAndThenFails(t *testing.T) {
	f := NewFake(
		CallTurn("c1", "search_knowledge", map[string]string{"query": "latency"}),
		TextTurn("I think it is the pool"),
	)

	first, err := f.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser}}})
	if err != nil || first.ToolCalls[0].Function.Name != "search_knowledge" {
		t.Fatalf("first turn: %+v (%v)", first, err)
	}
	second, err := f.Chat(context.Background(), Request{})
	if err != nil || second.Content == "" || len(second.ToolCalls) != 0 {
		t.Fatalf("second turn: %+v (%v)", second, err)
	}
	if _, err := f.Chat(context.Background(), Request{}); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("third turn: err = %v, want ErrScriptExhausted", err)
	}
	if f.Calls() != 3 || len(f.Requests()) != 3 {
		t.Errorf("Calls = %d, Requests = %d", f.Calls(), len(f.Requests()))
	}
}

func TestNewToolCallTakesAStringAsRawJSON(t *testing.T) {
	// A string is passed through unchanged, which is how a test produces
	// arguments the loop cannot decode.
	call := NewToolCall("c1", "finish", "not json")
	if call.Function.Arguments != "not json" {
		t.Errorf("Arguments = %q", call.Function.Arguments)
	}
	if NewToolCall("c2", "finish", nil).Function.Arguments != "{}" {
		t.Error("nil arguments should encode as an empty object")
	}
}
