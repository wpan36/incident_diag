package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrScriptExhausted is returned once a Fake has handed out every scripted
// turn.
//
// An exhausted script is an error rather than a repeat of the last response
// because the agent loop's tests are about how many calls it makes: a bound
// test that silently got one extra response would still pass while testing
// nothing.
var ErrScriptExhausted = errors.New("llm: the fake's script is exhausted")

// Turn is one scripted reply.
type Turn struct {
	Response Response

	// Err, when set, is returned instead of Response.
	Err error
}

// Fake is a scriptable Chatter.
//
// It ships in the package rather than in a _test.go file for the reason
// embed.Fake does: the code that most needs testing without a provider is the
// agent loop, which lives somewhere else.
type Fake struct {
	mu    sync.Mutex
	turns []Turn
	calls int
	reqs  []Request

	// Delay is slept before every reply, on a context the caller can cancel.
	// It is how a test makes a run exceed its wall-clock bound, or cancels a
	// call that is in flight.
	Delay time.Duration
}

// NewFake builds a Fake that returns these turns in order.
func NewFake(turns ...Turn) *Fake { return &Fake{turns: turns} }

// Chat implements Chatter.
func (f *Fake) Chat(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	delay := f.Delay
	index := f.calls
	f.calls++
	f.reqs = append(f.reqs, req)
	var turn Turn
	if index < len(f.turns) {
		turn = f.turns[index]
	}
	exhausted := index >= len(f.turns)
	f.mu.Unlock()

	if delay > 0 {
		if err := sleep(ctx, delay); err != nil {
			return Response{}, err
		}
	}
	// Checked after the delay so that a cancelled call reports cancellation
	// rather than the script running out.
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if exhausted {
		return Response{}, fmt.Errorf("%w after %d calls", ErrScriptExhausted, index)
	}
	if turn.Err != nil {
		return Response{}, turn.Err
	}
	return turn.Response, nil
}

// Calls returns how many times Chat was called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Requests returns every request Chat was given, in order. It is how a test
// asserts what the ContextBuilder actually sent.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// CallTurn is a turn in which the model calls one tool.
func CallTurn(id, name string, arguments any) Turn {
	return Turn{Response: Response{
		ToolCalls:    []ToolCall{NewToolCall(id, name, arguments)},
		FinishReason: "tool_calls",
		Usage:        Usage{PromptTokens: 100, CompletionTokens: 20},
	}}
}

// CallsTurn is a turn carrying several tool calls, which a provider that
// ignores parallel_tool_calls can produce and the loop has to handle.
func CallsTurn(calls ...ToolCall) Turn {
	return Turn{Response: Response{
		ToolCalls:    calls,
		FinishReason: "tool_calls",
		Usage:        Usage{PromptTokens: 100, CompletionTokens: 20},
	}}
}

// TextTurn is a turn in which the model answers in prose and calls nothing.
func TextTurn(content string) Turn {
	return Turn{Response: Response{
		Content:      content,
		FinishReason: "stop",
		Usage:        Usage{PromptTokens: 100, CompletionTokens: 20},
	}}
}

// NewToolCall builds a tool call, encoding arguments as the wire format's
// JSON string. A string argument is taken as already-encoded JSON, which is
// how a test produces arguments that will not decode.
func NewToolCall(id, name string, arguments any) ToolCall {
	var encoded string
	switch v := arguments.(type) {
	case nil:
		encoded = "{}"
	case string:
		encoded = v
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			panic(fmt.Sprintf("llm: fake tool call arguments will not marshal: %v", err))
		}
		encoded = string(raw)
	}
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: encoded}}
}
