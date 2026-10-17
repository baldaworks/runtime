package poolagent

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type scriptedMember struct {
	agent.Agent
	name      string
	errorCode string
	runs      int
}

func (m *scriptedMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		state := &session.Event{}
		state.Actions.StateDelta = map[string]any{"acp_session": m.name}
		if !yield(state, nil) {
			return
		}
		ev := &session.Event{}
		if m.errorCode != "" {
			ev.LLMResponse = model.LLMResponse{ErrorCode: m.errorCode, ErrorMessage: m.name + " failed"}
		}
		yield(ev, nil)
	}
}

type namedCreator struct {
	members map[string]*scriptedMember
}

func (c *namedCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	return c.members[name], nil
}

func newScriptedPool(codex, glm *scriptedMember) (*PoolAgent, *PoolExecutor, *time.Time) {
	creator := &namedCreator{members: map[string]*scriptedMember{"codex": codex, "glm": glm}}
	members := []MemberConfig{{Name: "codex"}, {Name: "glm"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	clock := time.Now()
	executor.now = func() time.Time { return clock }
	return &PoolAgent{executor: executor}, executor, &clock
}

func runPool(t *testing.T, pool *PoolAgent) []*session.Event {
	t.Helper()
	var events []*session.Event
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return events
}

func TestPoolFailsOverOnProviderErrorEvent(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: "usageLimitExceeded"}
	glm := &scriptedMember{name: "glm"}
	pool, _, _ := newScriptedPool(codex, glm)

	events := runPool(t, pool)
	for _, ev := range events {
		if ev.ErrorCode != "" || ev.Actions.StateDelta["acp_session"] == "codex" {
			t.Fatalf("failed member leaked an event: %+v", ev)
		}
	}
	if len(events) != 2 {
		t.Fatalf("want glm's two events, got %d", len(events))
	}
	if codex.runs != 1 || glm.runs != 1 {
		t.Fatalf("runs codex=%d glm=%d", codex.runs, glm.runs)
	}
}

func TestPoolSkipsCoolingMemberThenReturnsToPrimary(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: "usageLimitExceeded"}
	glm := &scriptedMember{name: "glm"}
	pool, _, clock := newScriptedPool(codex, glm)

	runPool(t, pool)
	runPool(t, pool)
	if codex.runs != 1 || glm.runs != 2 {
		t.Fatalf("codex must be skipped while cooling: codex=%d glm=%d", codex.runs, glm.runs)
	}

	*clock = clock.Add(memberCooldown + time.Second)
	codex.errorCode = ""
	runPool(t, pool)
	if codex.runs != 2 {
		t.Fatalf("pool must return to codex after cooldown: codex=%d", codex.runs)
	}
}

func TestPoolSurfacesErrorWhenEveryMemberFails(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: "usageLimitExceeded"}
	glm := &scriptedMember{name: "glm", errorCode: "rateLimited"}
	pool, _, _ := newScriptedPool(codex, glm)

	events := runPool(t, pool)
	last := events[len(events)-1]
	if len(events) != 1 || last.ErrorCode != "rateLimited" {
		t.Fatalf("want only last member's error event, got %+v", events)
	}
}

type erroringMember struct {
	agent.Agent
	runs      int
	afterText bool
}

func (m *erroringMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		if m.afterText {
			ev := &session.Event{}
			ev.Content = genai.NewContentFromText("partial", genai.RoleModel)
			if !yield(ev, nil) {
				return
			}
		}
		yield(nil, errors.New("boom"))
	}
}

func newErroringPool(first *erroringMember, second *scriptedMember) (*PoolAgent, *PoolExecutor) {
	creator := &mixedCreator{first: first, second: second}
	members := []MemberConfig{{Name: "first"}, {Name: "second"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	return &PoolAgent{executor: executor}, executor
}

type mixedCreator struct {
	first  *erroringMember
	second *scriptedMember
}

func (c *mixedCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	if name == "first" {
		return c.first, nil
	}
	return c.second, nil
}

func TestPoolDoesNotRerunTurnAfterOutput(t *testing.T) {
	first := &erroringMember{afterText: true}
	second := &scriptedMember{name: "second"}
	pool, _ := newErroringPool(first, second)

	var events int
	var gotErr error
	for _, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			gotErr = err
			continue
		}
		events++
	}
	if gotErr == nil || events != 1 {
		t.Fatalf("want one event then the error, got events=%d err=%v", events, gotErr)
	}
	if second.runs != 0 {
		t.Fatalf("turn was rerun after output reached the consumer: runs=%d", second.runs)
	}
}

func TestPoolCanceledRunDoesNotPenalizeMember(t *testing.T) {
	first := &erroringMember{}
	second := &scriptedMember{name: "second"}
	pool, executor := newErroringPool(first, second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range pool.run(retryContext{Context: ctx}) {
	}
	if executor.coolingDown("first") {
		t.Fatal("a canceled invocation put the member into cooldown")
	}
	if second.runs != 0 {
		t.Fatalf("canceled invocation failed over: runs=%d", second.runs)
	}
}

func TestPoolErrorBeforeOutputFailsOver(t *testing.T) {
	first := &erroringMember{}
	second := &scriptedMember{name: "second"}
	pool, executor := newErroringPool(first, second)

	runPool(t, pool)
	if first.runs != 1 || second.runs != 1 {
		t.Fatalf("runs first=%d second=%d", first.runs, second.runs)
	}
	if !executor.coolingDown("first") {
		t.Fatal("failed member must be cooling down")
	}
}

type countingCreator struct {
	failing map[string]bool
	created map[string]int
	members map[string]*scriptedMember
}

func (c *countingCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	c.created[name]++
	if c.failing[name] {
		return nil, errors.New("cannot start")
	}
	return c.members[name], nil
}

func newCountingPool(failing ...string) (*PoolAgent, *PoolExecutor, *countingCreator, *time.Time) {
	creator := &countingCreator{
		failing: map[string]bool{},
		created: map[string]int{},
		members: map[string]*scriptedMember{
			"primary":  {name: "primary"},
			"fallback": {name: "fallback"},
		},
	}
	for _, name := range failing {
		creator.failing[name] = true
	}
	members := []MemberConfig{{Name: "primary"}, {Name: "fallback"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	clock := time.Now()
	executor.now = func() time.Time { return clock }
	return &PoolAgent{executor: executor}, executor, creator, &clock
}

func TestPoolCreationFailureHonorsCooldown(t *testing.T) {
	pool, _, creator, clock := newCountingPool("primary")

	runPool(t, pool)
	runPool(t, pool)
	if creator.created["primary"] != 1 || creator.created["fallback"] != 1 {
		t.Fatalf("members must not be recreated every turn: %v", creator.created)
	}

	*clock = clock.Add(memberCooldown + time.Second)
	runPool(t, pool)
	if creator.created["primary"] != 2 {
		t.Fatalf("primary must be probed once its cooldown ended: %v", creator.created)
	}
}

func TestPoolDoesNotProbeCoolingMembers(t *testing.T) {
	pool, executor, creator, _ := newCountingPool()
	executor.startCooldown("primary")
	executor.startCooldown("fallback")

	var gotErr error
	for _, err := range pool.run(retryContext{Context: context.Background()}) {
		gotErr = err
	}
	var all *AllPoolMembersFailedError
	if !errors.As(gotErr, &all) {
		t.Fatalf("want AllPoolMembersFailedError, got %v", gotErr)
	}
	if len(creator.created) != 0 {
		t.Fatalf("cooling members were probed: %v", creator.created)
	}
}

func TestSingleMemberPoolNeverCoolsDown(t *testing.T) {
	creator := &countingCreator{
		failing: map[string]bool{},
		created: map[string]int{},
		members: map[string]*scriptedMember{"only": {name: "only"}},
	}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "only"}}, creator, AgentRequest{})
	executor.startCooldown("only")
	if executor.coolingDown("only") {
		t.Fatal("a pool without alternatives must not cool its only member down")
	}
}

func TestPoolErrorAfterOutputIsTerminal(t *testing.T) {
	first := &erroringMember{afterText: true}
	second := &scriptedMember{name: "second"}
	pool, _ := newErroringPool(first, second)

	var events int
	var errs []error
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev.Content != nil {
			events++
		}
	}
	if events != 1 || len(errs) != 1 || errs[0].Error() != "boom" {
		t.Fatalf("want one event then the original error, got events=%d errs=%v", events, errs)
	}
}

type partialThenErrorMember struct {
	agent.Agent
	runs int
}

func (m *partialThenErrorMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		text := &session.Event{}
		text.Content = genai.NewContentFromText("partial", genai.RoleModel)
		if !yield(text, nil) {
			return
		}
		failed := &session.Event{}
		failed.LLMResponse = model.LLMResponse{ErrorCode: "usageLimitExceeded", ErrorMessage: "limit"}
		yield(failed, nil)
	}
}

type partialCreator struct {
	first  *partialThenErrorMember
	second *scriptedMember
}

func (c *partialCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	if name == "first" {
		return c.first, nil
	}
	return c.second, nil
}

func TestPoolProviderErrorAfterOutputStartsCooldownWithoutRerun(t *testing.T) {
	first := &partialThenErrorMember{}
	second := &scriptedMember{name: "second"}
	creator := &partialCreator{first: first, second: second}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "first"}, {Name: "second"}}, creator, AgentRequest{})
	pool := &PoolAgent{executor: executor}

	var events []*session.Event
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	if len(events) != 2 || events[1].ErrorCode != "usageLimitExceeded" {
		t.Fatalf("want the partial text and then the provider error, got %d events", len(events))
	}
	if second.runs != 0 {
		t.Fatalf("turn was rerun on the fallback after output: runs=%d", second.runs)
	}
	if !executor.coolingDown("first") {
		t.Fatal("a provider that failed after output must start a cooldown")
	}

	runPool(t, pool)
	if first.runs != 1 || second.runs != 1 {
		t.Fatalf("next turn must go to the fallback: first=%d second=%d", first.runs, second.runs)
	}
}
