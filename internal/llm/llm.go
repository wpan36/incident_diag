// Package llm is an OpenAI-compatible chat completions client with native
// tool calling.
//
// It is deliberately thin. The provider enforces the tool schemas, so there is
// no tolerant parser here for code-block-wrapped JSON and no structured-output
// envelope of our own; a response either carries tool calls or it does not,
// and deciding what that means is the agent loop's job rather than this
// package's.
//
// There is no streaming. The final diagnosis arrives as the arguments of a
// finish tool call rather than as assistant content, so streaming it to a
// browser means streaming tool-call argument deltas — a different mechanism,
// and one nothing before M27 consumes.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/obs"
)

// tracer is resolved through the global provider on every span, so holding it
// here does not depend on obs.Setup having run first.
var tracer = obs.Tracer("llm")

// Message roles, as the wire format spells them.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Tool choices this package sends. Anything else a provider accepts is not
// used, so the set stays small enough to reason about when switching
// providers.
const (
	// ChoiceRequired demands that the model call one of the offered tools. The
	// agent sends it on the forced finish, where a prose answer would end the
	// run with no diagnosis at all.
	ChoiceRequired = "required"
)

const (
	baseRetryDelay = 500 * time.Millisecond
	maxRetryDelay  = 10 * time.Second
)

// ErrBadResponse marks a reply this client cannot read as a chat completion —
// no choices, most likely. It is never retried: the provider is not speaking
// the protocol.
var ErrBadResponse = errors.New("llm: the provider's response is not a chat completion")

// APIError is a non-2xx response from the provider.
type APIError struct {
	StatusCode int
	Body       string

	// retryAfter is what the provider asked for on a 429.
	retryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: the chat API returned %d: %s", e.StatusCode, e.Body)
}

// FunctionCall is the name and arguments of one tool call. Arguments is a
// string holding JSON, which is how the OpenAI-compatible format carries it;
// it is passed through unchanged so that a provider's formatting choices
// cannot change what the audit trail records.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is one tool the model asked to call.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// Message is one entry in the conversation.
//
// It is the wire shape rather than a friendlier one because the context is a
// real message sequence: an assistant message carrying tool calls has to be
// replayed with the matching tool replies, and some providers reject a history
// where they are missing.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool is one tool definition offered to the model.
//
// InputSchema is raw JSON Schema, taken from MCP unchanged. Re-modelling it in
// Go only to marshal it back would be a second place for the two to disagree —
// the same reasoning mcpclient.Tool follows.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Request is one chat completion.
type Request struct {
	Messages []Message
	Tools    []Tool

	// ToolChoice is sent only when set. The agent leaves it empty on an
	// ordinary step and sends ChoiceRequired on the forced finish.
	ToolChoice string
}

// Usage is what the provider billed. A provider that reports none leaves both
// at zero, which the agent records rather than estimating over.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Response is one reply.
//
// Content is the model's prose. The agent never persists it and never replays
// it into the context: the schema has nowhere to put chain-of-thought, which
// is deliberate.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Chatter is what the agent loop depends on.
//
// The interface exists because the loop's tests need a scriptable fake, not
// because a second implementation is expected: switching providers is three
// environment variables, not a second type.
type Chatter interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

// Client is a Chatter backed by an OpenAI-compatible /v1/chat/completions
// endpoint.
type Client struct {
	cfg    config.LLM
	http   *http.Client
	logger *slog.Logger
}

// New builds a client. The HTTP client has no timeout of its own: LLM_TIMEOUT
// is applied per attempt through the context, so a retry gets a fresh one.
func New(cfg config.LLM, logger *slog.Logger) *Client {
	return &Client{cfg: cfg, http: &http.Client{Transport: obs.Transport(nil)}, logger: logger}
}

// Chat sends one completion, retrying transient failures until the budget is
// spent.
//
// The span covers every attempt; otelhttp adds one child per attempt, so a
// slow call shows whether it was one slow request or three.
func (c *Client) Chat(ctx context.Context, req Request) (Response, error) {
	ctx, span := tracer.Start(ctx, "llm.chat",
		trace.WithAttributes(obs.AttrModel.String(c.cfg.Model)))
	defer span.End()

	started := time.Now()
	resp, err := c.chat(ctx, req)
	obs.LLMRequestDuration.WithLabelValues(obs.OutcomeOf(err)).Observe(time.Since(started).Seconds())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "the chat completion failed")
		return Response{}, err
	}
	obs.LLMTokens.WithLabelValues(obs.TokensPrompt).Add(float64(resp.Usage.PromptTokens))
	obs.LLMTokens.WithLabelValues(obs.TokensCompletion).Add(float64(resp.Usage.CompletionTokens))
	span.SetAttributes(
		attribute.Int("incident_diag.prompt_tokens", resp.Usage.PromptTokens),
		attribute.Int("incident_diag.completion_tokens", resp.Usage.CompletionTokens),
		attribute.String("incident_diag.finish_reason", resp.FinishReason),
	)
	return resp, nil
}

func (c *Client) chat(ctx context.Context, req Request) (Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, retryDelay(attempt, lastErr)); err != nil {
				return Response{}, errors.Join(lastErr, err)
			}
		}

		resp, err := c.request(ctx, req)
		switch {
		case err == nil:
			return resp, nil
		case !retryable(err):
			return Response{}, err
		case ctx.Err() != nil:
			// A cancelled or expired run is not the provider's fault and will
			// not improve with another attempt.
			return Response{}, err
		}

		lastErr = err
		if attempt < c.cfg.MaxRetries {
			c.logger.WarnContext(ctx, "chat request failed, retrying",
				"attempt", attempt+1, "max_attempts", c.cfg.MaxRetries+1, "error", err)
		}
	}
	return Response{}, fmt.Errorf("after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type toolDef struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []toolDef `json:"tools,omitempty"`
	// ToolChoice is a string here because the two forms this project uses —
	// absent and "required" — need nothing richer.
	ToolChoice string `json:"tool_choice,omitempty"`
	// ParallelToolCalls is a pointer so that it is sent only alongside tools.
	// The agent's schema records one action per step, so several calls in one
	// response can never all be executed.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// request performs one attempt.
func (c *Client) request(ctx context.Context, req Request) (Response, error) {
	wire := chatRequest{Model: c.cfg.Model, Messages: req.Messages, ToolChoice: req.ToolChoice}
	if len(req.Tools) > 0 {
		wire.Tools = make([]toolDef, 0, len(req.Tools))
		for _, t := range req.Tools {
			wire.Tools = append(wire.Tools, toolDef{
				Type:     "function",
				Function: functionDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema},
			})
		}
		parallel := false
		wire.ParallelToolCalls = &parallel
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return Response{}, fmt.Errorf("llm: encoding the request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("llm: building the request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: calling the chat API: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode/100 != 2 {
		// Bounded, because a provider answering an error with a page of HTML
		// would otherwise put all of it in a run's error column.
		detail, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return Response{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Body:       strings.TrimSpace(string(detail)),
			retryAfter: retryAfter(httpResp.Header),
		}
	}

	var decoded chatResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("llm: decoding the response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: it carries no choices", ErrBadResponse)
	}

	choice := decoded.Choices[0]
	out := Response{
		Content:      choice.Message.Content,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
	}
	if decoded.Usage == nil {
		// Recorded as a warning rather than estimated: a counter that is
		// obviously zero is easier to explain than one holding a number
		// nobody can reconcile with the provider's bill.
		c.logger.WarnContext(ctx, "the chat response reported no token usage", "model", c.cfg.Model)
	} else {
		out.Usage = Usage{
			PromptTokens:     decoded.Usage.PromptTokens,
			CompletionTokens: decoded.Usage.CompletionTokens,
		}
	}
	return out, nil
}

// retryable decides whether another attempt could plausibly succeed.
//
// The 400 family is the request being wrong — a schema the provider rejects, a
// key it does not know, a context longer than the model's window — and will be
// wrong the same way next time. This is the opposite of internal/embed's
// choice to retry 401 and 403, and for a reason: an ingestion backlog should
// heal itself once an operator fixes the key, whereas an agent run is one
// interactive request whose user is waiting.
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusTooManyRequests {
			return true
		}
		return apiErr.StatusCode/100 != 4
	}
	if errors.Is(err, ErrBadResponse) {
		return false
	}
	// What is left is a transport failure, a timeout, or a body that could not
	// be decoded. All three are worth one more attempt.
	return true
}

// retryDelay is exponential backoff, unless the provider said how long to
// wait. No jitter, for the reason internal/embed gives: one client with one
// in-flight request is not a thundering herd, and a deterministic schedule is
// one a test can assert.
func retryDelay(attempt int, lastErr error) time.Duration {
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.retryAfter > 0 {
		return apiErr.retryAfter
	}
	return min(baseRetryDelay<<(attempt-1), maxRetryDelay)
}

// retryAfter reads the Retry-After header, honouring only the delta-seconds
// form and capping it so a provider asking for an hour cannot park a run.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	seconds, err := strconv.Atoi(v)
	if err != nil || seconds < 0 {
		return 0
	}
	return min(time.Duration(seconds)*time.Second, maxRetryDelay)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
