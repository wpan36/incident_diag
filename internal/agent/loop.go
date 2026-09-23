package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/obs"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

// tracer is resolved through the global provider on every span, so holding it
// here does not depend on obs.Setup having run first.
var tracer = obs.Tracer("agent")

// emptyArguments is what a tool call with no arguments records, so that
// agent_steps.action and tool_calls.arguments are always valid JSON.
var emptyArguments = json.RawMessage(`{}`)

// record is what the loop remembers about a completed step, so that finish's
// citations can be resolved without reading anything back.
type record struct {
	ref         StepRef
	actionType  string
	toolName    string
	observation string

	// hits and query belong to a retrieval. Keeping the hits is what lets a
	// cited document_id be validated, and what fills an evidence row with the
	// one hit the model cited rather than with all five.
	hits  []search.Result
	query string
}

// runState is one run in progress.
type runState struct {
	run     Run
	builder ContextBuilder
	start   time.Time

	turns   []Turn
	records map[int]record

	// stepNumber is the last step started, so the next one is stepNumber + 1.
	// step_number starts at 1, and every persisted step occupies one —
	// including a step that called no tool.
	stepNumber int

	// stepCount counts steps the callback actually recorded, which is what
	// agent_runs.step_count means.
	stepCount int

	// toolCalls counts tool_calls rows. A retrieval writes none, and a
	// refusal writes one: it consumed a call, and pretending otherwise would
	// let an agent that keeps guessing wrong run unbounded.
	toolCalls int

	promptTokens     int
	completionTokens int

	// noToolStreak is how many responses in a row called no usable tool. The
	// loop retries once; two in a row ends the run.
	noToolStreak int
}

func (s *runState) outcome(status, stopReason, errText string, final json.RawMessage) store.RunOutcome {
	return store.RunOutcome{
		Status:           status,
		StopReason:       stopReason,
		FinalResult:      final,
		Error:            errText,
		StepCount:        s.stepCount,
		ToolCallCount:    s.toolCalls,
		PromptTokens:     s.promptTokens,
		CompletionTokens: s.completionTokens,
	}
}

func (s *runState) failed(reason string) store.RunOutcome {
	return s.outcome(store.RunFailed, store.StopError, reason, nil)
}

// bound reports which bound, if any, says to stop starting new work.
//
// All three are checked before the model chooses, so a run that has exhausted
// its tool calls is sent to finish even when its next action would have been a
// retrieval, which costs no tool call. Making a bound depend on what the model
// picks next would turn it into a gate and make it much harder to test.
// spent describes the step about to be taken, for the budget line.
//
// stepNumber is the last completed step, so the one being decided is the next.
// bound() has already refused to start a step past MaxSteps, so this is never
// more than MaxSteps.
func (s *runState) spent() Spent {
	return Spent{Step: s.stepNumber + 1, ToolCalls: s.toolCalls}
}

func (s *runState) bound() (string, bool) {
	switch {
	case s.stepNumber >= s.run.Budget.MaxSteps:
		return store.StopMaxSteps, true
	case s.toolCalls >= s.run.Budget.MaxToolCalls:
		return store.StopMaxToolCalls, true
	case time.Since(s.start) >= s.run.Budget.MaxRunDuration:
		return store.StopTimeout, true
	}
	return "", false
}

// Run investigates one incident and returns how it ended.
//
// The error return is cancellation and nothing else. A cancelled run writes
// nothing terminal: it stays RUNNING, its Kafka offset uncommitted, and the
// lease reclaims and restarts it (ADR 0009). Writing a terminal state here
// would be a claim that the investigation is over, which is what the restart
// machinery exists to contradict — so stop_reason = CANCELLED has no writer.
//
// Every other ending, including a failure, comes back as an outcome the
// caller writes with FinishRun.
func (a *Agent) Run(ctx context.Context, run Run) (outcome store.RunOutcome, err error) {
	// The root of one investigation's trace. Its parent is the Kafka process
	// span, which kotel joined to the API request that enqueued the run, so
	// "one run, one trace" holds across all four processes.
	started := time.Now()
	ctx, span := tracer.Start(ctx, "agent.run", trace.WithAttributes(
		obs.AttrRunID.String(run.ID),
		obs.AttrIncidentID.String(run.Incident.ID),
	))
	// Named returns so that every exit records how the run ended, including
	// the cancellation that writes nothing terminal.
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "the run was cancelled")
		} else {
			span.SetAttributes(
				obs.AttrStopReason.String(outcome.StopReason),
				attribute.Int("incident_diag.steps", outcome.StepCount),
				attribute.Int("incident_diag.tool_calls", outcome.ToolCallCount),
			)
			// Only a terminal run is counted. A cancelled one returns an error
			// above and is restarted, so counting it would count it twice.
			obs.AgentRuns.WithLabelValues(outcome.Status, outcome.StopReason).Inc()
			obs.AgentRunDuration.Observe(time.Since(started).Seconds())
		}
		span.End()
	}()

	s := &runState{
		run:     run,
		builder: ContextBuilder{Incident: run.Incident, Budget: run.Budget},
		start:   started,
		records: map[int]record{},
	}

	for {
		if err := ctx.Err(); err != nil {
			return store.RunOutcome{}, err
		}

		if reason, hit := s.bound(); hit {
			return a.forceFinish(ctx, s, reason)
		}

		messages := s.builder.Build(s.turns, s.spent())
		if EstimateTokens(messages) > run.Budget.MaxPromptTokens {
			// A safety net rather than an operating bound: config.LoadAgent's
			// invariant means a valid configuration cannot reach it.
			return a.forceFinish(ctx, s, store.StopTokenBudget)
		}

		outcome, done, err := a.step(ctx, s, messages)
		if err != nil {
			return store.RunOutcome{}, err
		}
		if done {
			return outcome, nil
		}
	}
}

// step runs one iteration. done says the run ended; a non-nil error is
// cancellation.
func (a *Agent) step(ctx context.Context, s *runState, messages []llm.Message) (store.RunOutcome, bool, error) {
	started := time.Now()
	s.stepNumber++

	ctx, span := tracer.Start(ctx, "agent.step",
		trace.WithAttributes(obs.AttrStepNumber.Int(s.stepNumber)))
	defer span.End()

	resp, err := a.deps.LLM.Chat(ctx, llm.Request{Messages: messages, Tools: a.definitions})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return store.RunOutcome{}, false, ctxErr
		}
		// Unlike a failing tool, an unreachable model is not something the
		// run can investigate around: there is nothing to decide the next
		// step with.
		return s.failed(fmt.Sprintf("the model could not be called: %v", err)), true, nil
	}
	s.promptTokens += resp.Usage.PromptTokens
	s.completionTokens += resp.Usage.CompletionTokens

	call, name, args, reason := a.decide(s, resp)
	span.SetAttributes(obs.AttrToolName.String(name))
	if reason != "" {
		return a.noToolStep(ctx, s, started, reason)
	}

	switch name {
	case ToolFinish:
		return a.finishStep(ctx, s, started, args, store.StopCompleted)

	case ToolSearchKnowledge:
		obs := a.deps.Knowledge.retrieve(ctx, args)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return store.RunOutcome{}, false, ctxErr
		}
		return a.observedStep(ctx, s, started, call, args, store.ActionRetrieve, obs)

	default:
		obs := a.callTool(ctx, name, args)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return store.RunOutcome{}, false, ctxErr
		}
		s.toolCalls++
		return a.observedStep(ctx, s, started, call, args, store.ActionToolCall, obs)
	}
}

// decide picks the one call a response is allowed to make.
//
// A non-empty reason means the response is unusable and the step is a none.
func (a *Agent) decide(s *runState, resp llm.Response) (llm.ToolCall, string, json.RawMessage, string) {
	if len(resp.ToolCalls) == 0 {
		return llm.ToolCall{}, "", nil, "the response called no tool"
	}
	if len(resp.ToolCalls) > 1 {
		// The first is executed and the rest are dropped: agent_steps records
		// one action per step, and silently executing the others would leave a
		// tool call in the audit trail with no step to hang it on. Only the
		// executed one is replayed into the context, because an assistant
		// message whose other calls have no paired reply is exactly the
		// history some providers reject.
		a.deps.Logger.Warn("the response carried several tool calls; executing the first and dropping the rest",
			"run_id", s.run.ID, "step", s.stepNumber, "calls", len(resp.ToolCalls))
	}

	call := resp.ToolCalls[0]
	call.Function.Name = strings.TrimSpace(call.Function.Name)
	if call.Function.Name == "" {
		return llm.ToolCall{}, "", nil, "the response called a tool with no name"
	}
	if call.ID == "" {
		// Some providers omit it. The id only has to pair the assistant
		// message with its reply inside one context, so one of ours does.
		call.ID = fmt.Sprintf("call_%d", s.stepNumber)
	}
	// Type is part of the assistant message replayed into the context, and a
	// provider that omits it on the way out may still require it on the way
	// back in.
	if call.Type == "" {
		call.Type = "function"
	}

	args := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
	if len(args) == 0 {
		args = emptyArguments
	} else if !json.Valid(args) {
		// Refused here rather than passed on: these bytes go into two JSON
		// columns, and no tool could act on them anyway.
		return llm.ToolCall{}, "", nil, fmt.Sprintf(
			"the arguments to %s are not valid JSON", call.Function.Name)
	}
	call.Function.Arguments = string(args)
	return call, call.Function.Name, args, ""
}

// observedStep records a retrieval or a tool call.
func (a *Agent) observedStep(ctx context.Context, s *runState, started time.Time,
	call llm.ToolCall, args json.RawMessage, actionType string, obs Observation,
) (store.RunOutcome, bool, error) {
	step := Step{
		RunID:       s.run.ID,
		StepNumber:  s.stepNumber,
		ActionType:  actionType,
		Action:      actionJSON(call.Function.Name, args),
		Observation: obs.Text,
		// OK in every case, including a refusal and a failing dependency: a
		// step's status says whether it produced a usable action and an
		// observation, not whether the observation was good news.
		Status:   store.StepOK,
		Error:    obs.Error,
		Duration: time.Since(started),
		ToolCall: obs.ToolCall,
	}
	rec := record{
		actionType:  actionType,
		toolName:    call.Function.Name,
		observation: obs.Text,
		hits:        obs.Hits,
		query:       obs.Query,
	}
	if outcome, failed, err := a.report(ctx, s, step, rec); failed {
		return outcome, true, err
	}

	s.turns = append(s.turns, Turn{Number: s.stepNumber, Call: call, Observation: obs.Text})
	s.noToolStreak = 0
	return store.RunOutcome{}, false, nil
}

// finishStep ends the run, or records a none when the arguments are unusable.
//
// stopReason is COMPLETED on an ordinary finish and the bound's reason on a
// forced one, because a bounded run that produced a diagnosis is a success
// that says why it stopped.
func (a *Agent) finishStep(ctx context.Context, s *runState, started time.Time,
	args json.RawMessage, stopReason string,
) (store.RunOutcome, bool, error) {
	final, err := a.decodeFinal(s.run.ID, args)
	if err != nil {
		return a.noToolStep(ctx, s, started,
			fmt.Sprintf("the arguments to %s could not be read: %v", ToolFinish, err))
	}
	if strings.TrimSpace(final.RootCause) == "" {
		return a.noToolStep(ctx, s, started,
			fmt.Sprintf("%s was called with an empty root_cause", ToolFinish))
	}

	step := Step{
		RunID:      s.run.ID,
		StepNumber: s.stepNumber,
		ActionType: store.ActionFinish,
		Action:     actionJSON(ToolFinish, args),
		Status:     store.StepOK,
		Duration:   time.Since(started),
		Evidence:   a.resolveEvidence(s, final.Evidence),
	}
	if outcome, failed, err := a.report(ctx, s, step, record{actionType: store.ActionFinish}); failed {
		return outcome, true, err
	}
	// The arguments as they were given, which is what agent_runs.final_result
	// holds.
	return s.outcome(store.RunSucceeded, stopReason, "", args), true, nil
}

// noToolStep records a response that produced no usable action.
//
// The step is persisted rather than dropped because the three agent tables
// exist to make a run auditable, and "the model stopped calling tools" is the
// single most useful thing an evaluation can count.
func (a *Agent) noToolStep(ctx context.Context, s *runState, started time.Time, reason string,
) (store.RunOutcome, bool, error) {
	step := Step{
		RunID:      s.run.ID,
		StepNumber: s.stepNumber,
		ActionType: store.ActionNone,
		Action:     emptyArguments,
		Status:     store.StepError,
		Error:      reason,
		Duration:   time.Since(started),
	}
	if outcome, failed, err := a.report(ctx, s, step, record{actionType: store.ActionNone}); failed {
		return outcome, true, err
	}

	s.turns = append(s.turns, Turn{Number: s.stepNumber, None: true, Reason: reason})
	s.noToolStreak++
	if s.noToolStreak > 1 {
		return s.failed(fmt.Sprintf("the model called no usable tool twice in a row: %s", reason)), true, nil
	}
	// The instruction to use a tool is the user message the builder renders
	// for a none step, so the retry is an ordinary next iteration rather than
	// a second code path.
	a.deps.Logger.Warn("the model called no usable tool; retrying once",
		"run_id", s.run.ID, "step", s.stepNumber, "reason", reason)
	return store.RunOutcome{}, false, nil
}

// forceFinish is the one extra call a bound buys.
//
// A bound means "stop starting new work", not "never exceed": the run ends
// with a real partial diagnosis rather than a mechanical summary of its own
// steps, and the forced call occupies a step number so the timeline shows why
// the run ended.
func (a *Agent) forceFinish(ctx context.Context, s *runState, stopReason string) (store.RunOutcome, error) {
	if err := ctx.Err(); err != nil {
		return store.RunOutcome{}, err
	}

	// The budget line is built too, and then the forced message follows it and
	// contradicts nothing: by here the run is over, and forcedFinishMessage
	// says which bound ended it.
	messages := append(s.builder.Build(s.turns, s.spent()),
		llm.Message{Role: llm.RoleUser, Content: forcedFinishMessage(stopReason)})

	started := time.Now()
	s.stepNumber++

	resp, err := a.deps.LLM.Chat(ctx, llm.Request{
		Messages:   messages,
		Tools:      []llm.Tool{toolFor(finishTool)},
		ToolChoice: llm.ChoiceRequired,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return store.RunOutcome{}, ctxErr
		}
		return a.forcedFinishFailed(ctx, s, started, stopReason,
			fmt.Sprintf("the model could not be called: %v", err))
	}
	s.promptTokens += resp.Usage.PromptTokens
	s.completionTokens += resp.Usage.CompletionTokens

	_, name, args, reason := a.decide(s, resp)
	switch {
	case reason != "":
		return a.forcedFinishFailed(ctx, s, started, stopReason, reason)
	case name != ToolFinish:
		return a.forcedFinishFailed(ctx, s, started, stopReason,
			fmt.Sprintf("the model called %s when only %s was offered", name, ToolFinish))
	}

	final, decodeErr := a.decodeFinal(s.run.ID, args)
	switch {
	case decodeErr != nil:
		return a.forcedFinishFailed(ctx, s, started, stopReason,
			fmt.Sprintf("the arguments to %s could not be read: %v", ToolFinish, decodeErr))
	case strings.TrimSpace(final.RootCause) == "":
		return a.forcedFinishFailed(ctx, s, started, stopReason,
			fmt.Sprintf("%s was called with an empty root_cause", ToolFinish))
	}

	step := Step{
		RunID:      s.run.ID,
		StepNumber: s.stepNumber,
		ActionType: store.ActionFinish,
		Action:     actionJSON(ToolFinish, args),
		Status:     store.StepOK,
		Duration:   time.Since(started),
		Evidence:   a.resolveEvidence(s, final.Evidence),
	}
	if outcome, failed, err := a.report(ctx, s, step, record{actionType: store.ActionFinish}); failed {
		return outcome, err
	}
	return s.outcome(store.RunSucceeded, stopReason, "", args), nil
}

// forcedFinishFailed records the forced call that produced nothing usable and
// ends the run.
//
// It is not retried: a run with no diagnosis is not a success, and a second
// call would be a second bound overrun for the same reason the first one did
// not work.
func (a *Agent) forcedFinishFailed(ctx context.Context, s *runState, started time.Time,
	stopReason, reason string,
) (store.RunOutcome, error) {
	errText := fmt.Sprintf("the run stopped on %s and the forced %s produced no diagnosis: %s",
		stopReason, ToolFinish, reason)

	step := Step{
		RunID:      s.run.ID,
		StepNumber: s.stepNumber,
		ActionType: store.ActionNone,
		Action:     emptyArguments,
		Status:     store.StepError,
		Error:      errText,
		Duration:   time.Since(started),
	}
	if outcome, failed, err := a.report(ctx, s, step, record{actionType: store.ActionNone}); failed {
		return outcome, err
	}
	return s.failed(errText), nil
}

// report hands a step to the callback and remembers the ids it wrote.
//
// failed being true means the run is over: either it was cancelled, in which
// case err carries that and nothing terminal should be written, or the step
// could not be persisted, which means the audit trail is already wrong.
func (a *Agent) report(ctx context.Context, s *runState, step Step, rec record) (store.RunOutcome, bool, error) {
	ref, err := a.deps.Report(ctx, step)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return store.RunOutcome{}, true, ctxErr
		}
		return s.failed(fmt.Sprintf("step %d could not be recorded: %v", step.StepNumber, err)), true, nil
	}

	obs.AgentSteps.WithLabelValues(step.ActionType, step.Status).Inc()

	s.stepCount++
	rec.ref = ref
	s.records[step.StepNumber] = rec
	return store.RunOutcome{}, false, nil
}

// actionJSON renders agent_steps.action, which holds the tool and its
// arguments for every action type that had one.
func actionJSON(name string, args json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(Action{Tool: name, Arguments: args})
	if err != nil {
		// decide has already rejected arguments that are not valid JSON, so
		// this is unreachable; recording the tool alone still beats a column
		// MySQL would refuse.
		raw, _ = json.Marshal(Action{Tool: name})
	}
	return raw
}
