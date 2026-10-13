package agentfactory

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const mcpSetupTimeout = 10 * time.Second
const mcpToolTimeout = 120 * time.Second

// One slot retains the SDK cancellation tail. Waiting callers never allocate
// another worker or wait on ADK's non-cancellable setup mutex.
type boundedMCPToolset struct {
	tool.Toolset
	setupTimeout time.Duration
	toolTimeout  time.Duration
	gate         chan struct{}
	stopping     chan struct{}
	mu           sync.Mutex
	closed       bool
	pendingDone  chan struct{}
	cancel       context.CancelFunc
}

type mcpOperationResult struct {
	tools  []tool.Tool
	output map[string]any
	err    error
}

func newBoundedMCPToolset(ts tool.Toolset, setup, run time.Duration) *boundedMCPToolset {
	return &boundedMCPToolset{Toolset: ts, setupTimeout: setup, toolTimeout: run, gate: make(chan struct{}, 1), stopping: make(chan struct{})}
}

func (s *boundedMCPToolset) operation(ctx context.Context, budget time.Duration, run func(context.Context) mcpOperationResult) mcpOperationResult {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return mcpOperationResult{err: ctx.Err()}
	case <-s.stopping:
		cancel()
		return mcpOperationResult{err: context.Canceled}
	}
	s.mu.Lock()
	if s.closed || ctx.Err() != nil {
		s.mu.Unlock()
		<-s.gate
		cancel()
		if ctx.Err() != nil {
			return mcpOperationResult{err: ctx.Err()}
		}
		return mcpOperationResult{err: context.Canceled}
	}
	done := make(chan struct{})
	s.pendingDone = done
	s.cancel = cancel
	s.mu.Unlock()
	result := make(chan mcpOperationResult, 1)
	go func() {
		defer func() { <-s.gate; close(done) }()
		result <- run(ctx)
	}()
	select {
	case out := <-result:
		if ctx.Err() != nil {
			return mcpOperationResult{err: ctx.Err()}
		}
		return out
	case <-ctx.Done():
		return mcpOperationResult{err: ctx.Err()}
	}
}

func (s *boundedMCPToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	out := s.operation(ctx, s.setupTimeout, func(deadline context.Context) mcpOperationResult {
		tools, err := s.Toolset.Tools(mcpReadonlyContext{ReadonlyContext: ctx, deadline: deadline})
		return mcpOperationResult{tools: tools, err: err}
	})
	if out.err != nil {
		return nil, out.err
	}
	tools := make([]tool.Tool, 0, len(out.tools))
	for _, original := range out.tools {
		function, ok := original.(mcpFunctionTool)
		if !ok {
			return nil, fmt.Errorf("mcp tool %q does not expose ADK function/processor capabilities", original.Name())
		}
		tools = append(tools, &boundedMCPTool{mcpFunctionTool: function, owner: s})
	}
	return tools, nil
}

func (s *boundedMCPToolset) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.stopping)
	}
	if s.cancel != nil {
		s.cancel()
	}
}
func (s *boundedMCPToolset) Close() error {
	s.stop()
	s.mu.Lock()
	done := s.pendingDone
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	timer := time.NewTimer(mcpCloseTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

type mcpFunctionTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

type boundedMCPTool struct {
	mcpFunctionTool
	owner *boundedMCPToolset
}

func (t *boundedMCPTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := t.mcpFunctionTool.ProcessRequest(ctx, req); err != nil {
		return err
	}
	// ADK packs the underlying tool itself. Keep execution on the bounded tool
	// while preserving its declaration and request processing.
	if req.Tools[t.Name()] != nil {
		req.Tools[t.Name()] = t
	}
	return nil
}

func (t *boundedMCPTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	shadow := newMCPToolContext(ctx)
	out := t.owner.operation(ctx, t.owner.toolTimeout, func(deadline context.Context) mcpOperationResult {
		shadow.deadline = deadline
		result, err := t.mcpFunctionTool.Run(shadow, args)
		return mcpOperationResult{output: result, err: err}
	})
	if errors.Is(out.err, context.Canceled) || errors.Is(out.err, context.DeadlineExceeded) {
		return nil, out.err
	}
	// Only the caller commits state/actions, after the worker has completed.
	// A timed-out worker owns isolated values and cannot mutate the invocation.
	if len(shadow.state.delta) > 0 && shadow.actions.StateDelta == nil {
		shadow.actions.StateDelta = make(map[string]any)
	}
	for key, value := range shadow.state.delta {
		shadow.actions.StateDelta[key] = value
		if err := ctx.State().Set(key, value); err != nil {
			return nil, errors.Join(out.err, err)
		}
	}
	if shadow.actions != nil {
		*ctx.Actions() = *shadow.actions
	}
	return out.output, out.err
}

type mcpReadonlyContext struct {
	agent.ReadonlyContext
	deadline context.Context
}

func (c mcpReadonlyContext) Deadline() (time.Time, bool) { return c.deadline.Deadline() }
func (c mcpReadonlyContext) Done() <-chan struct{}       { return c.deadline.Done() }
func (c mcpReadonlyContext) Err() error                  { return c.deadline.Err() }
func (c mcpReadonlyContext) Value(key any) any           { return c.deadline.Value(key) }

type mcpToolContext struct {
	agent.Context
	deadline context.Context
	actions  *session.EventActions
	state    *mcpOperationState
}

func newMCPToolContext(ctx agent.Context) *mcpToolContext {
	actions := *ctx.Actions()
	actions.StateDelta = maps.Clone(actions.StateDelta)
	actions.ArtifactDelta = maps.Clone(actions.ArtifactDelta)
	actions.RequestedToolConfirmations = maps.Clone(actions.RequestedToolConfirmations)
	values := make(map[string]any)
	for key, value := range ctx.State().All() {
		values[key] = value
	}
	return &mcpToolContext{Context: ctx, actions: &actions, state: &mcpOperationState{values: values, delta: make(map[string]any)}}
}
func (c *mcpToolContext) Deadline() (time.Time, bool)          { return c.deadline.Deadline() }
func (c *mcpToolContext) Done() <-chan struct{}                { return c.deadline.Done() }
func (c *mcpToolContext) Err() error                           { return c.deadline.Err() }
func (c *mcpToolContext) Value(key any) any                    { return c.deadline.Value(key) }
func (c *mcpToolContext) Actions() *session.EventActions       { return c.actions }
func (c *mcpToolContext) State() session.State                 { return c.state }
func (c *mcpToolContext) ReadonlyState() session.ReadonlyState { return c.state }
func (c *mcpToolContext) RequestConfirmation(hint string, payload any) error {
	// Reuse ADK's policy against isolated actions instead of mutating the live
	// context from a worker that may outlast its caller.
	isolated := agent.NewToolContext(c, c.FunctionCallID(), c.actions, c.ToolConfirmation())
	return isolated.RequestConfirmation(hint, payload)
}

type mcpOperationState struct{ values, delta map[string]any }

func (s *mcpOperationState) Get(key string) (any, error) {
	value, ok := s.values[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return value, nil
}
func (s *mcpOperationState) Set(key string, value any) error {
	s.values[key] = value
	s.delta[key] = value
	return nil
}
func (s *mcpOperationState) All() iter.Seq2[string, any] { return maps.All(s.values) }
